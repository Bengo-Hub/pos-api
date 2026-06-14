package handlers

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/cashdrawer"
	"github.com/bengobox/pos-service/internal/ent/cashdrawerevent"
)

// validMovementTypes are the cash-drawer movement types subject to logging.
// no_sale       — drawer opened without a sale (must carry a reason)
// pay_in        — cash added to the drawer (e.g. change float top-up)
// pay_out       — cash removed for an expense (a prime skimming vector — always logged)
// cash_drop     — mid-shift drop to the safe (reduces drawer exposure)
var validMovementTypes = map[string]bool{
	"no_sale": true, "pay_in": true, "pay_out": true, "cash_drop": true,
}

type drawerMovementInput struct {
	Type   string  `json:"type"`
	Amount float64 `json:"amount"`
	Reason string  `json:"reason"`
}

// RecordMovement handles POST /{tenantID}/pos/drawers/{id}/movement.
// Records a typed cash-drawer event (no_sale / pay_in / pay_out / cash_drop)
// with a mandatory reason and writes it to the centralized audit trail. These
// movements feed the close-of-day expected-cash reconciliation and the
// exception report (unlogged pay-outs are a primary skimming gap).
func (h *DrawerHandler) RecordMovement(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	drawerID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid drawer id", http.StatusBadRequest)
		return
	}
	var input drawerMovementInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || !validMovementTypes[input.Type] {
		jsonError(w, "type must be one of no_sale, pay_in, pay_out, cash_drop", http.StatusBadRequest)
		return
	}
	if input.Reason == "" {
		jsonError(w, "reason is required", http.StatusBadRequest)
		return
	}
	if input.Type != "no_sale" && input.Amount <= 0 {
		jsonError(w, "amount must be greater than zero", http.StatusBadRequest)
		return
	}

	dr, err := h.client.CashDrawer.Query().
		Where(cashdrawer.ID(drawerID), cashdrawer.TenantID(tid)).
		Only(r.Context())
	if err != nil {
		jsonError(w, "drawer not found", http.StatusNotFound)
		return
	}

	ev, err := h.client.CashDrawerEvent.Create().
		SetDrawerID(drawerID).
		SetEventType(input.Type).
		SetAmount(input.Amount).
		SetReason(input.Reason).
		Save(r.Context())
	if err != nil {
		h.log.Error("record drawer movement failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	if h.auditSvc != nil {
		var actor uuid.UUID
		if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
			actor, _ = uuid.Parse(claims.Subject)
		}
		oid := dr.OutletID
		amt := input.Amount
		h.auditSvc.Record(r.Context(), audit.Entry{
			TenantID:    tid,
			OutletID:    &oid,
			ActorUserID: actor,
			Action:      "drawer." + input.Type,
			EntityType:  "cash_drawer",
			EntityID:    drawerID.String(),
			Reason:      input.Reason,
			Amount:      &amt,
		})
	}

	jsonOK(w, ev)
}

// drawerEventDTO is one cash-drawer event in list responses.
type drawerEventDTO struct {
	ID        uuid.UUID `json:"id"`
	EventType string    `json:"event_type"`
	Amount    float64   `json:"amount"`
	Reason    string    `json:"reason,omitempty"`
	OccurredAt time.Time `json:"occurred_at"`
}

// ListDrawerEvents handles GET /{tenantID}/pos/drawers/{id}/events.
func (h *DrawerHandler) ListDrawerEvents(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	drawerID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		jsonError(w, "invalid drawer id", http.StatusBadRequest)
		return
	}
	// Confirm the drawer belongs to the tenant before listing its events.
	if ok, _ := h.client.CashDrawer.Query().Where(cashdrawer.ID(drawerID), cashdrawer.TenantID(tid)).Exist(r.Context()); !ok {
		jsonError(w, "drawer not found", http.StatusNotFound)
		return
	}
	rows, err := h.client.CashDrawerEvent.Query().
		Where(cashdrawerevent.DrawerID(drawerID)).
		Order(ent.Asc(cashdrawerevent.FieldOccurredAt)).
		All(r.Context())
	if err != nil {
		jsonError(w, "failed to load events", http.StatusInternalServerError)
		return
	}
	out := make([]drawerEventDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, drawerEventDTO{ID: e.ID, EventType: e.EventType, Amount: e.Amount, Reason: e.Reason, OccurredAt: e.OccurredAt})
	}
	jsonOK(w, map[string]any{"data": out})
}
