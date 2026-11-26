// Package tableseat owns which order sits at which dine-in table: seating an order (idempotent),
// releasing it, finding a table's live order and the "one open bill per table" guard.
//
// Before 2026-11-26 this logic lived inline in the tables handler and the payments service, and
// the till assigned tables itself after creating the order. An order rung up offline was assigned
// under its local uuid, which matched no server order, so the table showed no bill and the waiter
// rang the order up again (urban-loft 003390/003392). Seating now happens on the server when the
// order is created, so online and synced offline orders behave the same.
package tableseat

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entoutletsetting "github.com/bengobox/pos-service/internal/ent/outletsetting"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	enttable "github.com/bengobox/pos-service/internal/ent/table"
	"github.com/bengobox/pos-service/internal/ent/tableassignment"
)

// GuardMode is the outlet's policy for starting a second bill on a table that already has one.
// It lives in outlet settings metadata under "table_guard" so each outlet admin can choose it.
type GuardMode string

const (
	// GuardWarn (default) rejects a second bill unless the till explicitly asks for a separate
	// bill, so the waiter is offered "Add to existing bill" first.
	GuardWarn GuardMode = "warn"
	// GuardBlock allows only one open bill per table; splitting goes through the split feature.
	GuardBlock GuardMode = "block"
	// GuardOff keeps the old behaviour: any number of bills per table.
	GuardOff GuardMode = "off"
)

// MetadataKey is the outlet settings metadata key holding the GuardMode.
const MetadataKey = "table_guard"

// closedStatuses are the order statuses that no longer hold a table.
var closedStatuses = []string{"completed", "cancelled", "refunded", "voided"}

// ErrTableHasOpenOrder is matched with errors.Is by handlers to answer 409 table_has_open_order.
var ErrTableHasOpenOrder = errors.New("table has an open order")

// ErrTableNotFound means the table id does not belong to the tenant.
var ErrTableNotFound = errors.New("table not found")

// ConflictError carries the bill already on the table so the till can offer "Add to existing bill".
type ConflictError struct {
	TableID     uuid.UUID
	OrderID     uuid.UUID
	OrderNumber string
	Mode        GuardMode
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("table already has open order %s", e.OrderNumber)
}

func (e *ConflictError) Is(target error) bool { return target == ErrTableHasOpenOrder }

// ParseGuardMode normalises a stored value; anything unknown falls back to GuardWarn.
func ParseGuardMode(v any) GuardMode {
	s, _ := v.(string)
	switch GuardMode(strings.ToLower(strings.TrimSpace(s))) {
	case GuardBlock:
		return GuardBlock
	case GuardOff:
		return GuardOff
	default:
		return GuardWarn
	}
}

// OutletGuardMode reads the outlet's table guard from its settings metadata.
func OutletGuardMode(ctx context.Context, client *ent.Client, outletID uuid.UUID) GuardMode {
	setting, err := client.OutletSetting.Query().
		Where(entoutletsetting.OutletID(outletID)).
		Select(entoutletsetting.FieldMetadata).
		Only(ctx)
	if err != nil || setting == nil {
		return GuardWarn
	}
	return ParseGuardMode(setting.Metadata[MetadataKey])
}

// OpenOrderOnTable returns the newest still-open order seated at the table, or nil.
// excludeOrderID (may be uuid.Nil) is skipped so re-seating the same order is not a conflict.
func OpenOrderOnTable(ctx context.Context, client *ent.Client, tenantID, tableID, excludeOrderID uuid.UUID) (*ent.POSOrder, error) {
	asgns, err := client.TableAssignment.Query().
		Where(
			tableassignment.TableID(tableID),
			tableassignment.ReleasedAtIsNil(),
			tableassignment.OrderIDNotNil(),
		).
		Select(tableassignment.FieldOrderID).
		All(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(asgns))
	for _, a := range asgns {
		if a.OrderID != nil && *a.OrderID != excludeOrderID {
			ids = append(ids, *a.OrderID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	o, err := client.POSOrder.Query().
		Where(
			entposorder.TenantID(tenantID),
			entposorder.IDIn(ids...),
			entposorder.StatusNotIn(closedStatuses...),
		).
		Order(ent.Desc(entposorder.FieldCreatedAt)).
		First(ctx)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	return o, err
}

// CheckGuard applies the outlet's table guard before a NEW order is seated at tableID.
// separateBill is the till's explicit "open a separate bill" choice, honoured only in GuardWarn.
func CheckGuard(ctx context.Context, client *ent.Client, tenantID, outletID, tableID uuid.UUID, separateBill bool) error {
	mode := OutletGuardMode(ctx, client, outletID)
	if mode == GuardOff || (mode == GuardWarn && separateBill) {
		return nil
	}
	existing, err := OpenOrderOnTable(ctx, client, tenantID, tableID, uuid.Nil)
	if err != nil || existing == nil {
		// A lookup failure must never block a sale.
		return nil
	}
	return &ConflictError{TableID: tableID, OrderID: existing.ID, OrderNumber: existing.OrderNumber, Mode: mode}
}

// Seat marks the table occupied by the order. Idempotent: seating the same order at the same
// table again returns the existing assignment, so a replayed request never stacks assignments.
func Seat(ctx context.Context, client *ent.Client, tenantID, tableID, orderID uuid.UUID) (*ent.TableAssignment, error) {
	ok, err := client.Table.Query().Where(enttable.ID(tableID), enttable.TenantID(tenantID)).Exist(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrTableNotFound
	}
	if existing, qerr := client.TableAssignment.Query().
		Where(
			tableassignment.TableID(tableID),
			tableassignment.OrderID(orderID),
			tableassignment.ReleasedAtIsNil(),
		).
		First(ctx); qerr == nil {
		return existing, nil
	}
	if _, err := client.Table.Update().
		Where(enttable.ID(tableID), enttable.TenantID(tenantID)).
		SetStatus("occupied").
		Save(ctx); err != nil {
		return nil, err
	}
	return client.TableAssignment.Create().
		SetTableID(tableID).
		SetOrderID(orderID).
		SetAssignedAt(time.Now()).
		Save(ctx)
}

// ReleaseOrder closes the order's open assignments. A table goes back to available only when no
// other open bill is still seated there (a separate bill on the same table keeps it occupied).
func ReleaseOrder(ctx context.Context, client *ent.Client, tenantID, orderID uuid.UUID) error {
	asgns, err := client.TableAssignment.Query().
		Where(tableassignment.OrderID(orderID), tableassignment.ReleasedAtIsNil()).
		All(ctx)
	if err != nil || len(asgns) == 0 {
		return err
	}
	now := time.Now()
	var firstErr error
	for _, a := range asgns {
		if _, uerr := client.TableAssignment.UpdateOne(a).SetReleasedAt(now).Save(ctx); uerr != nil && firstErr == nil {
			firstErr = uerr
		}
		other, _ := OpenOrderOnTable(ctx, client, tenantID, a.TableID, orderID)
		if other != nil {
			continue
		}
		if _, uerr := client.Table.Update().
			Where(enttable.ID(a.TableID), enttable.TenantID(tenantID)).
			SetStatus("available").
			Save(ctx); uerr != nil && firstErr == nil {
			firstErr = uerr
		}
	}
	return firstErr
}

// LiveOrderIDs maps each table to its newest open seated order in ONE pair of queries, for the
// tables list. The previous eager load used .Limit(1) inside WithAssignments, which in ent limits
// the whole eager query (not per table), so only one table in the outlet ever showed its bill.
func LiveOrderIDs(ctx context.Context, client *ent.Client, tenantID uuid.UUID, tableIDs []uuid.UUID) (map[uuid.UUID]*ent.POSOrder, map[uuid.UUID]time.Time, error) {
	byTable := map[uuid.UUID]*ent.POSOrder{}
	since := map[uuid.UUID]time.Time{}
	if len(tableIDs) == 0 {
		return byTable, since, nil
	}
	asgns, err := client.TableAssignment.Query().
		Where(
			tableassignment.TableIDIn(tableIDs...),
			tableassignment.ReleasedAtIsNil(),
			tableassignment.OrderIDNotNil(),
		).
		Order(ent.Desc(tableassignment.FieldAssignedAt)).
		All(ctx)
	if err != nil {
		return nil, nil, err
	}
	orderIDs := make([]uuid.UUID, 0, len(asgns))
	for _, a := range asgns {
		orderIDs = append(orderIDs, *a.OrderID)
	}
	if len(orderIDs) == 0 {
		return byTable, since, nil
	}
	orders, err := client.POSOrder.Query().
		Where(
			entposorder.TenantID(tenantID),
			entposorder.IDIn(orderIDs...),
			entposorder.StatusNotIn(closedStatuses...),
		).
		Select(entposorder.FieldID, entposorder.FieldOrderNumber, entposorder.FieldTotalAmount,
			entposorder.FieldCoversCount, entposorder.FieldStatus).
		All(ctx)
	if err != nil {
		return nil, nil, err
	}
	open := make(map[uuid.UUID]*ent.POSOrder, len(orders))
	for _, o := range orders {
		open[o.ID] = o
	}
	// Assignments are newest first, so the first open order seen per table wins.
	for _, a := range asgns {
		if _, done := byTable[a.TableID]; done {
			continue
		}
		if o, ok := open[*a.OrderID]; ok {
			byTable[a.TableID] = o
			since[a.TableID] = a.AssignedAt
		}
	}
	return byTable, since, nil
}
