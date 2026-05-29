package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	entorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entpayment "github.com/bengobox/pos-service/internal/ent/pospayment"
)

// StreamPaymentStatus handles GET /{tenantID}/pos/orders/{orderID}/payment-status/stream
//
// Opens an SSE stream that fires a single event when the order has a completed
// payment record, or after a 90s timeout. Replaces polling-based M-Pesa wait UIs.
//
// Events:
//
//	data: {"status":"paid","order_id":"<id>"}    — payment confirmed
//	data: {"status":"timeout","order_id":"<id>"} — no payment within 90s
//
// The chi 30s request-context timeout will fire before the 90s stream timeout on
// slow M-Pesa responses. The browser EventSource auto-reconnects (~3s gap); on
// reconnect the handler immediately finds the already-completed payment in the DB.
func (h *PaymentHandler) StreamPaymentStatus(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	orderID, err := uuid.Parse(chi.URLParam(r, "orderID"))
	if err != nil {
		jsonError(w, "invalid order_id", http.StatusBadRequest)
		return
	}

	// Verify order belongs to this tenant before streaming.
	orderExists, err := h.client.POSOrder.Query().
		Where(entorder.ID(orderID), entorder.TenantID(tid)).
		Exist(r.Context())
	if err != nil || !orderExists {
		jsonError(w, "order not found", http.StatusNotFound)
		return
	}

	// Detach from chi's 30s request-context timeout.
	// The goroutine below terminates bgCtx the moment the HTTP client disconnects.
	bgCtx, bgCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer bgCancel()
	go func() {
		<-r.Context().Done()
		bgCancel()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}

	send := func(status string) {
		data, _ := json.Marshal(map[string]string{"status": status, "order_id": orderID.String()})
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	// Initial keepalive so the browser EventSource doesn't immediately give up.
	fmt.Fprintf(w, ": ping\n\n")
	flusher.Flush()

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-bgCtx.Done():
			send("timeout")
			return
		case <-ticker.C:
			exists, pollErr := h.client.POSPayment.Query().
				Where(entpayment.OrderID(orderID), entpayment.Status("completed")).
				Exist(bgCtx)
			if pollErr != nil {
				continue
			}
			if exists {
				send("paid")
				return
			}
		}
	}
}
