// Package staffcredit owns the pos-api side of staff fund-from-salary: it turns a staff layaway /
// credit sale into a StaffPurchaseLink, pushes it to erp-api (which creates the payroll recoverable),
// books the treasury receivable, and pays down the local balance as erp reports recoveries.
package staffcredit

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/layawayplan"
	"github.com/bengobox/pos-service/internal/ent/staffmember"
	"github.com/bengobox/pos-service/internal/ent/staffpurchaselink"
	"github.com/bengobox/pos-service/internal/platform/erp"
)

// FeatureCode is the premium entitlement gating staff fund-from-salary.
const FeatureCode = "staff_fund_from_salary"

// Publisher + FeatureGate are the narrow interfaces this service needs, declared here to avoid an
// import cycle (the events package hosts the settlement subscriber; the subscriptions package
// imports events).
type Publisher interface {
	PublishStaffPurchaseCreated(ctx context.Context, tenantID uuid.UUID, data map[string]any) error
}

type FeatureGate interface {
	ConsumerHasFeature(ctx context.Context, tenantID, featureCode string) bool
}

// Service provisions + settles staff-credit links.
type Service struct {
	db        *ent.Client
	erp       *erp.Client
	publisher Publisher
	subs      FeatureGate
	log       *zap.Logger
}

func NewService(db *ent.Client, erpClient *erp.Client, publisher Publisher, subs FeatureGate, log *zap.Logger) *Service {
	return &Service{db: db, erp: erpClient, publisher: publisher, subs: subs, log: log.Named("staffcredit")}
}

// ProvisionInput describes a staff purchase to fund from salary.
type ProvisionInput struct {
	OutletID          *uuid.UUID
	StaffMemberID     uuid.UUID
	Origin            string // layaway | credit_sale
	LayawayPlanID     *uuid.UUID
	POSOrderID        *uuid.UUID
	Principal         decimal.Decimal
	InstallmentAmount decimal.Decimal
	InstallmentsTotal int
}

// Entitled reports whether the tenant may use staff fund-from-salary (premium feature).
func (s *Service) Entitled(ctx context.Context, tenantID uuid.UUID) bool {
	if s.subs == nil {
		return true
	}
	return s.subs.ConsumerHasFeature(ctx, tenantID.String(), FeatureCode)
}

// Provision creates the local link, books the treasury receivable, and pushes the recoverable to
// erp. Idempotent on the deterministic source_key. erp/transport failures are recorded on the link
// (sync_status=failed) for retry — they never fail the caller's sale.
func (s *Service) Provision(ctx context.Context, tenantID uuid.UUID, in ProvisionInput) (*ent.StaffPurchaseLink, error) {
	origin := in.Origin
	if origin != "layaway" && origin != "credit_sale" {
		origin = "credit_sale"
	}
	ref := ""
	switch {
	case in.LayawayPlanID != nil:
		ref = in.LayawayPlanID.String()
	case in.POSOrderID != nil:
		ref = in.POSOrderID.String()
	}
	sourceKey := fmt.Sprintf("pos:%s:%s", origin, ref)

	// Idempotency.
	if existing, err := s.db.StaffPurchaseLink.Query().
		Where(staffpurchaselink.TenantID(tenantID), staffpurchaselink.SourceKey(sourceKey)).
		Only(ctx); err == nil && existing != nil {
		return existing, nil
	}

	sm, err := s.db.StaffMember.Query().
		Where(staffmember.ID(in.StaffMemberID), staffmember.TenantID(tenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("staffcredit: staff member not found: %w", err)
	}

	c := s.db.StaffPurchaseLink.Create().
		SetTenantID(tenantID).
		SetStaffMemberID(sm.ID).
		SetUserID(sm.UserID).
		SetOrigin(staffpurchaselink.Origin(origin)).
		SetSourceKey(sourceKey).
		SetPrincipal(in.Principal).
		SetAmountSettled(decimal.Zero).
		SetOutstanding(in.Principal).
		SetSyncStatus("pending").
		SetStatus("active")
	if in.OutletID != nil {
		c.SetOutletID(*in.OutletID)
	}
	if in.LayawayPlanID != nil {
		c.SetLayawayPlanID(*in.LayawayPlanID)
	}
	if in.POSOrderID != nil {
		c.SetPosOrderID(*in.POSOrderID)
	}
	link, err := c.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("staffcredit: create link: %w", err)
	}

	// Book the employee receivable in treasury (Dr Staff Receivable / Cr Revenue).
	if s.publisher != nil {
		_ = s.publisher.PublishStaffPurchaseCreated(ctx, tenantID, map[string]any{
			"source_key":      sourceKey,
			"origin":          origin,
			"pos_reference":   ref,
			"staff_member_id": sm.ID.String(),
			"user_id":         sm.UserID.String(),
			"employee_name":   sm.Name,
			"principal":       in.Principal.StringFixed(2),
		})
	}

	// Push to erp (best-effort; record the outcome on the link).
	if s.erp != nil && s.erp.Enabled() {
		resp, rerr := s.erp.CreateStaffPurchase(ctx, tenantID.String(), erp.StaffPurchaseRequest{
			AuthUserID:        sm.UserID.String(),
			Origin:            origin,
			POSReference:      ref,
			SourceKey:         sourceKey,
			Principal:         in.Principal.StringFixed(2),
			InstallmentAmount: in.InstallmentAmount.StringFixed(2),
			InstallmentsTotal: in.InstallmentsTotal,
			EmployeeName:      sm.Name,
		})
		if rerr != nil {
			s.log.Warn("erp staff-purchase sync failed (link left pending/failed for retry)", zap.String("source_key", sourceKey), zap.Error(rerr))
			link, _ = link.Update().SetSyncStatus("failed").SetSyncError(rerr.Error()).Save(ctx)
			return link, nil
		}
		upd := link.Update().SetSyncStatus("synced")
		if resp != nil && resp.ID != "" {
			if erpID, perr := uuid.Parse(resp.ID); perr == nil {
				upd = upd.SetErpPurchaseID(erpID)
			}
		}
		link, _ = upd.Save(ctx)
	}
	return link, nil
}

// Settle applies an erp.staff_purchase.recovered / recovery_reversed event. It sets the link's
// cumulative amount_settled to erp's reported total (self-healing regardless of order) and adjusts
// the linked layaway balance by the delta (positive for recovery, negative for reversal). Idempotent
// via the consumer's IdempotencyStore; JetStream preserves per-purchase ordering.
func (s *Service) Settle(ctx context.Context, tenantID uuid.UUID, sourceKey string, totalRecovered decimal.Decimal, settled bool) error {
	link, err := s.db.StaffPurchaseLink.Query().
		Where(staffpurchaselink.TenantID(tenantID), staffpurchaselink.SourceKey(sourceKey)).
		Only(ctx)
	if err != nil {
		return fmt.Errorf("staffcredit: link not found for %s: %w", sourceKey, err)
	}

	delta := totalRecovered.Sub(link.AmountSettled)
	newOutstanding := link.Principal.Sub(totalRecovered)
	if newOutstanding.IsNegative() {
		newOutstanding = decimal.Zero
	}

	upd := s.db.StaffPurchaseLink.UpdateOne(link).
		SetAmountSettled(totalRecovered).
		SetOutstanding(newOutstanding)
	if settled || newOutstanding.IsZero() {
		upd = upd.SetStatus("settled")
	} else {
		upd = upd.SetStatus("active") // a reversal re-opens a previously-settled link
	}
	if _, err := upd.Save(ctx); err != nil {
		return err
	}

	// Adjust the linked layaway balance (credit-sale AR clears via the treasury GL reclass instead).
	if link.LayawayPlanID != nil && !delta.IsZero() {
		if plan, perr := s.db.LayawayPlan.Query().
			Where(layawayplan.ID(*link.LayawayPlanID), layawayplan.TenantID(tenantID)).
			Only(ctx); perr == nil && plan != nil {
			paid := plan.PaidAmount.Add(delta)
			if paid.IsNegative() {
				paid = decimal.Zero
			}
			remaining := plan.TotalAmount.Sub(paid)
			if remaining.IsNegative() {
				remaining = decimal.Zero
			}
			pu := s.db.LayawayPlan.UpdateOne(plan).SetPaidAmount(paid).SetRemainingAmount(remaining)
			if remaining.IsZero() {
				pu = pu.SetStatus("completed")
			} else if plan.Status == "completed" {
				pu = pu.SetStatus("active") // reversal re-opened it
			}
			if _, err := pu.Save(ctx); err != nil {
				s.log.Warn("staffcredit: layaway paydown failed", zap.Error(err))
			}
		}
	}
	return nil
}
