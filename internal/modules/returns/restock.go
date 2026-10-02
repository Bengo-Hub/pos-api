package returns

import (
	"context"
	"fmt"
	"sort"
	"time"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/posreturnline"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

// Restock tracking lives in POSReturn.metadata (no schema change). inventory-api reports each
// return's outcome on inventory.return.restocked and the return detail page shows it.
const (
	mdRestockStatus    = "restock_status"
	mdRestockUpdatedAt = "restock_updated_at"
	mdRestockLines     = "restock_lines"
	mdRestockLocations = "restock_locations"
	mdRestockSkipped   = "restock_skipped"
	mdRestockError     = "restock_error"
	mdRestockResyncAt  = "restock_resync_requested_at"

	restockPending          = "pending"
	restockDone             = "restocked"
	restockAlreadyDone      = "already_restocked"
	restockNothing          = "nothing_to_restock"
	restockNotEntitled      = "skipped_not_entitled"
	restockFailed           = "failed"
	restockResyncMaxBatch   = 200
	restockOutcomeDurable   = "pos-inv-return-restocked"
	restockOutcomeSubject   = "inventory.return.restocked"
	restockOutcomeAckWait   = 30 * time.Second
	restockOutcomeMaxDelivr = 5
)

// settledRestockStatuses are final: nothing left for a resync to do.
var settledRestockStatuses = []any{restockDone, restockAlreadyDone, restockNothing, restockNotEntitled}

// RestockOutcome is inventory.return.restocked's payload.
type RestockOutcome struct {
	TenantID    string               `json:"tenant_id"`
	ReturnID    string               `json:"return_id"`
	Status      string               `json:"status"`
	ProcessedAt string               `json:"processed_at"`
	Skipped     []string             `json:"skipped"`
	Error       string               `json:"error"`
	Lines       []RestockOutcomeLine `json:"lines"`
}

// RestockOutcomeLine is where one returned SKU went. OutletID is the branch the warehouse serves
// (nil for a shared/HQ warehouse); pos-api resolves its name, since location means outlet here.
type RestockOutcomeLine struct {
	SKU           string  `json:"sku"`
	Quantity      float64 `json:"quantity"`
	WarehouseID   string  `json:"warehouse_id"`
	WarehouseName string  `json:"warehouse_name"`
	OutletID      string  `json:"outlet_id"`
	Method        string  `json:"method"`
}

// ApplyRestockOutcome records inventory's restock result on the return. The row is locked so a
// concurrent metadata write (approval notes, a resync) is never lost, and a confirmed restock is
// never downgraded by a late "failed" report.
func (s *Service) ApplyRestockOutcome(ctx context.Context, tenantID, returnID uuid.UUID, out RestockOutcome) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("restock outcome: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ret, err := lockReturn(ctx, tx, posreturn.ID(returnID), posreturn.TenantID(tenantID))
	if err != nil {
		if ent.IsNotFound(err) {
			return nil // not ours (or deleted): nothing to record
		}
		return fmt.Errorf("restock outcome: load return: %w", err)
	}

	md := cloneReturnMetadata(ret.Metadata)
	current, _ := md[mdRestockStatus].(string)
	status := out.Status
	if status == restockFailed && (current == restockDone || current == restockAlreadyDone) {
		return nil
	}
	// A replay of an already-applied restock keeps the original "restocked" wording.
	if status == restockAlreadyDone && current == restockDone {
		status = restockDone
	}
	md[mdRestockStatus] = status
	md[mdRestockUpdatedAt] = time.Now().UTC().Format(time.RFC3339)

	if len(out.Lines) > 0 {
		outletNames := s.outletNames(ctx, tx.Client(), tenantID, out.Lines)
		lines := make([]map[string]any, 0, len(out.Lines))
		seen := map[string]bool{}
		var locations []string
		for _, l := range out.Lines {
			// Location is the outlet (branch); a shared/HQ warehouse has none, so its name stands in.
			location := outletNames[l.OutletID]
			if location == "" {
				location = l.WarehouseName
			}
			lines = append(lines, map[string]any{
				"sku": l.SKU, "quantity": l.Quantity, "method": l.Method,
				"outlet_id": l.OutletID, "outlet_name": outletNames[l.OutletID],
				"warehouse_id": l.WarehouseID, "warehouse_name": l.WarehouseName,
				"location": location,
			})
			if location != "" && !seen[location] {
				seen[location] = true
				locations = append(locations, location)
			}
		}
		sort.Strings(locations)
		md[mdRestockLines] = lines
		md[mdRestockLocations] = locations
	}
	if len(out.Skipped) > 0 {
		md[mdRestockSkipped] = out.Skipped
	} else {
		delete(md, mdRestockSkipped)
	}
	if status == restockFailed && out.Error != "" {
		md[mdRestockError] = out.Error
	} else {
		delete(md, mdRestockError)
	}

	if _, err := tx.POSReturn.UpdateOne(ret).SetMetadata(md).Save(ctx); err != nil {
		return fmt.Errorf("restock outcome: save: %w", err)
	}
	return tx.Commit()
}

// outletNames resolves the outlets named in an outcome with one batched query.
func (s *Service) outletNames(ctx context.Context, client *ent.Client, tenantID uuid.UUID, lines []RestockOutcomeLine) map[string]string {
	ids := make([]uuid.UUID, 0, len(lines))
	for _, l := range lines {
		if id, err := uuid.Parse(l.OutletID); err == nil && id != uuid.Nil {
			ids = append(ids, id)
		}
	}
	out := map[string]string{}
	if len(ids) == 0 {
		return out
	}
	outlets, err := client.Outlet.Query().
		Where(entoutlet.TenantID(tenantID), entoutlet.IDIn(ids...)).
		Select(entoutlet.FieldID, entoutlet.FieldName).
		All(ctx)
	if err != nil {
		s.log.Warn("restock outcome: outlet names lookup failed", zap.Error(err))
		return out
	}
	for _, o := range outlets {
		out[o.ID.String()] = o.Name
	}
	return out
}

// ResyncRestockRequest selects completed returns whose restock should be (re)requested.
// ReturnIDs targets specific returns regardless of their recorded status (inventory dedupes, so
// a return already restocked is reported back as such and never restocked twice). Without IDs,
// only returns not confirmed restocked are picked, optionally within a completion-date window.
type ResyncRestockRequest struct {
	ReturnIDs []uuid.UUID
	From, To  *time.Time
	DryRun    bool
}

// ResyncRestockItem is one return the resync picked.
type ResyncRestockItem struct {
	ReturnID      uuid.UUID `json:"return_id"`
	ReturnNumber  string    `json:"return_number"`
	OrderID       uuid.UUID `json:"order_id"`
	ReturnType    string    `json:"return_type"`
	CreatedAt     time.Time `json:"created_at"`
	RestockStatus string    `json:"restock_status"`
	Lines         []string  `json:"lines"`
	Requested     bool      `json:"requested"`
	Error         string    `json:"error,omitempty"`
}

// ResyncRestockResult reports what the resync did. More is true when the batch cap was hit.
type ResyncRestockResult struct {
	DryRun bool                `json:"dry_run"`
	Items  []ResyncRestockItem `json:"items"`
	More   bool                `json:"more"`
}

// outstandingRestock matches returns whose metadata has no settled restock status. Covers every
// return completed before restock tracking existed (no key at all).
func outstandingRestock() predicate.POSReturn {
	return predicate.POSReturn(func(s *sql.Selector) {
		s.Where(sql.Or(
			sql.IsNull(s.C(posreturn.FieldMetadata)),
			sql.Not(sqljson.HasKey(posreturn.FieldMetadata, sqljson.Path(mdRestockStatus))),
			sql.Not(sqljson.ValueIn(posreturn.FieldMetadata, settledRestockStatuses, sqljson.Path(mdRestockStatus))),
		))
	})
}

// ResyncRestock re-requests inventory restock for completed returns via
// pos.return.restock_requested. Bounded to restockResyncMaxBatch per call (oldest first) so a
// large backlog is worked through in pages, never in one unbounded scan.
func (s *Service) ResyncRestock(ctx context.Context, tenantID uuid.UUID, req ResyncRestockRequest) (*ResyncRestockResult, error) {
	q := s.client.POSReturn.Query().
		Where(posreturn.TenantID(tenantID), posreturn.StatusEQ(posreturn.StatusCompleted))
	if len(req.ReturnIDs) > 0 {
		q = q.Where(posreturn.IDIn(req.ReturnIDs...))
	} else {
		q = q.Where(outstandingRestock())
	}
	if req.From != nil {
		q = q.Where(posreturn.CreatedAtGTE(*req.From))
	}
	if req.To != nil {
		q = q.Where(posreturn.CreatedAtLTE(*req.To))
	}
	rets, err := q.
		WithLines(func(lq *ent.POSReturnLineQuery) { lq.Order(ent.Asc(posreturnline.FieldID)) }).
		Order(ent.Asc(posreturn.FieldCreatedAt)).
		Limit(restockResyncMaxBatch + 1).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("restock resync: list: %w", err)
	}

	res := &ResyncRestockResult{DryRun: req.DryRun, Items: []ResyncRestockItem{}}
	if len(rets) > restockResyncMaxBatch {
		res.More = true
		rets = rets[:restockResyncMaxBatch]
	}
	for _, ret := range rets {
		status, _ := ret.Metadata[mdRestockStatus].(string)
		item := ResyncRestockItem{
			ReturnID: ret.ID, ReturnNumber: ret.ReturnNumber, OrderID: ret.OrderID,
			ReturnType: string(ret.ReturnType), CreatedAt: ret.CreatedAt, RestockStatus: status,
		}
		for _, l := range ret.Edges.Lines {
			item.Lines = append(item.Lines, fmt.Sprintf("%s x%s", l.Sku, fmtQty(l.Quantity)))
		}
		if !req.DryRun {
			if err := s.requestRestock(ctx, ret); err != nil {
				item.Error = err.Error()
			} else {
				item.Requested = true
				item.RestockStatus = restockPending
			}
		}
		res.Items = append(res.Items, item)
	}
	return res, nil
}

// requestRestock publishes the restock-only event and marks the return pending.
func (s *Service) requestRestock(ctx context.Context, ret *ent.POSReturn) error {
	if s.publisher == nil {
		return fmt.Errorf("event publisher not configured")
	}
	if len(ret.Edges.Lines) == 0 {
		return fmt.Errorf("return has no lines")
	}
	payload := s.completedPayload(ctx, ret, ret.Edges.Lines, "")
	if err := s.publisher.PublishReturnRestockRequested(ctx, ret.TenantID, payload); err != nil {
		return err
	}

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	locked, err := lockReturn(ctx, tx, posreturn.ID(ret.ID))
	if err != nil {
		return err
	}
	md := cloneReturnMetadata(locked.Metadata)
	if cur, _ := md[mdRestockStatus].(string); cur != restockDone && cur != restockAlreadyDone {
		md[mdRestockStatus] = restockPending
	}
	md[mdRestockResyncAt] = time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.POSReturn.UpdateOne(locked).SetMetadata(md).Save(ctx); err != nil {
		return err
	}
	return tx.Commit()
}

// SubscribeRestockOutcomes consumes inventory.return.restocked and records each outcome.
func (s *Service) SubscribeRestockOutcomes(nc *nats.Conn) error {
	js, err := nc.JetStream()
	if err != nil {
		return fmt.Errorf("restock outcomes: jetstream: %w", err)
	}
	if _, err := js.StreamInfo("inventory"); err != nil {
		if _, addErr := js.AddStream(&nats.StreamConfig{
			Name: "inventory", Subjects: []string{"inventory.>"}, Retention: nats.LimitsPolicy,
			MaxAge: 72 * time.Hour, Storage: nats.FileStorage,
		}); addErr != nil && addErr != nats.ErrStreamNameAlreadyInUse {
			return fmt.Errorf("restock outcomes: ensure inventory stream: %w", addErr)
		}
	}
	sharedevents.SubscribeQueueWithRebind(s.log, js, "inventory", restockOutcomeSubject, restockOutcomeDurable,
		func(msg *nats.Msg) {
			env, out, derr := sharedevents.DecodeEvent[RestockOutcome](msg.Data)
			if derr != nil {
				s.log.Warn("restock outcome: malformed, dropping", zap.Error(derr))
				_ = msg.Term()
				return
			}
			tenantID := env.TenantID
			if tenantID == uuid.Nil {
				tenantID, _ = uuid.Parse(out.TenantID)
			}
			returnID, perr := uuid.Parse(out.ReturnID)
			if tenantID == uuid.Nil || perr != nil {
				_ = msg.Term()
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), restockOutcomeAckWait-5*time.Second)
			defer cancel()
			if aerr := s.ApplyRestockOutcome(ctx, tenantID, returnID, out); aerr != nil {
				s.log.Warn("restock outcome: apply failed, retrying", zap.String("return_id", out.ReturnID), zap.Error(aerr))
				_ = msg.NakWithDelay(10 * time.Second)
				return
			}
			_ = msg.Ack()
		},
		nats.Durable(restockOutcomeDurable),
		nats.DeliverAll(),
		nats.AckExplicit(),
		nats.AckWait(restockOutcomeAckWait),
		nats.MaxDeliver(restockOutcomeMaxDelivr),
	)
	s.log.Info("return restock outcome subscriber started")
	return nil
}
