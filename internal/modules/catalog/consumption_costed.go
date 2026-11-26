package catalog

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"time"

	sharedevents "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"

	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
)

// Order line metadata keys for the cost a sale actually carried.
const (
	unitCostAtSaleKey = "unit_cost_at_sale"
	costSourceKey     = "cost_source"
	// CostSourceInventory marks a unit_cost_at_sale replaced by inventory's actual consumption
	// cost (lot cost under FIFO/LIFO/FEFO, running average under wavg). Lines without it still
	// carry the till's standard-cost estimate.
	CostSourceInventory = "inventory"
)

// costedLine is one sold SKU of an inventory.consumption.costed event.
type costedLine struct {
	SKU       string
	TotalCost float64
}

func parseCostedLines(payload map[string]any) []costedLine {
	raw, _ := payload["lines"].([]any)
	out := make([]costedLine, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		sku, _ := m["sku"].(string)
		cost, ok := m["total_cost"].(float64)
		if sku == "" || !ok || cost < 0 {
			continue
		}
		out = append(out, costedLine{SKU: sku, TotalCost: cost})
	}
	return out
}

// unitCostsBySKU spreads each SKU's actual total cost over the quantity sold of that SKU, so the
// per-unit figure the reports multiply back by each line's quantity sums to inventory's total.
func unitCostsBySKU(lines []costedLine, qtyBySKU map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(lines))
	for _, l := range lines {
		qty := qtyBySKU[l.SKU]
		if qty <= 0 {
			continue
		}
		out[l.SKU] = math.Round(l.TotalCost/qty*10000) / 10000
	}
	return out
}

// onConsumptionCosted replaces each sold line's unit_cost_at_sale with what inventory actually
// charged for the stock (inventory owns stock cost). Idempotent: rewriting the same value is a
// no-op, so redelivery is safe. Only POS sales are touched.
func (h *InventoryEventHandler) onConsumptionCosted(ctx context.Context, evt *sharedevents.Event) error {
	if reason, _ := evt.Payload["reason"].(string); reason != "pos_sale" {
		return nil
	}
	orderID, err := uuid.Parse(fmt.Sprint(evt.Payload["order_id"]))
	if err != nil || evt.TenantID == uuid.Nil {
		return nil // malformed: nothing to retry
	}
	costed := parseCostedLines(evt.Payload)
	if len(costed) == 0 {
		return nil
	}
	exists, err := h.client.POSOrder.Query().
		Where(entposorder.ID(orderID), entposorder.TenantID(evt.TenantID)).
		Exist(ctx)
	if err != nil {
		return err
	}
	if !exists {
		return nil // an ordering-channel or other tenant order: not ours
	}
	lines, err := h.client.POSOrderLine.Query().Where(entposorderline.OrderID(orderID)).All(ctx)
	if err != nil {
		return err
	}
	qty := map[string]float64{}
	for _, l := range lines {
		qty[l.Sku] += l.Quantity
	}
	unit := unitCostsBySKU(costed, qty)
	updated := 0
	for _, l := range lines {
		c, ok := unit[l.Sku]
		if !ok {
			continue
		}
		md := l.Metadata
		if md == nil {
			md = map[string]any{}
		}
		if prev, _ := md[unitCostAtSaleKey].(float64); prev == c && md[costSourceKey] == CostSourceInventory {
			continue
		}
		if est, had := md[unitCostAtSaleKey]; had && md[costSourceKey] != CostSourceInventory {
			md["unit_cost_estimate"] = est
		}
		md[unitCostAtSaleKey] = c
		md[costSourceKey] = CostSourceInventory
		if _, err := h.client.POSOrderLine.UpdateOne(l).SetMetadata(md).Save(ctx); err != nil {
			return err
		}
		updated++
	}
	if updated > 0 {
		h.logger.Info("sale cost set from inventory consumption",
			zap.String("order_id", orderID.String()), zap.Int("lines", updated))
	}
	return nil
}

// inventoryEventRetryDelay spaces redeliveries of a failed inventory event: 2s doubling per
// delivery, capped at 2 minutes, plus up to 20% jitter. MaxDeliver on the consumer bounds the count.
func inventoryEventRetryDelay(msg *nats.Msg) time.Duration {
	delivered := uint64(1)
	if md, err := msg.Metadata(); err == nil {
		delivered = md.NumDelivered
	}
	return retryDelayFor(delivered)
}

func retryDelayFor(delivered uint64) time.Duration {
	d := 2 * time.Second
	for i := uint64(1); i < delivered && d < 2*time.Minute; i++ {
		d *= 2
	}
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	return d + time.Duration(rand.Int63n(int64(d/5)+1))
}
