package handlers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entauditlog "github.com/bengobox/pos-service/internal/ent/auditlog"
)

// auditLogDTO is one audit entry in list responses.
type auditLogDTO struct {
	ID         uuid.UUID      `json:"id"`
	OutletID   *uuid.UUID     `json:"outlet_id,omitempty"`
	Actor      uuid.UUID      `json:"actor_user_id"`
	Approver   *uuid.UUID     `json:"approver_user_id,omitempty"`
	Action     string         `json:"action"`
	EntityType string         `json:"entity_type,omitempty"`
	EntityID   string         `json:"entity_id,omitempty"`
	Reason     string         `json:"reason,omitempty"`
	Before     map[string]any `json:"before_json,omitempty"`
	After      map[string]any `json:"after_json,omitempty"`
	Amount     *float64       `json:"amount,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

// ListAuditLogs handles GET /{tenantID}/pos/reports/audit-logs with optional
// filters: action, actor, outlet, from, to, limit, offset. Returns {data,total}.
func (h *ReportsHandler) ListAuditLogs(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.db.AuditLog.Query().Where(entauditlog.TenantID(tid))
	if a := r.URL.Query().Get("action"); a != "" {
		q = q.Where(entauditlog.Action(a))
	}
	if a := r.URL.Query().Get("actor"); a != "" {
		if id, e := uuid.Parse(a); e == nil {
			q = q.Where(entauditlog.ActorUserID(id))
		}
	}
	if o := r.URL.Query().Get("outlet"); o != "" {
		if id, e := uuid.Parse(o); e == nil {
			q = q.Where(entauditlog.OutletID(id))
		}
	}
	if f := r.URL.Query().Get("from"); f != "" {
		if t, e := time.Parse(time.RFC3339, f); e == nil {
			q = q.Where(entauditlog.CreatedAtGTE(t))
		}
	}
	if t := r.URL.Query().Get("to"); t != "" {
		if tt, e := time.Parse(time.RFC3339, t); e == nil {
			q = q.Where(entauditlog.CreatedAtLTE(tt))
		}
	}
	total, _ := q.Clone().Count(r.Context())
	limit := atoiDefault(r.URL.Query().Get("limit"), 50)
	offset := atoiDefault(r.URL.Query().Get("offset"), 0)
	rows, err := q.Order(ent.Desc(entauditlog.FieldCreatedAt)).Limit(limit).Offset(offset).All(r.Context())
	if err != nil {
		jsonError(w, "failed to load audit logs", http.StatusInternalServerError)
		return
	}
	out := make([]auditLogDTO, 0, len(rows))
	for _, a := range rows {
		out = append(out, auditLogDTO{
			ID: a.ID, OutletID: a.OutletID, Actor: a.ActorUserID, Approver: a.ApproverUserID,
			Action: a.Action, EntityType: a.EntityType, EntityID: a.EntityID, Reason: a.Reason,
			Before: a.BeforeJSON, After: a.AfterJSON, Amount: a.Amount, CreatedAt: a.CreatedAt,
		})
	}
	jsonOK(w, map[string]any{"data": out, "total": total})
}

// Exceptions handles GET /{tenantID}/pos/reports/exceptions — counts of
// fraud-relevant actions per actor over a date range (voids, line removals,
// no-sales, pay-outs, cash-drops, overrides, refunds) for cashier review.
func (h *ReportsHandler) Exceptions(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	q := h.db.AuditLog.Query().Where(entauditlog.TenantID(tid))
	if o := r.URL.Query().Get("outlet"); o != "" {
		if id, e := uuid.Parse(o); e == nil {
			q = q.Where(entauditlog.OutletID(id))
		}
	}
	if f := r.URL.Query().Get("from"); f != "" {
		if t, e := time.Parse(time.RFC3339, f); e == nil {
			q = q.Where(entauditlog.CreatedAtGTE(t))
		}
	}
	if t := r.URL.Query().Get("to"); t != "" {
		if tt, e := time.Parse(time.RFC3339, t); e == nil {
			q = q.Where(entauditlog.CreatedAtLTE(tt))
		}
	}
	// The set of actions considered exceptions for loss-prevention review.
	exceptionActions := map[string]bool{
		"order.void": true, "order.line_remove": true, "order.line_qty_decrease": true,
		"order.discount_override": true, "price.override": true, "return.refund": true,
		"drawer.no_sale": true, "drawer.pay_out": true, "drawer.cash_drop": true,
	}
	rows, err := q.All(r.Context())
	if err != nil {
		jsonError(w, "failed to load exceptions", http.StatusInternalServerError)
		return
	}
	// Aggregate per actor → per action counts (+ total amount).
	type agg struct {
		Actor   uuid.UUID         `json:"actor_user_id"`
		Counts  map[string]int    `json:"counts"`
		Amounts map[string]float64 `json:"amounts"`
		Total   int               `json:"total"`
	}
	byActor := map[uuid.UUID]*agg{}
	for _, a := range rows {
		if !exceptionActions[a.Action] {
			continue
		}
		g := byActor[a.ActorUserID]
		if g == nil {
			g = &agg{Actor: a.ActorUserID, Counts: map[string]int{}, Amounts: map[string]float64{}}
			byActor[a.ActorUserID] = g
		}
		g.Counts[a.Action]++
		g.Total++
		if a.Amount != nil {
			g.Amounts[a.Action] += *a.Amount
		}
	}
	out := make([]*agg, 0, len(byActor))
	for _, g := range byActor {
		out = append(out, g)
	}
	jsonOK(w, map[string]any{"data": out})
}

// atoiDefault parses a base-10 int, returning def on empty/invalid input.
func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
