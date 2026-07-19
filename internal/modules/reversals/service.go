// Package reversals orchestrates platform-owner transaction reversals of FINALIZED POS
// sales (whole order or individual items) across every integrated service: POS order
// totals/void, inventory BOM consumption, treasury GL, and the KRA eTIMS credit note when
// the sale was fiscalised. Each cross-service step is tracked on the POSReversal record so
// the sync-monitor "Txn Reversals" tab can show exactly what happened and retry failures.
//
// This is a data-repair tool, distinct from the customer-facing POSReturn lifecycle: it
// corrects the original order record itself (soft-voided lines, recomputed totals, netted
// payments per the 2026-07-17 platform decision) while reusing the SAME money-movement
// integrations returns use (treasury /refunds + credit notes), so GL and fiscal behaviour
// stay consistent between the two flows.
package reversals

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	entposreversal "github.com/bengobox/pos-service/internal/ent/posreversal"
	entschema "github.com/bengobox/pos-service/internal/ent/schema"
	"github.com/bengobox/pos-service/internal/modules/documents"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/orders"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// Step names — one per integrated service touch-point.
const (
	StepPOSTotals       = "pos_totals"
	StepInventory       = "inventory_consumption"
	StepTreasuryGL      = "treasury_gl"
	StepEtimsCreditNote = "etims_credit_note"
)

// Step statuses.
const (
	StatusPending   = "pending"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusSkipped   = "skipped"
)

// Service orchestrates cross-service reversal execution.
type Service struct {
	log             *zap.Logger
	client          *ent.Client
	orderSvc        *orders.Service
	treasuryClient  *treasury.Client
	inventoryClient *inventory.Client
	auditSvc        *audit.Service
	// seq, when wired, mints reversal numbers through the tenant-configurable document sequence
	// (numeric by default), falling back to the legacy REV-<epoch-ms> format.
	seq *documents.SequenceService
}

// NewService wires the reversal orchestrator.
func NewService(log *zap.Logger, client *ent.Client, orderSvc *orders.Service, treasuryClient *treasury.Client, inventoryClient *inventory.Client) *Service {
	return &Service{
		log:             log.Named("reversals"),
		client:          client,
		orderSvc:        orderSvc,
		treasuryClient:  treasuryClient,
		inventoryClient: inventoryClient,
	}
}

// SetAuditService wires the centralized audit trail.
func (s *Service) SetAuditService(a *audit.Service) { s.auditSvc = a }

// WithSequence wires the document-sequence service so reversal numbers are minted through the
// tenant's pos_reversal sequence (numeric by default), falling back to the legacy format.
func (s *Service) WithSequence(seq *documents.SequenceService) *Service {
	s.seq = seq
	return s
}

// Client exposes the ent client for the handler's read-only list/detail queries.
func (s *Service) Client() *ent.Client { return s.client }

// LineSelection selects (part of) one order line for a partial reversal.
type LineSelection struct {
	LineID   uuid.UUID `json:"line_id"`
	Quantity float64   `json:"quantity,omitempty"` // 0 => whole line
}

// CreateRequest describes a reversal to execute.
type CreateRequest struct {
	OrderID        uuid.UUID       `json:"order_id"`
	Scope          string          `json:"scope"` // full | partial
	Lines          []LineSelection `json:"lines,omitempty"`
	Reason         string          `json:"reason"`
	RefundChannel  string          `json:"refund_channel,omitempty"` // default cash
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	// TenantSlug is the URL tenant segment, forwarded to treasury S2S calls (which key on slug).
	TenantSlug  string    `json:"-"`
	RequestedBy uuid.UUID `json:"-"`
}

// finalizedStatuses are order states a reversal may act on — the same set the line-void
// handler refuses (because GL/eTIMS already posted), which is exactly this tool's job.
var finalizedStatuses = map[string]bool{"completed": true, "paid": true, "closed": true}

// Execute validates, records and runs a reversal. The returned POSReversal carries the
// per-step outcome; overall status is completed / partial_failure / failed.
func (s *Service) Execute(ctx context.Context, tenantID uuid.UUID, req CreateRequest) (*ent.POSReversal, error) {
	if req.Reason == "" {
		return nil, fmt.Errorf("reason is required")
	}
	if req.Scope != "full" && req.Scope != "partial" {
		return nil, fmt.Errorf("scope must be full or partial")
	}

	// Idempotent replay of the whole reversal (client retry safety).
	if req.IdempotencyKey != "" {
		if existing, err := s.client.POSReversal.Query().
			Where(entposreversal.IdempotencyKeyEQ(req.IdempotencyKey)).
			First(ctx); err == nil {
			return existing, nil
		}
	}

	order, err := s.client.POSOrder.Query().
		Where(entposorder.ID(req.OrderID), entposorder.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("order not found")
	}
	if !finalizedStatuses[order.Status] {
		return nil, fmt.Errorf("only a finalized sale (completed/paid/closed) can be reversed — this order is %q; open bills use the normal line-void flow", order.Status)
	}

	lines, err := s.client.POSOrderLine.Query().
		Where(entposorderline.OrderID(order.ID)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load order lines: %w", err)
	}

	revLines, amount, tax, err := s.resolveLines(order, lines, req)
	if err != nil {
		return nil, err
	}

	// COGS from the catalog cost source — symmetric with the sale-time COGS posting and the
	// returns flow, so treasury never reverses cost that was never posted.
	cost := s.catalogCost(ctx, tenantID, revLines)

	steps := []entschema.ReversalStepJSON{
		{Step: StepPOSTotals, Service: "pos", Status: StatusPending},
		{Step: StepInventory, Service: "inventory", Status: StatusPending},
		{Step: StepTreasuryGL, Service: "treasury", Status: StatusPending},
		{Step: StepEtimsCreditNote, Service: "treasury", Status: StatusPending},
	}

	// Reversal number: numeric-by-default via the tenant's pos_reversal document sequence,
	// falling back to the legacy REV-<epoch-ms> format when the sequence is unwired/errors.
	reversalNumber := fmt.Sprintf("REV-%d", time.Now().UnixMilli())
	if s.seq != nil {
		if n, err := s.seq.GenerateNumber(ctx, tenantID, documents.DocTypePosReversal); err == nil && n != "" {
			reversalNumber = n
		}
	}

	create := s.client.POSReversal.Create().
		SetTenantID(tenantID).
		SetOrderID(order.ID).
		SetOrderNumber(order.OrderNumber).
		SetReversalNumber(reversalNumber).
		SetScope(entposreversal.Scope(req.Scope)).
		SetStatus(entposreversal.StatusPending).
		SetReason(req.Reason).
		SetRefundChannel(coalesce(req.RefundChannel, "cash")).
		SetLines(revLines).
		SetAmount(round2(amount)).
		SetTaxAmount(round2(tax)).
		SetCostAmount(round2(cost)).
		SetSteps(steps).
		SetRequestedBy(req.RequestedBy)
	if req.IdempotencyKey != "" {
		create.SetIdempotencyKey(req.IdempotencyKey)
	}
	rev, err := create.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create reversal record: %w", err)
	}

	rev = s.runSteps(ctx, rev, req.TenantSlug)
	s.audit(ctx, rev)
	return rev, nil
}

// Retry re-runs the failed/pending steps of an existing reversal. Every step is idempotent
// downstream (soft-void guards, inventory idempotency key, treasury reference-id idempotency),
// so re-running a completed step is impossible and re-running a failed one is safe.
func (s *Service) Retry(ctx context.Context, tenantID, reversalID uuid.UUID, tenantSlug string) (*ent.POSReversal, error) {
	rev, err := s.client.POSReversal.Query().
		Where(entposreversal.ID(reversalID), entposreversal.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("reversal not found")
	}
	if rev.Status == entposreversal.StatusCompleted {
		return rev, nil
	}
	rev = s.runSteps(ctx, rev, tenantSlug)
	return rev, nil
}

// resolveLines expands the request into concrete reversal lines with money amounts.
func (s *Service) resolveLines(order *ent.POSOrder, lines []*ent.POSOrderLine, req CreateRequest) ([]entschema.ReversalLineJSON, float64, float64, error) {
	byID := make(map[uuid.UUID]*ent.POSOrderLine, len(lines))
	for _, l := range lines {
		byID[l.ID] = l
	}

	toJSON := func(l *ent.POSOrderLine, qty float64) entschema.ReversalLineJSON {
		ratio := 1.0
		if l.Quantity > 0 {
			ratio = qty / l.Quantity
		}
		taxAmt := 0.0
		if l.TaxAmount != nil {
			taxAmt = *l.TaxAmount * ratio
		}
		return entschema.ReversalLineJSON{
			LineID:     l.ID,
			SKU:        l.Sku,
			Name:       l.Name,
			Quantity:   qty,
			OfQuantity: l.Quantity,
			Amount:     round2(l.TotalPrice * ratio),
			TaxAmount:  round2(taxAmt),
		}
	}

	var out []entschema.ReversalLineJSON
	var amount, tax float64

	if req.Scope == "full" {
		for _, l := range lines {
			if l.VoidedQty != nil {
				continue // already voided (e.g. by an earlier partial reversal / line void)
			}
			j := toJSON(l, l.Quantity)
			out = append(out, j)
			amount += j.Amount
			tax += j.TaxAmount
		}
		if len(out) == 0 {
			return nil, 0, 0, fmt.Errorf("order has no active (un-voided) lines left to reverse")
		}
		return out, amount, tax, nil
	}

	if len(req.Lines) == 0 {
		return nil, 0, 0, fmt.Errorf("partial reversal requires at least one line")
	}
	for _, sel := range req.Lines {
		l, ok := byID[sel.LineID]
		if !ok {
			return nil, 0, 0, fmt.Errorf("line %s does not belong to this order", sel.LineID)
		}
		if l.VoidedQty != nil {
			return nil, 0, 0, fmt.Errorf("line %q is already voided", l.Name)
		}
		qty := sel.Quantity
		if qty <= 0 || qty > l.Quantity {
			qty = l.Quantity
		}
		j := toJSON(l, qty)
		out = append(out, j)
		amount += j.Amount
		tax += j.TaxAmount
	}
	return out, amount, tax, nil
}

// catalogCost sums the catalog COGS for the reversal lines (see orders.CatalogCostBySKU).
func (s *Service) catalogCost(ctx context.Context, tenantID uuid.UUID, revLines []entschema.ReversalLineJSON) float64 {
	skus := make([]string, 0, len(revLines))
	for _, l := range revLines {
		if l.SKU != "" {
			skus = append(skus, l.SKU)
		}
	}
	costBySKU := orders.CatalogCostBySKU(ctx, s.client, tenantID, skus)
	var total float64
	for _, l := range revLines {
		total += costBySKU[l.SKU] * l.Quantity
	}
	return total
}

// audit records the reversal in the central audit trail (platform-owner money-reversing action).
func (s *Service) audit(ctx context.Context, rev *ent.POSReversal) {
	if s.auditSvc == nil {
		return
	}
	amt := rev.Amount
	s.auditSvc.Record(ctx, audit.Entry{
		TenantID:    rev.TenantID,
		ActorUserID: rev.RequestedBy,
		Action:      "order.reversal",
		EntityType:  "pos_reversal",
		EntityID:    rev.ID.String(),
		Reason:      rev.Reason,
		Amount:      &amt,
		After: map[string]any{
			"reversal_number": rev.ReversalNumber,
			"order_number":    rev.OrderNumber,
			"scope":           string(rev.Scope),
			"status":          string(rev.Status),
		},
	})
}

func coalesce(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
