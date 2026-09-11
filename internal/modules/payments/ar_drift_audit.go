package payments

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/syncfailure"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// arDriftEntityType tags SyncFailure rows written by ARDriftAuditScheduler — a pre-existing,
// previously-unused generic entity (tenant_id/entity_type/external_id/error_message/payload/
// is_resolved) that already fits this exactly; reused here instead of adding a new ent schema/
// migration for the same shape.
const arDriftEntityType = "ar_drift"

// arDriftTolerance absorbs float rounding noise — anything smaller is not a real discrepancy.
const arDriftTolerance = 1.0

// ============================================================================================
// Fleet-wide POS-vs-treasury AR drift audit.
//
// Every fix so far in this bug family (see [[boi-treasury-payment-reflection-gap-audit-2026-09-09]],
// [[boi-mixed-edit-reconcile-race-2026-09-10]] and the many prior sessions referenced from those)
// closed ONE specific mechanism by which POS's own record of what a credit customer owes can drift
// from treasury's — a silently-failed sync call, a race between two async treasury writes, a
// resolver bug, a data-entry mistake. New mechanisms keep surfacing because POS and treasury are
// two independently-writable systems kept in sync by a patchwork of best-effort, log-and-continue
// event handlers and S2S calls with NO retry and NO detection layer: a fix closes the gap it was
// aimed at, but nothing ever re-checks whether some OTHER gap quietly let a customer's real balance
// drift out from under POS. That's why a real customer's statement can go wrong for weeks before
// anyone notices — nothing was watching.
//
// This scheduler is that watch. It does NOT replace fixing root causes when found (see the two
// memory files above for two just-fixed examples) — it is the safety net for whatever the NEXT
// undiscovered mechanism turns out to be. Runs fleet-wide (every tenant, matching
// scheduler.LayawayReminderScheduler's own convention), on a plain interval:
//   - SAFE direction (POS shows MORE owed than treasury): reduce-only self-heal via the EXISTING
//     ReconcileCustomerOrders (payments/ar_reconcile.go) — identical guarantee to the event-driven
//     path, just a backstop for the case where the triggering event itself never arrived.
//   - UNSAFE direction (POS shows LESS owed than treasury): never auto-corrected — the surplus
//     could be legitimate non-POS AR (an invoice, an opening balance) or, as confirmed live
//     2026-09-10 (boi-enterprises, MR SAMMON MALABA / MR SOLOMON NG'ETHE), a genuine customer
//     mis-attribution that needs a human decision. Durably flagged via SyncFailure instead —
//     survives past log retention, queryable later, doesn't silently sit for months.
// ============================================================================================

// ARDriftAuditScheduler periodically compares each on-account customer's POS-open total against
// treasury's live balance, fleet-wide. See the package doc comment above for the full rationale.
type ARDriftAuditScheduler struct {
	log *zap.Logger
	svc *Service
}

// NewARDriftAuditScheduler creates the scheduler. svc must have its treasury client wired
// (SetTreasuryClient) for the live-balance checks to do anything — a nil client makes every pass
// a safe no-op (matches this codebase's established fail-open posture for optional wiring).
func NewARDriftAuditScheduler(log *zap.Logger, svc *Service) *ARDriftAuditScheduler {
	return &ARDriftAuditScheduler{log: log.Named("pos.ar_drift_audit"), svc: svc}
}

// arDriftInterval is how often a full fleet-wide pass runs. Frequent enough that a real drift
// surfaces within hours, not months; infrequent enough not to hammer treasury with per-customer
// GetCreditTerms calls across every tenant.
const arDriftInterval = 6 * time.Hour

// Start launches the background ticker. Call in a goroutine from main (mirrors
// scheduler.LayawayReminderScheduler.Start exactly).
func (s *ARDriftAuditScheduler) Start(ctx context.Context) {
	ticker := time.NewTicker(arDriftInterval)
	defer ticker.Stop()
	// Stagger the very first run so it doesn't compete with every other startup task.
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Minute):
		s.run(ctx)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.run(ctx)
		}
	}
}

// custGroupKey identifies one credit customer within one tenant — phone is either a real phone
// number or the "staff:<id>" synthetic key (staffCreditFromOrderParty's convention).
type custGroupKey struct {
	tenantID uuid.UUID
	phone    string
}

type custGroup struct {
	name string
	due  float64
}

// run performs one fleet-wide pass: load every non-terminal on-account order across every tenant
// ONCE, group by (tenant, customer), sum each group's owed amount via the SAME formula every
// other read path uses (orders.ComputeSettlement + completedReturnsTotal — never reimplemented),
// then compare each group's total against treasury.
func (s *ARDriftAuditScheduler) run(ctx context.Context) {
	if s.svc == nil || s.svc.treasuryClient == nil {
		return
	}
	candidates, err := s.svc.client.POSOrder.Query().
		Where(posorder.StatusNotIn(orders.StatusVoided, orders.StatusCancelled, orders.StatusRefunded, orders.StatusDraft)).
		All(ctx)
	if err != nil {
		s.log.Error("ar drift audit: load candidate orders failed", zap.Error(err))
		return
	}

	groups := map[custGroupKey]*custGroup{}
	for _, o := range candidates {
		if !orders.IsOnAccount(o.Metadata) {
			continue
		}
		phone := ""
		if o.CustomerPhone != nil {
			phone = strings.TrimSpace(*o.CustomerPhone)
		}
		if phone == "" {
			if staffID, _, isStaff := staffCreditFromOrderParty(o); isStaff {
				phone = "staff:" + staffID.String()
			} else {
				continue // no customer key at all — nothing to reconcile against (matches
				// RecordSettledSale's own no-op rule for a true walk-in with no identity).
			}
		}
		cr, rerr := s.svc.completedReturnsTotal(ctx, o.ID)
		if rerr != nil {
			s.log.Warn("ar drift audit: completed-returns lookup failed", zap.String("order", o.OrderNumber), zap.Error(rerr))
		}
		due := orders.ComputeSettlement(o, cr).AmountDue
		if due <= 0.01 {
			continue
		}
		key := custGroupKey{tenantID: o.TenantID, phone: phone}
		g, ok := groups[key]
		if !ok {
			name := ""
			if o.CustomerName != nil {
				name = *o.CustomerName
			}
			g = &custGroup{name: name}
			groups[key] = g
		}
		g.due += due
	}

	var flagged, healed int
	for key, g := range groups {
		s.auditOne(ctx, key.tenantID, key.phone, g.name, round2(g.due), &flagged, &healed)
	}
	if flagged > 0 || healed > 0 {
		s.log.Info("ar drift audit pass complete",
			zap.Int("customers_checked", len(groups)),
			zap.Int("auto_healed", healed),
			zap.Int("flagged_for_review", flagged))
	}
}

// auditOne compares one customer's POS-open total against treasury's live balance and acts per
// the package doc comment's two-direction rule.
func (s *ARDriftAuditScheduler) auditOne(ctx context.Context, tenantID uuid.UUID, phone, name string, posOpen float64, flagged, healed *int) {
	isStaff := strings.HasPrefix(phone, "staff:")
	crmContactID := ""
	if !isStaff {
		crmContactID = s.svc.ResolveCrmContactID(ctx, tenantID, phone)
	}
	key := crmContactID
	if key == "" {
		key = phone
	}
	terms, err := s.svc.treasuryClient.GetCreditTerms(ctx, tenantID.String(), key)
	if err != nil {
		s.log.Warn("ar drift audit: live balance fetch failed",
			zap.String("tenant_id", tenantID.String()), zap.String("customer_key", key), zap.Error(err))
		return
	}
	treasuryBal, perr := strconv.ParseFloat(terms.OutstandingDebit, 64)
	if perr != nil {
		return
	}

	diff := round2(posOpen - treasuryBal)
	if diff <= arDriftTolerance && diff >= -arDriftTolerance {
		// In agreement now — close out any stale flag from a drift that has since resolved
		// itself (a human fixed it, or a later event healed it), so the review queue reflects
		// reality instead of accumulating noise.
		s.autoResolveIfMatched(ctx, tenantID, key)
		return
	}
	if diff > arDriftTolerance {
		// Safe direction — see package doc comment. Reuses ReconcileCustomerOrders exactly as
		// the event-driven path does; this call is just a backstop for a lost/never-fired event.
		if _, rerr := s.svc.ReconcileCustomerOrders(ctx, ReconcileParams{
			TenantID: tenantID, CrmContactID: crmContactID, CustomerIdentifier: phone,
			TargetOutstanding: treasuryBal, Reference: "ar-drift-audit",
		}); rerr != nil {
			s.log.Warn("ar drift audit: auto-heal failed", zap.String("customer_key", key), zap.Error(rerr))
			return
		}
		*healed++
		return
	}
	// Unsafe direction — never auto-corrected. Durably flag instead.
	s.recordDrift(ctx, tenantID, key, name, posOpen, treasuryBal, diff)
	*flagged++
}

// recordDrift upserts one unresolved SyncFailure row per (tenant, customer key) — refreshes the
// figures on an already-open flag rather than spamming a new row every pass for the same ongoing
// drift.
func (s *ARDriftAuditScheduler) recordDrift(ctx context.Context, tenantID uuid.UUID, key, name string, posOpen, treasuryBal, diff float64) {
	msg := "POS shows " + strconv.FormatFloat(posOpen, 'f', 2, 64) + " owed but treasury shows " +
		strconv.FormatFloat(treasuryBal, 'f', 2, 64) + " (diff " + strconv.FormatFloat(diff, 'f', 2, 64) +
		") — needs manual review before any automatic correction"
	payload := map[string]any{
		"customer_name": name, "customer_key": key,
		"pos_open": posOpen, "treasury_balance": treasuryBal, "diff": diff,
		"checked_at": time.Now().Format(time.RFC3339),
	}
	existing, err := s.svc.client.SyncFailure.Query().
		Where(syncfailure.TenantID(tenantID), syncfailure.EntityType(arDriftEntityType),
			syncfailure.ExternalID(key), syncfailure.IsResolved(false)).
		First(ctx)
	if err == nil && existing != nil {
		if _, uerr := existing.Update().SetPayload(payload).SetErrorMessage(msg).SetOccurredAt(time.Now()).Save(ctx); uerr != nil {
			s.log.Warn("ar drift audit: failed to refresh existing flag", zap.Error(uerr))
		}
		return
	}
	if !ent.IsNotFound(err) && err != nil {
		s.log.Warn("ar drift audit: lookup existing flag failed", zap.Error(err))
	}
	if _, cerr := s.svc.client.SyncFailure.Create().
		SetTenantID(tenantID).SetEntityType(arDriftEntityType).SetExternalID(key).
		SetErrorMessage(msg).SetPayload(payload).Save(ctx); cerr != nil {
		s.log.Error("ar drift audit: failed to record drift flag", zap.Error(cerr))
	}
}

func (s *ARDriftAuditScheduler) autoResolveIfMatched(ctx context.Context, tenantID uuid.UUID, key string) {
	existing, err := s.svc.client.SyncFailure.Query().
		Where(syncfailure.TenantID(tenantID), syncfailure.EntityType(arDriftEntityType),
			syncfailure.ExternalID(key), syncfailure.IsResolved(false)).
		First(ctx)
	if err != nil || existing == nil {
		return
	}
	if _, uerr := existing.Update().SetIsResolved(true).Save(ctx); uerr != nil {
		s.log.Warn("ar drift audit: failed to auto-resolve stale flag", zap.Error(uerr))
	}
}

// ListARDriftFlags returns every unresolved ar_drift SyncFailure for a tenant, newest first — the
// read side of the scheduler above, exposed via PaymentHandler.ListARDriftFlags (platform-owner
// only).
func (s *Service) ListARDriftFlags(ctx context.Context, tenantID uuid.UUID) ([]*ent.SyncFailure, error) {
	return s.client.SyncFailure.Query().
		Where(syncfailure.TenantID(tenantID), syncfailure.EntityType(arDriftEntityType), syncfailure.IsResolved(false)).
		Order(ent.Desc(syncfailure.FieldOccurredAt)).
		All(ctx)
}
