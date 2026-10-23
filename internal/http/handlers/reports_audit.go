package handlers

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entauditlog "github.com/bengobox/pos-service/internal/ent/auditlog"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/posreversal"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
)

// auditLogDTO is one audit entry in list responses.
type auditLogDTO struct {
	ID       uuid.UUID  `json:"id"`
	OutletID *uuid.UUID `json:"outlet_id,omitempty"`
	Actor    uuid.UUID  `json:"actor_user_id"`
	// ActorName is the actor's staff display name so the UI never shows a raw UUID; empty when the
	// actor isn't a resolvable staff member (the UI falls back to a short id).
	ActorName    string         `json:"actor_name,omitempty"`
	Approver     *uuid.UUID     `json:"approver_user_id,omitempty"`
	ApproverName string         `json:"approver_name,omitempty"`
	Action       string         `json:"action"`
	EntityType   string         `json:"entity_type,omitempty"`
	EntityID     string         `json:"entity_id,omitempty"`
	Reason       string         `json:"reason,omitempty"`
	Before       map[string]any `json:"before_json,omitempty"`
	After        map[string]any `json:"after_json,omitempty"`
	Amount       *float64       `json:"amount,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	// Client is the device, browser and IP the action came from (audit.TrailHook); absent on
	// entries written before 2026-10-23 or by background work.
	Client map[string]any `json:"client,omitempty"`
	// EntityLabel is a human reference for the entity ("Order 002384", "Return R-0007").
	EntityLabel string `json:"entity_label,omitempty"`
	// OrderID is the order the entry concerns, when there is one, so the UI can link to it.
	OrderID *uuid.UUID `json:"order_id,omitempty"`
}

// resolveActorNames maps auth user ids to a staff display name ("Jane Doe (cashier)") for this
// tenant, so audit reports show WHO acted instead of a raw UUID. Ids with no matching staff member
// are omitted; the caller falls back to a short id. One query for the whole result set.
func (h *ReportsHandler) resolveActorNames(ctx context.Context, tid uuid.UUID, ids []uuid.UUID) map[uuid.UUID]string {
	return staffNames(ctx, h.db, tid, ids)
}

// staffNames is resolveActorNames for callers without a ReportsHandler (the PDF reports).
func staffNames(ctx context.Context, db *ent.Client, tid uuid.UUID, ids []uuid.UUID) map[uuid.UUID]string {
	names := map[uuid.UUID]string{}
	uniq := make([]uuid.UUID, 0, len(ids))
	seen := map[uuid.UUID]bool{}
	for _, id := range ids {
		if id == uuid.Nil || seen[id] {
			continue
		}
		seen[id] = true
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return names
	}
	members, err := db.StaffMember.Query().
		Where(entstaff.TenantID(tid), entstaff.UserIDIn(uniq...)).
		All(ctx)
	if err != nil {
		return names
	}
	for _, m := range members {
		if m.Name == "" {
			continue
		}
		label := m.Name
		if m.Role != "" {
			label += " (" + m.Role + ")"
		}
		names[m.UserID] = label
	}
	return names
}

// auditRangePreds parses the shared outlet/from/to filters.
func auditRangePreds(r *http.Request) []predicate.AuditLog {
	var preds []predicate.AuditLog
	if o := r.URL.Query().Get("outlet"); o != "" {
		if id, e := uuid.Parse(o); e == nil {
			preds = append(preds, entauditlog.OutletID(id))
		}
	}
	if f := r.URL.Query().Get("from"); f != "" {
		if t, e := time.Parse(time.RFC3339, f); e == nil {
			preds = append(preds, entauditlog.CreatedAtGTE(t))
		}
	}
	if t := r.URL.Query().Get("to"); t != "" {
		if tt, e := time.Parse(time.RFC3339, t); e == nil {
			preds = append(preds, entauditlog.CreatedAtLTE(tt))
		}
	}
	return preds
}

// metadataClientEQ matches entries whose client trail key equals v.
func metadataClientEQ(key, v string) predicate.AuditLog {
	return func(s *entsql.Selector) {
		s.Where(sqljson.ValueEQ(s.C(entauditlog.FieldMetadata), v, sqljson.Path(audit.KeyClient, key)))
	}
}

// ListAuditLogs handles GET /{tenantID}/pos/reports/audit-logs with optional filters: action
// (one code or a comma list), actor, outlet, from, to, client (a browser's client id), ip,
// order (an order number: the order's own entries and its lines'), limit (max 200), offset.
// Returns {data,total}.
func (h *ReportsHandler) ListAuditLogs(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	qs := r.URL.Query()
	preds := append([]predicate.AuditLog{entauditlog.TenantID(tid)}, auditRangePreds(r)...)
	if a := strings.TrimSpace(qs.Get("action")); a != "" {
		preds = append(preds, entauditlog.ActionIn(splitCSV(a)...))
	}
	if a := qs.Get("actor"); a != "" {
		if id, e := uuid.Parse(a); e == nil {
			preds = append(preds, entauditlog.ActorUserID(id))
		}
	}
	if c := strings.TrimSpace(qs.Get("client")); c != "" {
		preds = append(preds, metadataClientEQ("client_id", c))
	}
	if ip := strings.TrimSpace(qs.Get("ip")); ip != "" {
		preds = append(preds, metadataClientEQ("ip", ip))
	}
	if num := strings.TrimSpace(qs.Get("order")); num != "" {
		ids := h.orderEntityIDs(ctx, tid, num)
		if len(ids) == 0 {
			jsonOK(w, map[string]any{"data": []auditLogDTO{}, "total": 0})
			return
		}
		preds = append(preds, entauditlog.EntityIDIn(ids...))
	}
	q := h.db.AuditLog.Query().Where(preds...)
	total, _ := q.Clone().Count(ctx)
	limit := atoiDefault(qs.Get("limit"), 50)
	if limit < 1 || limit > 200 {
		limit = 50
	}
	offset := atoiDefault(qs.Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	rows, err := q.Order(ent.Desc(entauditlog.FieldCreatedAt)).Limit(limit).Offset(offset).All(ctx)
	if err != nil {
		jsonError(w, "failed to load audit logs", http.StatusInternalServerError)
		return
	}
	// Resolve actor + approver ids to staff names in one query so each row shows who acted/approved.
	ids := make([]uuid.UUID, 0, len(rows)*2)
	for _, a := range rows {
		ids = append(ids, a.ActorUserID)
		if a.ApproverUserID != nil {
			ids = append(ids, *a.ApproverUserID)
		}
	}
	names := h.resolveActorNames(ctx, tid, ids)
	labels := h.auditEntityLabels(ctx, tid, rows)
	out := make([]auditLogDTO, 0, len(rows))
	for _, a := range rows {
		dto := auditLogDTO{
			ID: a.ID, OutletID: a.OutletID, Actor: a.ActorUserID, ActorName: names[a.ActorUserID],
			Approver: a.ApproverUserID, Action: a.Action, EntityType: a.EntityType, EntityID: a.EntityID,
			Reason: a.Reason, Before: a.BeforeJSON, After: a.AfterJSON, Amount: a.Amount, CreatedAt: a.CreatedAt,
		}
		if a.ApproverUserID != nil {
			dto.ApproverName = names[*a.ApproverUserID]
		}
		if c, ok := a.Metadata[audit.KeyClient].(map[string]any); ok {
			dto.Client = c
		}
		if l, ok := labels[a.EntityType+":"+a.EntityID]; ok {
			dto.EntityLabel = l.label
			dto.OrderID = l.orderID
		}
		out = append(out, dto)
	}
	jsonOK(w, map[string]any{"data": out, "total": total})
}

type entityLabel struct {
	label   string
	orderID *uuid.UUID
}

// auditEntityLabels resolves the page's entity ids to human references with one query per
// entity type present (orders, order lines, returns, reversals).
func (h *ReportsHandler) auditEntityLabels(ctx context.Context, tid uuid.UUID, rows []*ent.AuditLog) map[string]entityLabel {
	byType := map[string][]uuid.UUID{}
	for _, a := range rows {
		if id, err := uuid.Parse(a.EntityID); err == nil {
			byType[a.EntityType] = append(byType[a.EntityType], id)
		}
	}
	out := map[string]entityLabel{}
	orderIDs := append([]uuid.UUID(nil), byType["pos_order"]...)
	lineOrder := map[uuid.UUID]uuid.UUID{}
	if ids := byType["pos_order_line"]; len(ids) > 0 {
		lines, _ := h.db.POSOrderLine.Query().
			Where(posorderline.IDIn(ids...)).
			Select(posorderline.FieldID, posorderline.FieldOrderID).
			All(ctx)
		for _, l := range lines {
			lineOrder[l.ID] = l.OrderID
			orderIDs = append(orderIDs, l.OrderID)
		}
	}
	returnOrder := map[uuid.UUID]*ent.POSReturn{}
	if ids := byType["pos_return"]; len(ids) > 0 {
		rets, _ := h.db.POSReturn.Query().
			Where(posreturn.TenantID(tid), posreturn.IDIn(ids...)).
			Select(posreturn.FieldID, posreturn.FieldOrderID, posreturn.FieldReturnNumber).
			All(ctx)
		for _, rt := range rets {
			returnOrder[rt.ID] = rt
			orderIDs = append(orderIDs, rt.OrderID)
		}
	}
	numbers := map[uuid.UUID]string{}
	if len(orderIDs) > 0 {
		orders, _ := h.db.POSOrder.Query().
			Where(posorder.TenantID(tid), posorder.IDIn(orderIDs...)).
			Select(posorder.FieldID, posorder.FieldOrderNumber).
			All(ctx)
		for _, o := range orders {
			numbers[o.ID] = o.OrderNumber
		}
	}
	orderLabel := func(id uuid.UUID) (entityLabel, bool) {
		n, ok := numbers[id]
		if !ok {
			return entityLabel{}, false
		}
		oid := id
		return entityLabel{label: "Order " + n, orderID: &oid}, true
	}
	for _, id := range byType["pos_order"] {
		if l, ok := orderLabel(id); ok {
			out["pos_order:"+id.String()] = l
		}
	}
	for lineID, oid := range lineOrder {
		if l, ok := orderLabel(oid); ok {
			out["pos_order_line:"+lineID.String()] = l
		}
	}
	for id, rt := range returnOrder {
		l, _ := orderLabel(rt.OrderID)
		label := "Return " + rt.ReturnNumber
		if l.label != "" {
			label += " · " + l.label
		}
		out["pos_return:"+id.String()] = entityLabel{label: label, orderID: l.orderID}
	}
	if ids := byType["pos_reversal"]; len(ids) > 0 {
		revs, _ := h.db.POSReversal.Query().
			Where(posreversal.TenantID(tid), posreversal.IDIn(ids...)).
			Select(posreversal.FieldID, posreversal.FieldOrderID, posreversal.FieldReversalNumber).
			All(ctx)
		for _, rv := range revs {
			label := "Reversal " + rv.ReversalNumber
			var oid *uuid.UUID
			if n, ok := numbers[rv.OrderID]; ok {
				label += " · Order " + n
			}
			id := rv.OrderID
			oid = &id
			out["pos_reversal:"+rv.ID.String()] = entityLabel{label: label, orderID: oid}
		}
	}
	return out
}

// orderEntityIDs returns the entity ids (the order itself and its lines) for an order number.
func (h *ReportsHandler) orderEntityIDs(ctx context.Context, tid uuid.UUID, number string) []string {
	order, err := h.db.POSOrder.Query().
		Where(posorder.TenantID(tid), posorder.OrderNumber(number)).
		Select(posorder.FieldID).
		First(ctx)
	if err != nil {
		return nil
	}
	ids := []string{order.ID.String()}
	lineIDs, _ := h.db.POSOrderLine.Query().Where(posorderline.OrderID(order.ID)).IDs(ctx)
	for _, id := range lineIDs {
		ids = append(ids, id.String())
	}
	return ids
}

// exceptionActions are the actions counted for loss-prevention review.
var exceptionActions = []string{
	"order.void", "order.delete", "order.draft_deleted", "order.line_remove", "order.line_qty_decrease",
	"order.edit_reduce", "order.line_price_edit", "order.discount_override", "price.override",
	"order.complimentary", "order.reversal", "return.refund", "receipt.reprint",
	"drawer.no_sale", "drawer.pay_out", "drawer.cash_drop",
	actionPINFailed, "order.offline_sync",
}

// exceptionsDefaultRange bounds the report when the caller sends no from date, so the query
// cost tracks a review period rather than the tenant's whole history.
const exceptionsDefaultRange = 30 * 24 * time.Hour

// Exceptions handles GET /{tenantID}/pos/reports/exceptions: per staff member, counts and
// amounts of fraud-relevant actions over a date range (default: the last 30 days), plus the
// devices and IPs they signed in from. Offline sales count only when they were synced under a
// different login than the person who rang them up. Failed PINs that matched nobody are
// grouped under the zero user id.
func (h *ReportsHandler) Exceptions(w http.ResponseWriter, r *http.Request) {
	tid, err := parseTenantUUID(r)
	if err != nil {
		jsonError(w, "invalid tenant_id", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	preds := append([]predicate.AuditLog{entauditlog.TenantID(tid)}, auditRangePreds(r)...)
	from := time.Now().Add(-exceptionsDefaultRange)
	if f := r.URL.Query().Get("from"); f != "" {
		if t, e := time.Parse(time.RFC3339, f); e == nil {
			from = t
		}
	} else {
		preds = append(preds, entauditlog.CreatedAtGTE(from))
	}

	var counts []struct {
		Actor  uuid.UUID `json:"actor_user_id"`
		Action string    `json:"action"`
		Count  int       `json:"count"`
		Amount float64   `json:"amount"`
	}
	err = h.db.AuditLog.Query().
		Where(append(preds,
			entauditlog.ActionIn(exceptionActions...),
			entauditlog.Or(entauditlog.ActionNEQ("order.offline_sync"), entauditlog.ReasonContains("different login")),
		)...).
		GroupBy(entauditlog.FieldActorUserID, entauditlog.FieldAction).
		Aggregate(ent.Count(), func(s *entsql.Selector) string {
			return entsql.As("COALESCE(SUM("+s.C(entauditlog.FieldAmount)+"), 0)", "amount")
		}).
		Scan(ctx, &counts)
	if err != nil {
		jsonError(w, "failed to load exceptions", http.StatusInternalServerError)
		return
	}

	type deviceUse struct {
		Label    string `json:"label"`
		ClientID string `json:"client_id,omitempty"`
		IP       string `json:"ip,omitempty"`
		Kind     string `json:"device_kind,omitempty"`
		SignIns  int    `json:"sign_ins"`
		LastAt   string `json:"last_at"`
	}
	type agg struct {
		Actor uuid.UUID `json:"actor_user_id"`
		// ActorName is the staff member's name so the report never shows a raw UUID.
		ActorName   string             `json:"actor_name,omitempty"`
		Counts      map[string]int     `json:"counts"`
		Amounts     map[string]float64 `json:"amounts"`
		Total       int                `json:"total"`
		SignIns     int                `json:"sign_ins"`
		DeviceCount int                `json:"device_count"`
		IPCount     int                `json:"ip_count"`
		Devices     []deviceUse        `json:"devices"`
	}
	byActor := map[uuid.UUID]*agg{}
	get := func(id uuid.UUID) *agg {
		g := byActor[id]
		if g == nil {
			g = &agg{Actor: id, Counts: map[string]int{}, Amounts: map[string]float64{}, Devices: []deviceUse{}}
			byActor[id] = g
		}
		return g
	}
	for _, c := range counts {
		g := get(c.Actor)
		g.Counts[c.Action] += c.Count
		g.Total += c.Count
		if c.Amount != 0 {
			g.Amounts[c.Action] += c.Amount
		}
	}

	// Devices and IPs each person signed in from in the period (only the two needed columns).
	signIns, err := h.db.AuditLog.Query().
		Where(append(preds, entauditlog.Action(actionPINLogin))...).
		Select(entauditlog.FieldActorUserID, entauditlog.FieldMetadata, entauditlog.FieldCreatedAt).
		All(ctx)
	if err != nil {
		jsonError(w, "failed to load exceptions", http.StatusInternalServerError)
		return
	}
	type devAcc struct {
		use deviceUse
		at  time.Time
	}
	devs := map[uuid.UUID]map[string]*devAcc{}
	ips := map[uuid.UUID]map[string]bool{}
	for _, s := range signIns {
		c, _ := s.Metadata[audit.KeyClient].(map[string]any)
		info := clientInfoFromMap(c)
		key := deviceClientKey(info)
		if key == "" {
			key = "unknown"
		}
		if devs[s.ActorUserID] == nil {
			devs[s.ActorUserID] = map[string]*devAcc{}
			ips[s.ActorUserID] = map[string]bool{}
		}
		if info.IP != "" {
			ips[s.ActorUserID][info.IP] = true
		}
		d := devs[s.ActorUserID][key]
		if d == nil {
			label := info.Label()
			if label == "" {
				label = "Unknown device"
			}
			d = &devAcc{use: deviceUse{Label: label, ClientID: info.ClientID, Kind: info.DeviceKind}}
			devs[s.ActorUserID][key] = d
		}
		d.use.SignIns++
		if s.CreatedAt.After(d.at) {
			d.at = s.CreatedAt
			d.use.IP = info.IP
			d.use.LastAt = s.CreatedAt.UTC().Format(time.RFC3339)
		}
	}
	for actor, m := range devs {
		g := get(actor)
		for _, d := range m {
			g.SignIns += d.use.SignIns
			g.Devices = append(g.Devices, d.use)
		}
		sort.Slice(g.Devices, func(i, j int) bool { return g.Devices[i].LastAt > g.Devices[j].LastAt })
		g.DeviceCount = len(g.Devices)
		g.IPCount = len(ips[actor])
	}

	// Resolve the actors to staff names in one query and stamp each row.
	ids := make([]uuid.UUID, 0, len(byActor))
	for id := range byActor {
		ids = append(ids, id)
	}
	names := h.resolveActorNames(ctx, tid, ids)
	out := make([]*agg, 0, len(byActor))
	for _, g := range byActor {
		g.ActorName = names[g.Actor]
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].SignIns > out[j].SignIns
	})
	jsonOK(w, map[string]any{"data": out, "from": from.UTC().Format(time.RFC3339)})
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
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
