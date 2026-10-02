package returns

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/posreturnline"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

const qtyEpsilon = 1e-6

// resolveReturnLines checks each requested line against the original sale and returns the
// lines to persist. The order line is the source of truth:
//   - every line must reference a line of THIS order (order_line_id, or a unique SKU match for
//     older clients that sent none);
//   - SKU and name come from the order line, so the restock targets exactly what was sold;
//   - quantity must be positive and fit within sold minus voided minus already returned. An
//     over-return used to be accepted, which overstated refunds and, once restock works, stock;
//   - on the customer path the refund value is prorated from what the line actually charged
//     (discounts included) instead of trusting unit_price x qty from the client. Edit Sale
//     (internal) already prorates the same way, so its totals are kept.
//
// pendingCounts decides which existing returns hold quantity: the customer path counts
// pending/approved/completed so two open returns cannot claim the same unit; Edit Sale counts
// completed only, matching its own returnedQtyByLine.
func (s *Service) resolveReturnLines(ctx context.Context, orderID uuid.UUID, in []LineInput, internal bool) ([]LineInput, error) {
	orderLines, err := s.client.POSOrderLine.Query().
		Where(entposorderline.OrderID(orderID)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load order: lines: %w", err)
	}
	byID := make(map[uuid.UUID]*ent.POSOrderLine, len(orderLines))
	for _, ol := range orderLines {
		byID[ol.ID] = ol
	}

	held, err := s.heldReturnQty(ctx, orderID, internal)
	if err != nil {
		return nil, err
	}

	out := make([]LineInput, 0, len(in))
	claimed := map[uuid.UUID]float64{}
	for _, l := range in {
		if l.Quantity <= 0 || math.IsNaN(l.Quantity) || math.IsInf(l.Quantity, 0) {
			return nil, fmt.Errorf("return quantity must be greater than zero")
		}
		ol := byID[l.OrderLineID]
		if ol == nil {
			ol = matchOrderLineBySKU(orderLines, l.SKU)
		}
		if ol == nil {
			label := l.Name
			if label == "" {
				label = l.SKU
			}
			return nil, fmt.Errorf("return line %q is not on this sale", label)
		}

		voided := 0.0
		if ol.VoidedQty != nil {
			voided = *ol.VoidedQty
		}
		remaining := ol.Quantity - voided - held[ol.ID] - claimed[ol.ID]
		if l.Quantity > remaining+qtyEpsilon {
			if remaining < 0 {
				remaining = 0
			}
			return nil, fmt.Errorf("cannot return %s of %s: only %s left to return on this sale",
				fmtQty(l.Quantity), ol.Name, fmtQty(remaining))
		}
		claimed[ol.ID] += l.Quantity

		line := l
		line.OrderLineID = ol.ID
		line.SKU = ol.Sku
		line.Name = ol.Name
		if !internal {
			line.UnitPrice = ol.UnitPrice
			line.TotalPrice = round2(ol.TotalPrice * (l.Quantity / ol.Quantity))
		}
		out = append(out, line)
	}
	return out, nil
}

// heldReturnQty sums, per order line, the quantity already claimed by returns on the order.
// One query over the order's returns with their lines.
func (s *Service) heldReturnQty(ctx context.Context, orderID uuid.UUID, completedOnly bool) (map[uuid.UUID]float64, error) {
	statuses := []posreturn.Status{posreturn.StatusPending, posreturn.StatusApproved, posreturn.StatusCompleted}
	if completedOnly {
		statuses = []posreturn.Status{posreturn.StatusCompleted}
	}
	rows, err := s.client.POSReturnLine.Query().
		Where(posreturnline.HasReturnWith(posreturn.OrderID(orderID), posreturn.StatusIn(statuses...))).
		Select(posreturnline.FieldOrderLineID, posreturnline.FieldQuantity).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load order: prior returns: %w", err)
	}
	out := make(map[uuid.UUID]float64, len(rows))
	for _, r := range rows {
		out[r.OrderLineID] += r.Quantity
	}
	return out, nil
}

// matchOrderLineBySKU resolves a line sent without order_line_id. Only an unambiguous match
// counts: a SKU sold on two lines needs the explicit id.
func matchOrderLineBySKU(lines []*ent.POSOrderLine, sku string) *ent.POSOrderLine {
	if sku == "" {
		return nil
	}
	var found *ent.POSOrderLine
	for _, ol := range lines {
		if ol.Sku == sku {
			if found != nil {
				return nil
			}
			found = ol
		}
	}
	return found
}

// soldQtyBySKU totals each SKU's sold quantity on the order. Inventory prorates a return's
// recorded consumption by quantity / of_quantity (a SKU may span several lines).
func (s *Service) soldQtyBySKU(ctx context.Context, orderID uuid.UUID) map[string]float64 {
	lines, err := s.client.POSOrderLine.Query().
		Where(entposorderline.OrderID(orderID)).
		Select(entposorderline.FieldSku, entposorderline.FieldQuantity).
		All(ctx)
	out := map[string]float64{}
	if err != nil {
		return out
	}
	for _, l := range lines {
		out[l.Sku] += l.Quantity
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// lockUnsupported reports SQLite's refusal of SELECT ... FOR UPDATE. SQLite only backs the unit
// tests and serializes writers itself, so callers fall back to an unlocked read there; Postgres
// always takes the row lock.
func lockUnsupported(err error) bool {
	return err != nil && strings.Contains(err.Error(), "FOR UPDATE/SHARE not supported")
}

// lockOrder takes the order row lock for the rest of the transaction.
func lockOrder(ctx context.Context, tx *ent.Tx, orderID uuid.UUID) error {
	_, err := tx.POSOrder.Query().Where(entposorder.ID(orderID)).ForUpdate().Only(ctx)
	if lockUnsupported(err) {
		return nil
	}
	return err
}

// lockReturn loads a return row under lock for the rest of the transaction.
func lockReturn(ctx context.Context, tx *ent.Tx, preds ...predicate.POSReturn) (*ent.POSReturn, error) {
	ret, err := tx.POSReturn.Query().Where(preds...).ForUpdate().Only(ctx)
	if lockUnsupported(err) {
		return tx.POSReturn.Query().Where(preds...).Only(ctx)
	}
	return ret, err
}

func fmtQty(q float64) string {
	if q == math.Trunc(q) {
		return fmt.Sprintf("%.0f", q)
	}
	return fmt.Sprintf("%.2f", q)
}
