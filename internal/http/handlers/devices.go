package handlers

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/google/uuid"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posdevice"
	"github.com/bengobox/pos-service/internal/ent/posdevicesession"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/tender"
	"github.com/bengobox/pos-service/internal/platform/events"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
)

// errLimitWritten signals that a 402 limit-reached response was already written to the
// ResponseWriter, so the caller must return without writing again.
var errLimitWritten = errors.New("subscription limit response written")

// DeviceHandler handles device session (shift) endpoints.
type DeviceHandler struct {
	log    *zap.Logger
	client *ent.Client
	pub    *events.Publisher
}

func NewDeviceHandler(log *zap.Logger, client *ent.Client) *DeviceHandler {
	return &DeviceHandler{log: log, client: client}
}

// SetPublisher injects the event publisher for usage tracking events.
func (h *DeviceHandler) SetPublisher(pub *events.Publisher) {
	h.pub = pub
}

// ListDevices handles GET /{tenantID}/pos/devices
// Returns all registered terminals for the tenant with outlet info.
func (h *DeviceHandler) ListDevices(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	p := pagination.Parse(r)
	baseQ := h.client.POSDevice.Query().Where(posdevice.TenantID(tid))
	total, _ := baseQ.Clone().Count(r.Context())
	devices, err := baseQ.WithOutlet().Order(ent.Desc(posdevice.FieldRegisteredAt)).Limit(p.Limit).Offset(p.Offset).All(r.Context())
	if err != nil {
		h.log.Error("list devices failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	type deviceResp struct {
		ID           string  `json:"id"`
		DeviceCode   string  `json:"device_code"`
		DeviceType   string  `json:"device_type"`
		Status       string  `json:"status"`
		OutletName   string  `json:"outlet_name,omitempty"`
		LastSeenAt   *string `json:"last_seen_at,omitempty"`
		RegisteredAt string  `json:"registered_at"`
	}

	result := make([]deviceResp, 0, len(devices))
	for _, d := range devices {
		dr := deviceResp{
			ID:           d.ID.String(),
			DeviceCode:   d.DeviceCode,
			DeviceType:   d.DeviceType,
			Status:       d.Status,
			RegisteredAt: d.RegisteredAt.Format(time.RFC3339),
		}
		if d.LastSeenAt != nil {
			s := d.LastSeenAt.Format(time.RFC3339)
			dr.LastSeenAt = &s
		}
		if d.Edges.Outlet != nil {
			dr.OutletName = d.Edges.Outlet.Name
		}
		result = append(result, dr)
	}

	jsonOK(w, pagination.NewResponse(result, total, p))
}

// GetCurrentSession handles GET /{tenantID}/pos/devices/current/sessions/current
// Returns the open session for the currently authenticated user, or 404.
func (h *DeviceHandler) GetCurrentSession(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userID := currentUserID(r)

	session, err := h.client.POSDeviceSession.Query().
		Where(
			posdevicesession.TenantID(tid),
			posdevicesession.UserID(userID),
			posdevicesession.SessionStatus("open"),
		).
		Order(ent.Desc(posdevicesession.FieldOpenedAt)).
		First(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		jsonOK(w, nil)
		return
	}

	jsonOK(w, session)
}

type openSessionInput struct {
	OpeningFloat float64        `json:"opening_float"`
	DeviceID     *uuid.UUID     `json:"device_id,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// OpenSession handles POST /{tenantID}/pos/devices/current/sessions/open
func (h *DeviceHandler) OpenSession(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userID := currentUserID(r)

	var input openSessionInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	// Resolve the device_id — must be a real POSDevice FK or session creation fails.
	deviceID, err := h.resolveOrCreateDevice(w, r, tid, input.DeviceID)
	if err != nil {
		if errors.Is(err, errLimitWritten) {
			return // 402 device_limit_reached already written
		}
		h.log.Error("could not resolve device", zap.Error(err))
		jsonError(w, "failed to resolve device", http.StatusInternalServerError)
		return
	}

	meta := input.Metadata
	if meta == nil {
		meta = map[string]any{}
	}

	session, err := h.client.POSDeviceSession.Create().
		SetTenantID(tid).
		SetDeviceID(deviceID).
		SetUserID(userID).
		SetSessionStatus("open").
		SetFloatAmount(input.OpeningFloat).
		SetOpenedAt(time.Now()).
		SetMetadata(meta).
		Save(r.Context())
	if err != nil {
		h.log.Error("open session failed", zap.Error(err))
		jsonError(w, "failed to open session", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	jsonOK(w, session)
}

// resolveOrCreateDevice finds an existing POSDevice for the outlet (from JWT claims or
// input.DeviceID), or creates a web_terminal device on first use.
// The POSDeviceSession schema requires a valid device_id FK.
func (h *DeviceHandler) resolveOrCreateDevice(w http.ResponseWriter, r *http.Request, tid uuid.UUID, inputDeviceID *uuid.UUID) (uuid.UUID, error) {
	ctx := r.Context()

	// Caller-supplied device_id takes precedence when it's a real FK.
	if inputDeviceID != nil {
		exists, _ := h.client.POSDevice.Query().
			Where(posdevice.ID(*inputDeviceID), posdevice.TenantID(tid)).
			Exist(ctx)
		if exists {
			return *inputDeviceID, nil
		}
	}

	// Resolve outlet from JWT claims (terminal sessions have OutletID in claims).
	var outletID uuid.UUID
	if claims, ok := authclient.ClaimsFromContext(ctx); ok && claims.OutletID != "" {
		if id, err := uuid.Parse(claims.OutletID); err == nil {
			outletID = id
		}
	}
	if outletID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("outlet_id not available in claims")
	}

	// Look for an existing web_terminal device for this outlet.
	device, err := h.client.POSDevice.Query().
		Where(posdevice.TenantID(tid), posdevice.OutletID(outletID), posdevice.DeviceType("web_terminal")).
		First(ctx)
	if err == nil {
		return device.ID, nil
	}
	if !ent.IsNotFound(err) {
		return uuid.Nil, err
	}

	// Create a web_terminal device for this outlet on first use.
	// Enforce the plan's max_devices structural cap before provisioning a NEW device
	// (existing-device reuse above is never blocked). Hard-block, no overage.
	if count, cerr := h.client.POSDevice.Query().Where(posdevice.TenantID(tid)).Count(ctx); cerr == nil {
		if !subscriptions.CheckDeviceLimit(w, r, count) {
			return uuid.Nil, errLimitWritten
		}
	}

	device, err = h.client.POSDevice.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetDeviceCode(fmt.Sprintf("WEB-%s", outletID.String()[:8])).
		SetDeviceType("web_terminal").
		SetStatus("active").
		Save(ctx)
	if err != nil {
		return uuid.Nil, err
	}

	if h.pub != nil {
		_ = h.pub.PublishDeviceRegistered(ctx, tid, map[string]any{
			"device_id":   device.ID.String(),
			"outlet_id":   outletID.String(),
			"device_type": device.DeviceType,
		})
	}
	return device.ID, nil
}

type closeSessionInput struct {
	ClosingFloat float64        `json:"closing_float"`
	Notes        string         `json:"notes,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// CloseSession handles POST /{tenantID}/pos/devices/current/sessions/close
// Saves the closing float, calculates expected cash, and records variance.
func (h *DeviceHandler) CloseSession(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userID := currentUserID(r)

	var input closeSessionInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		jsonError(w, "invalid request body", http.StatusBadRequest)
		return
	}

	session, err := h.client.POSDeviceSession.Query().
		Where(
			posdevicesession.TenantID(tid),
			posdevicesession.UserID(userID),
			posdevicesession.SessionStatus("open"),
		).
		Order(ent.Desc(posdevicesession.FieldOpenedAt)).
		First(r.Context())
	if err != nil {
		jsonError(w, "no open session found", http.StatusNotFound)
		return
	}

	// Compute expected cash: opening float + total cash sales during this session.
	expectedCash, err := h.computeExpectedCash(r, tid, session)
	if err != nil {
		h.log.Warn("CloseSession: could not compute expected cash", zap.Error(err))
		expectedCash = session.FloatAmount
	}

	variance := input.ClosingFloat - expectedCash

	now := time.Now()
	update := h.client.POSDeviceSession.UpdateOne(session).
		SetSessionStatus("closed").
		SetClosedAt(now).
		SetClosingFloat(input.ClosingFloat).
		SetVariance(variance)

	if input.Notes != "" {
		update = update.SetNotes(input.Notes)
	}

	updated, err := update.Save(r.Context())
	if err != nil {
		h.log.Error("close session failed", zap.Error(err))
		jsonError(w, "failed to close session", http.StatusInternalServerError)
		return
	}

	jsonOK(w, map[string]any{
		"session":       updated,
		"expected_cash": expectedCash,
		"variance":      variance,
	})
}

// computeExpectedCash calculates opening_float + total completed cash-tender payments
// for orders during this session window on this device.
func (h *DeviceHandler) computeExpectedCash(r *http.Request, tid uuid.UUID, session *ent.POSDeviceSession) (float64, error) {
	ctx := r.Context()

	// Get all completed orders for this device since session opened.
	orders, err := h.client.POSOrder.Query().
		Where(
			posorder.TenantID(tid),
			posorder.DeviceID(session.DeviceID),
			posorder.StatusEQ("completed"),
			posorder.CreatedAtGTE(session.OpenedAt),
		).
		WithPayments().
		All(ctx)
	if err != nil {
		return 0, err
	}

	// Load cash tenders for the tenant.
	cashTenders, err := h.client.Tender.Query().
		Where(tender.TenantID(tid), tender.TypeEQ("cash")).
		All(ctx)
	if err != nil {
		return 0, err
	}
	cashTenderIDs := make(map[uuid.UUID]bool, len(cashTenders))
	for _, t := range cashTenders {
		cashTenderIDs[t.ID] = true
	}

	cashTotal := session.FloatAmount
	for _, o := range orders {
		for _, p := range o.Edges.Payments {
			if p.Status == "completed" && cashTenderIDs[p.TenderID] {
				cashTotal += p.Amount
			}
		}
	}
	return cashTotal, nil
}

// SessionSummaryResponse is the full live summary for an open shift.
type SessionSummaryResponse struct {
	SessionID       string            `json:"session_id"`
	OpenedAt        time.Time         `json:"opened_at"`
	OpeningFloat    float64           `json:"opening_float"`
	OrderCount      int               `json:"order_count"`
	TotalRevenue    float64           `json:"total_revenue"`
	ExpectedCash    float64           `json:"expected_cash"`
	RefundCount     int               `json:"refund_count"`
	TotalRefunds    float64           `json:"total_refunds"`
	VoidCount       int               `json:"void_count"`
	TenderBreakdown []TenderBreakdown `json:"tender_breakdown"`
	// Flattened convenience fields for frontend components.
	CashInTotal float64 `json:"cash_in_total"`
	CardTotal   float64 `json:"card_total"`
	MpesaTotal  float64 `json:"mpesa_total"`
}

// TenderBreakdown is the revenue split by payment method.
type TenderBreakdown struct {
	TenderName string  `json:"tender_name"`
	TenderType string  `json:"tender_type"`
	Amount     float64 `json:"amount"`
	Count      int     `json:"count"`
}

// GetSessionSummary handles GET /{tenantID}/pos/devices/current/sessions/current/summary
// Returns aggregated sales stats for the active shift with payment method breakdown.
func (h *DeviceHandler) GetSessionSummary(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userID := currentUserID(r)

	session, err := h.client.POSDeviceSession.Query().
		Where(
			posdevicesession.TenantID(tid),
			posdevicesession.UserID(userID),
			posdevicesession.SessionStatus("open"),
		).
		Order(ent.Desc(posdevicesession.FieldOpenedAt)).
		First(r.Context())
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		jsonOK(w, nil)
		return
	}

	// Query completed orders with their payments.
	orders, err := h.client.POSOrder.Query().
		Where(
			posorder.TenantID(tid),
			posorder.DeviceID(session.DeviceID),
			posorder.CreatedAtGTE(session.OpenedAt),
		).
		WithPayments().
		All(r.Context())
	if err != nil {
		h.log.Error("session summary: order query failed", zap.Error(err))
		jsonError(w, "failed to compute session summary", http.StatusInternalServerError)
		return
	}

	// Load all tenders for tender type lookup.
	tenders, err := h.client.Tender.Query().
		Where(tender.TenantID(tid)).
		All(r.Context())
	if err != nil {
		h.log.Warn("session summary: tender query failed", zap.Error(err))
	}
	tenderMap := make(map[uuid.UUID]*ent.Tender, len(tenders))
	for _, t := range tenders {
		tenderMap[t.ID] = t
	}

	var (
		totalRevenue float64
		voidCount    int
		orderCount   int
	)
	// map: tender_id → {name, type, amount, count}
	type tenderAccum struct {
		name   string
		tType  string
		amount float64
		count  int
	}
	tenderAccums := make(map[uuid.UUID]*tenderAccum)

	for _, o := range orders {
		switch o.Status {
		case "completed":
			orderCount++
			totalRevenue += o.TotalAmount
		case "voided":
			voidCount++
		}
		for _, p := range o.Edges.Payments {
			if p.Status != "completed" {
				continue
			}
			acc := tenderAccums[p.TenderID]
			if acc == nil {
				name, tType := p.TenderID.String()[:8], "other"
				if t, ok := tenderMap[p.TenderID]; ok {
					name = t.Name
					tType = t.Type
				}
				acc = &tenderAccum{name: name, tType: tType}
				tenderAccums[p.TenderID] = acc
			}
			acc.amount += p.Amount
			acc.count++
		}
	}

	// Compute expected cash = float + all cash-tender payments.
	cashExpected := session.FloatAmount
	for tID2, acc := range tenderAccums {
		if t, ok := tenderMap[tID2]; ok && t.Type == "cash" {
			cashExpected += acc.amount
		}
	}

	breakdown := make([]TenderBreakdown, 0, len(tenderAccums))
	var cashInTotal, cardTotal, mpesaTotal float64
	for tID, acc := range tenderAccums {
		breakdown = append(breakdown, TenderBreakdown{
			TenderName: acc.name,
			TenderType: acc.tType,
			Amount:     acc.amount,
			Count:      acc.count,
		})
		if t, ok := tenderMap[tID]; ok {
			switch t.Type {
			case "cash":
				cashInTotal += acc.amount
			case "card":
				cardTotal += acc.amount
			case "mpesa", "mobile_money":
				mpesaTotal += acc.amount
			}
		}
	}

	jsonOK(w, SessionSummaryResponse{
		SessionID:       session.ID.String(),
		OpenedAt:        session.OpenedAt,
		OpeningFloat:    session.FloatAmount,
		OrderCount:      orderCount,
		TotalRevenue:    totalRevenue,
		ExpectedCash:    cashExpected,
		VoidCount:       voidCount,
		TenderBreakdown: breakdown,
		CashInTotal:     cashInTotal,
		CardTotal:       cardTotal,
		MpesaTotal:      mpesaTotal,
	})
}

// GetSessionHistory handles GET /{tenantID}/pos/devices/current/sessions/history
// Returns the last N closed sessions for the currently authenticated user.
func (h *DeviceHandler) GetSessionHistory(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}

	userID := currentUserID(r)

	sessions, err := h.client.POSDeviceSession.Query().
		Where(
			posdevicesession.TenantID(tid),
			posdevicesession.UserID(userID),
		).
		Order(ent.Desc(posdevicesession.FieldOpenedAt)).
		Limit(20).
		All(r.Context())
	if err != nil {
		h.log.Error("session history query failed", zap.Error(err))
		jsonError(w, "internal error", http.StatusInternalServerError)
		return
	}

	// For each closed session, attach quick stats from orders.
	type sessionRow struct {
		ID           string   `json:"id"`
		Status       string   `json:"status"`
		OpenedAt     string   `json:"opened_at"`
		ClosedAt     *string  `json:"closed_at,omitempty"`
		OpeningFloat float64  `json:"opening_float"`
		ClosingFloat *float64 `json:"closing_float,omitempty"`
		Variance     *float64 `json:"variance,omitempty"`
		Notes        string   `json:"notes,omitempty"`
		OrderCount   int      `json:"order_count"`
		TotalRevenue float64  `json:"total_revenue"`
	}

	rows := make([]sessionRow, 0, len(sessions))
	for _, s := range sessions {
		orderQ := h.client.POSOrder.Query().
			Where(
				posorder.TenantID(tid),
				posorder.StatusEQ("completed"),
				posorder.CreatedAtGTE(s.OpenedAt),
			)
		// For closed sessions, bound the upper time so orders from the next shift aren't counted.
		if s.ClosedAt != nil {
			orderQ = orderQ.Where(posorder.CreatedAtLTE(*s.ClosedAt))
		}
		orders, _ := orderQ.All(r.Context())

		var rev float64
		for _, o := range orders {
			rev += o.TotalAmount
		}

		row := sessionRow{
			ID:           s.ID.String(),
			Status:       s.SessionStatus,
			OpenedAt:     s.OpenedAt.Format(time.RFC3339),
			OpeningFloat: s.FloatAmount,
			Notes:        s.Notes,
			OrderCount:   len(orders),
			TotalRevenue: rev,
		}
		if s.ClosedAt != nil {
			ts := s.ClosedAt.Format(time.RFC3339)
			row.ClosedAt = &ts
		}
		if s.ClosingFloat != nil {
			row.ClosingFloat = s.ClosingFloat
		}
		if s.Variance != nil {
			row.Variance = s.Variance
		}
		rows = append(rows, row)
	}

	jsonOK(w, map[string]any{"data": rows, "total": len(rows)})
}

// currentUserID extracts the user UUID from JWT claims; falls back to nil UUID.
func currentUserID(r *http.Request) uuid.UUID {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	if !ok || claims.Subject == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// tenderTypeEQ is a helper used by computeExpectedCash to match cash tenders.
func tenderTypeEQ(v string) func(*ent.TenderMutation) {
	return func(m *ent.TenderMutation) {
		m.SetType(v)
	}
}