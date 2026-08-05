// Package salecorrection holds loyalty/commission clawback logic shared by every flow that
// reverses value on a finalized POS sale — the reversals engine (non-fiscalized in-place
// reductions, the platform-owner Txn Reversal tool, saledelete's fiscalized branch) and the
// returns engine's admin Edit-Sale auto-complete path (fiscalized reductions). Extracted out
// of internal/modules/reversals so routing a fiscalized reduction through a POSReturn instead
// of a POSReversal doesn't silently regress this clawback — before this extraction it only
// existed inside the reversals package.
package salecorrection

import (
	"context"
	"fmt"
	"math"
	"strings"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	entcommissionrecord "github.com/bengobox/pos-service/internal/ent/commissionrecord"
	entla "github.com/bengobox/pos-service/internal/ent/loyaltyaccount"
	entloyaltytransaction "github.com/bengobox/pos-service/internal/ent/loyaltytransaction"
)

// ReverseLoyalty claws back a prorated share of the points an order earned (auto-earn +
// referral bonus, both keyed on order_id), floored at 0 so a balance already spent elsewhere
// never goes negative. Records a "reversal"-typed LoyaltyTransaction for audit. reasonRef is
// the correcting record's own reference (a reversal number or return number) recorded in the
// transaction note. ratio is 1.0 for a full reversal/return, or the reversed/returned
// fraction of the order's total for a partial one.
func ReverseLoyalty(ctx context.Context, log *zap.Logger, client *ent.Client, tenantID, orderID uuid.UUID, ratio float64, reasonRef string) string {
	txns, err := client.LoyaltyTransaction.Query().
		Where(
			entloyaltytransaction.TenantID(tenantID),
			entloyaltytransaction.OrderID(orderID),
			entloyaltytransaction.TypeFieldIn("earn", "referral"),
		).
		All(ctx)
	if err != nil || len(txns) == 0 {
		return ""
	}

	byAccount := map[uuid.UUID]int{}
	for _, t := range txns {
		if t.Points > 0 {
			byAccount[t.AccountID] += t.Points
		}
	}

	var parts []string
	for accountID, earned := range byAccount {
		clawback := int(math.Round(float64(earned) * ratio))
		if clawback <= 0 {
			continue
		}
		// Locked read-modify-write: two concurrent corrections against the same account
		// (e.g. a Retry racing a fresh edit) would otherwise both load the same
		// acc.PointsBalance here and both compute/save from it — a lost update that claws
		// back points twice. ForUpdate() makes the second caller wait for the first commit,
		// so it re-derives from the already-decremented balance.
		tx, txErr := client.Tx(ctx)
		if txErr != nil {
			log.Warn("clawback: loyalty tx start failed", zap.String("account_id", accountID.String()), zap.Error(txErr))
			continue
		}
		acc, err := tx.LoyaltyAccount.Query().Where(entla.ID(accountID), entla.TenantID(tenantID)).ForUpdate().Only(ctx)
		if err != nil {
			_ = tx.Rollback()
			log.Warn("clawback: loyalty account lookup failed", zap.String("account_id", accountID.String()), zap.Error(err))
			continue
		}
		actual := clawback
		if actual > acc.PointsBalance {
			actual = acc.PointsBalance // can't claw into points already redeemed elsewhere
		}
		if actual <= 0 {
			_ = tx.Rollback()
			continue
		}
		newBalance := acc.PointsBalance - actual
		newLifetime := acc.LifetimePoints - actual
		if newLifetime < 0 {
			newLifetime = 0
		}
		if _, err := tx.LoyaltyAccount.UpdateOneID(accountID).SetPointsBalance(newBalance).SetLifetimePoints(newLifetime).Save(ctx); err != nil {
			_ = tx.Rollback()
			log.Warn("clawback: loyalty balance update failed", zap.String("account_id", accountID.String()), zap.Error(err))
			continue
		}
		if _, err := tx.LoyaltyTransaction.Create().
			SetTenantID(tenantID).
			SetAccountID(accountID).
			SetOrderID(orderID).
			SetTypeField("reversal").
			SetPoints(-actual).
			SetBalanceAfter(newBalance).
			SetNotes(fmt.Sprintf("Correction %s: clawed back %d of %d earned point(s)", reasonRef, actual, earned)).
			Save(ctx); err != nil {
			log.Warn("clawback: loyalty transaction write failed", zap.Error(err))
		}
		if err := tx.Commit(); err != nil {
			log.Warn("clawback: loyalty tx commit failed", zap.String("account_id", accountID.String()), zap.Error(err))
			continue
		}
		parts = append(parts, fmt.Sprintf("%d loyalty pt(s) clawed back", actual))
	}
	return strings.Join(parts, ", ")
}

// ReverseCommission voids the still-pending CommissionRecord(s) tied to an order (or, when
// skus is non-empty, only those matching the reversed/returned lines' SKUs — CommissionRecord.
// order_line_id is never populated by the auto-create path, so SKU is the reliable
// correlation key). Already-paid commissions are left untouched (can't claw back a payout
// automatically) and counted separately so the detail is honest about what wasn't recovered.
func ReverseCommission(ctx context.Context, log *zap.Logger, client *ent.Client, tenantID, orderID uuid.UUID, skus map[string]bool, reasonRef string) string {
	recs, err := client.CommissionRecord.Query().
		Where(entcommissionrecord.TenantID(tenantID), entcommissionrecord.OrderID(orderID)).
		All(ctx)
	if err != nil || len(recs) == 0 {
		return ""
	}

	inScope := func(r *ent.CommissionRecord) bool {
		return len(skus) == 0 || skus[r.ServiceSku]
	}

	voided, paidSkipped := 0, 0
	for _, r := range recs {
		if !inScope(r) {
			continue
		}
		switch r.Status {
		case "pending":
			note := r.Notes
			if note != "" {
				note += " | "
			}
			note += fmt.Sprintf("voided by correction %s", reasonRef)
			if _, err := r.Update().SetStatus("voided").SetNotes(note).Save(ctx); err != nil {
				log.Warn("clawback: commission void failed", zap.String("commission_id", r.ID.String()), zap.Error(err))
				continue
			}
			voided++
		case "paid":
			paidSkipped++
		}
	}

	var parts []string
	if voided > 0 {
		parts = append(parts, fmt.Sprintf("%d commission record(s) voided", voided))
	}
	if paidSkipped > 0 {
		parts = append(parts, fmt.Sprintf("%d already-paid commission(s) left untouched — recover manually", paidSkipped))
	}
	return strings.Join(parts, "; ")
}
