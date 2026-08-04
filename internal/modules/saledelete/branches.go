package saledelete

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	entbartabevent "github.com/bengobox/pos-service/internal/ent/bartabevent"
	entbillsplit "github.com/bengobox/pos-service/internal/ent/billsplit"
	entcommissionrecord "github.com/bengobox/pos-service/internal/ent/commissionrecord"
	entkdssyncfailure "github.com/bengobox/pos-service/internal/ent/kdssyncfailure"
	entkdsticket "github.com/bengobox/pos-service/internal/ent/kdsticket"
	entloyaltytransaction "github.com/bengobox/pos-service/internal/ent/loyaltytransaction"
	entorderlink "github.com/bengobox/pos-service/internal/ent/orderlink"
	entordervoidcode "github.com/bengobox/pos-service/internal/ent/ordervoidcode"
	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	entpossaleshred "github.com/bengobox/pos-service/internal/ent/possaleshred"
	entprintjob "github.com/bengobox/pos-service/internal/ent/printjob"
	entpromotionapplication "github.com/bengobox/pos-service/internal/ent/promotionapplication"
	entschema "github.com/bengobox/pos-service/internal/ent/schema"
	entstockconsumptionevent "github.com/bengobox/pos-service/internal/ent/stockconsumptionevent"
	enttableassignment "github.com/bengobox/pos-service/internal/ent/tableassignment"
	"github.com/bengobox/pos-service/internal/modules/inventory"
	"github.com/bengobox/pos-service/internal/modules/reversals"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// deleteFiscalized handles a sale that has a KRA eTIMS-signed treasury invoice: "sales
// transmitted to KRA cannot be deleted" — reverse it via the SAME engine the platform-owner Txn
// Reversal tool uses (unmodified), then soft-mark the order deleted. No SQL row is removed.
func (s *Service) deleteFiscalized(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder, req Request, invoiceID string) (*Result, error) {
	rev, err := s.reversalSvc.Execute(ctx, tenantID, reversals.CreateRequest{
		OrderID:     order.ID,
		Scope:       "full",
		Reason:      fmt.Sprintf("Delete Sale: %s", req.Reason),
		RequestedBy: req.RequestedBy,
		TenantSlug:  req.TenantSlug,
	})
	if err != nil {
		return nil, fmt.Errorf("reverse fiscalised sale: %w", err)
	}

	md := map[string]any{}
	for k, v := range order.Metadata {
		md[k] = v
	}
	md["deletion"] = map[string]any{
		"deleted_by":     req.RequestedBy.String(),
		"delete_reason":  req.Reason,
		"deleted_at":     time.Now().UTC().Format(time.RFC3339),
		"invoice_id":     invoiceID,
		"reversal_id":    rev.ID.String(),
		"reversal_number": rev.ReversalNumber,
	}
	now := time.Now()
	if _, err := order.Update().SetDeletedAt(now).SetMetadata(md).Save(ctx); err != nil {
		return nil, fmt.Errorf("mark order deleted: %w", err)
	}

	s.audit(ctx, tenantID, req.RequestedBy, order.OrderNumber, order.ID, req.Reason, "fiscalized", nil)

	return &Result{
		Branch:  "fiscalized",
		OrderID: order.ID,
		Steps:   rev.Steps,
	}, nil
}

// deleteNonFiscalized handles the common cash-sale case: snapshot everything into a permanent
// POSSaleShred record, restore inventory stock, hard-delete the treasury ledger entries (genuine
// deletion — see treasury.Client.ShredLedger), then hard-delete pos-api's own rows.
func (s *Service) deleteNonFiscalized(ctx context.Context, tenantID uuid.UUID, order *ent.POSOrder, req Request) (*Result, error) {
	lines, err := s.client.POSOrderLine.Query().Where(entposorderline.OrderID(order.ID)).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load lines: %w", err)
	}
	payments, err := s.client.POSPayment.Query().Where(entpospayment.OrderID(order.ID)).All(ctx)
	if err != nil {
		return nil, fmt.Errorf("load payments: %w", err)
	}

	steps := []entschema.ReversalStepJSON{
		nowStep(StepInventory, "inventory", StatusSkipped, "pending", ""),
		nowStep(StepTreasuryLedger, "treasury", StatusSkipped, "pending", ""),
		nowStep(StepARWriteoff, "treasury", StatusSkipped, "pending", ""),
		nowStep(StepPOSHardDelete, "pos", StatusSkipped, "pending", ""),
	}

	shred, err := s.client.POSSaleShred.Create().
		SetTenantID(tenantID).
		SetOrderID(order.ID).
		SetOrderNumber(order.OrderNumber).
		SetReason(req.Reason).
		SetStatus(entpossaleshred.StatusPending).
		SetSnapshot(snapshotOrder(order, lines, payments)).
		SetSteps(steps).
		SetRequestedBy(req.RequestedBy).
		Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create shred record: %w", err)
	}

	// Step: restore inventory stock (same idempotent S2S call the reversal engine uses — empty
	// Items reverses the order's ENTIRE recorded consumption).
	if s.inventoryClient != nil {
		resp, ierr := s.inventoryClient.ReverseConsumption(ctx, tenantID.String(), inventory.ReverseConsumptionRequest{
			OrderID:        order.ID.String(),
			Reason:         fmt.Sprintf("Delete Sale (shred): %s", req.Reason),
			IdempotencyKey: "pos-shred-" + shred.ID.String(),
		})
		if ierr != nil {
			steps[0] = nowStep(StepInventory, "inventory", StatusFailed, ierr.Error(), "")
			shred = s.persistShredSteps(ctx, shred, steps, "")
			return s.failResult(shred, order.ID), fmt.Errorf("restore inventory: %w", ierr)
		}
		detail := fmt.Sprintf("%d ingredient line(s) reversed", len(resp.Ingredients))
		if resp.AlreadyProcessed {
			detail = "already reversed (idempotent replay)"
		}
		steps[0] = nowStep(StepInventory, "inventory", StatusCompleted, detail, resp.ID)
	} else {
		steps[0] = nowStep(StepInventory, "inventory", StatusSkipped, "inventory client not configured", "")
	}
	shred = s.persistShredSteps(ctx, shred, steps, "")

	// Step: hard-delete the treasury ledger entries.
	if s.treasuryClient != nil {
		resp, terr := s.treasuryClient.ShredLedger(ctx, req.TenantSlug, order.ID.String())
		if terr != nil {
			steps[1] = nowStep(StepTreasuryLedger, "treasury", StatusFailed, terr.Error(), "")
			shred = s.persistShredSteps(ctx, shred, steps, "")
			return s.failResult(shred, order.ID), fmt.Errorf("shred treasury ledger: %w", terr)
		}
		steps[1] = nowStep(StepTreasuryLedger, "treasury", StatusCompleted,
			fmt.Sprintf("%d journal entr(y/ies) deleted", resp.EntriesDeleted), "")
	} else {
		steps[1] = nowStep(StepTreasuryLedger, "treasury", StatusSkipped, "treasury client not configured", "")
	}
	shred = s.persistShredSteps(ctx, shred, steps, "")

	// Step: reduce the customer's AR when this sale was settled on account — the gap this fix
	// closes. The ledger step above HARD-DELETES the sale's GL entries (never reverses them), so
	// there is nothing left to post a reversal against; only the operational CustomerBalance row
	// needs correcting. Outstanding amount is simply total-paid (never returns-netted: DeleteSale
	// already refuses any order with return/refund/reversal history before either branch runs).
	if s.reversalSvc != nil && s.treasuryClient != nil && s.reversalSvc.OrderSettledOnAccount(ctx, tenantID, order.ID) {
		outstanding := order.TotalAmount - order.PaidTotal
		if outstanding > 0.009 {
			crmContactID, _, customerPhone := s.reversalSvc.ResolveOrderCustomer(ctx, tenantID, order.ID)
			if crmContactID == "" && customerPhone == "" {
				steps[2] = nowStep(StepARWriteoff, "treasury", StatusFailed,
					fmt.Sprintf("owed %.2f but no customer identity on the order to write it off against", outstanding), "")
				shred = s.persistShredSteps(ctx, shred, steps, "")
				return s.failResult(shred, order.ID), fmt.Errorf("write off AR: order %s has no resolvable customer", order.ID)
			}
			key := crmContactID
			if key == "" {
				key = customerPhone
			}
			resp, aerr := s.treasuryClient.WriteOffDebt(ctx, req.TenantSlug, key, treasury.WriteOffDebtRequest{
				Amount:    outstanding,
				Reason:    fmt.Sprintf("Delete Sale (shred) of on-account order %s: %s", order.OrderNumber, req.Reason),
				Reference: order.OrderNumber,
			})
			if aerr != nil {
				steps[2] = nowStep(StepARWriteoff, "treasury", StatusFailed, aerr.Error(), "")
				shred = s.persistShredSteps(ctx, shred, steps, "")
				return s.failResult(shred, order.ID), fmt.Errorf("write off AR: %w", aerr)
			}
			steps[2] = nowStep(StepARWriteoff, "treasury", StatusCompleted,
				fmt.Sprintf("wrote off %.2f owed; balance now %s", outstanding, resp.BalanceDue), "")
		} else {
			steps[2] = nowStep(StepARWriteoff, "treasury", StatusSkipped, "on-account sale but nothing outstanding (already settled)", "")
		}
	} else {
		steps[2] = nowStep(StepARWriteoff, "treasury", StatusSkipped, "not an on-account sale", "")
	}
	shred = s.persistShredSteps(ctx, shred, steps, "")

	// Step: hard-delete pos-api's own rows in one transaction. Lines/payments/the order itself
	// must succeed; the long tail of secondary tables (KDS tickets, print jobs, loyalty points,
	// commissions, etc.) is deleted best-effort — a stray orphaned row there is a cosmetic leftover,
	// never a correctness or double-count risk, since the order they reference is already gone.
	if err := s.hardDeleteOrder(ctx, tenantID, order.ID); err != nil {
		steps[3] = nowStep(StepPOSHardDelete, "pos", StatusFailed, err.Error(), "")
		shred = s.persistShredSteps(ctx, shred, steps, "")
		return s.failResult(shred, order.ID), fmt.Errorf("hard delete order: %w", err)
	}
	steps[3] = nowStep(StepPOSHardDelete, "pos", StatusCompleted, "order, lines, payments and related records removed", "")
	shred = s.persistShredSteps(ctx, shred, steps, entpossaleshred.StatusCompleted)

	s.audit(ctx, tenantID, req.RequestedBy, order.OrderNumber, order.ID, req.Reason, "non_fiscalized", &shred.ID)

	return &Result{
		Branch:  "non_fiscalized",
		OrderID: order.ID,
		Steps:   steps,
		ShredID: &shred.ID,
	}, nil
}

func (s *Service) persistShredSteps(ctx context.Context, shred *ent.POSSaleShred, steps []entschema.ReversalStepJSON, finalStatus entpossaleshred.Status) *ent.POSSaleShred {
	upd := shred.Update().SetSteps(steps)
	if finalStatus != "" {
		upd = upd.SetStatus(finalStatus)
	}
	updated, err := upd.Save(ctx)
	if err != nil {
		s.log.Warn("shred: persist steps failed", zap.Error(err))
		return shred
	}
	return updated
}

func (s *Service) failResult(shred *ent.POSSaleShred, orderID uuid.UUID) *Result {
	_, _ = shred.Update().SetStatus(entpossaleshred.StatusFailed).Save(context.Background())
	return &Result{Branch: "non_fiscalized", OrderID: orderID, Steps: shred.Steps, ShredID: &shred.ID}
}

// hardDeleteOrder physically removes the order and every row that references it. Wrapped in one
// transaction so a failure never leaves the sale half-deleted.
func (s *Service) hardDeleteOrder(ctx context.Context, tenantID, orderID uuid.UUID) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}

	// Best-effort secondary tables — a failure here is logged, never aborts the delete (the
	// order itself is the source of truth being removed; these are downstream/derived records).
	bestEffort := func(name string, fn func() error) {
		if err := fn(); err != nil {
			s.log.Warn("shred: best-effort cleanup failed", zap.String("table", name), zap.Error(err))
		}
	}
	bestEffort("bar_tab_events", func() error {
		_, err := tx.BarTabEvent.Delete().Where(entbartabevent.OrderID(orderID)).Exec(ctx)
		return err
	})
	bestEffort("bill_splits", func() error {
		_, err := tx.BillSplit.Delete().Where(entbillsplit.TenantID(tenantID), entbillsplit.OrderID(orderID)).Exec(ctx)
		return err
	})
	bestEffort("commission_records", func() error {
		_, err := tx.CommissionRecord.Delete().Where(entcommissionrecord.TenantID(tenantID), entcommissionrecord.OrderID(orderID)).Exec(ctx)
		return err
	})
	bestEffort("kds_sync_failures", func() error {
		_, err := tx.KDSSyncFailure.Delete().Where(entkdssyncfailure.TenantID(tenantID), entkdssyncfailure.OrderID(orderID)).Exec(ctx)
		return err
	})
	bestEffort("kds_tickets", func() error {
		_, err := tx.KDSTicket.Delete().Where(entkdsticket.TenantID(tenantID), entkdsticket.OrderID(orderID)).Exec(ctx)
		return err
	})
	bestEffort("loyalty_transactions", func() error {
		_, err := tx.LoyaltyTransaction.Delete().Where(entloyaltytransaction.TenantID(tenantID), entloyaltytransaction.OrderID(orderID)).Exec(ctx)
		return err
	})
	bestEffort("order_links", func() error {
		_, err := tx.OrderLink.Delete().Where(entorderlink.OrderID(orderID)).Exec(ctx)
		return err
	})
	bestEffort("order_void_codes", func() error {
		_, err := tx.OrderVoidCode.Delete().Where(entordervoidcode.TenantID(tenantID), entordervoidcode.OrderID(orderID)).Exec(ctx)
		return err
	})
	bestEffort("print_jobs", func() error {
		_, err := tx.PrintJob.Delete().Where(entprintjob.TenantID(tenantID), entprintjob.OrderID(orderID)).Exec(ctx)
		return err
	})
	bestEffort("promotion_applications", func() error {
		_, err := tx.PromotionApplication.Delete().Where(entpromotionapplication.OrderID(orderID)).Exec(ctx)
		return err
	})
	bestEffort("stock_consumption_events", func() error {
		_, err := tx.StockConsumptionEvent.Delete().Where(entstockconsumptionevent.TenantID(tenantID), entstockconsumptionevent.OrderID(orderID)).Exec(ctx)
		return err
	})
	bestEffort("table_assignments", func() error {
		_, err := tx.TableAssignment.Delete().Where(enttableassignment.OrderID(orderID)).Exec(ctx)
		return err
	})

	// Required — these must succeed or the whole delete rolls back.
	if _, err := tx.POSPayment.Delete().Where(entpospayment.OrderID(orderID)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete payments: %w", err)
	}
	if _, err := tx.POSOrderLine.Delete().Where(entposorderline.OrderID(orderID)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete lines: %w", err)
	}
	if _, err := tx.POSOrder.Delete().Where(entposorder.ID(orderID), entposorder.TenantID(tenantID)).Exec(ctx); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("delete order: %w", err)
	}
	return tx.Commit()
}

func (s *Service) audit(ctx context.Context, tenantID, actorID uuid.UUID, orderNumber string, orderID uuid.UUID, reason, branch string, shredID *uuid.UUID) {
	if s.auditSvc == nil {
		return
	}
	after := map[string]any{
		"order_number": orderNumber,
		"order_id":     orderID.String(),
		"branch":       branch,
	}
	if shredID != nil {
		after["shred_id"] = shredID.String()
	}
	s.auditSvc.Record(ctx, audit.Entry{
		TenantID:    tenantID,
		ActorUserID: actorID,
		Action:      "order.delete",
		EntityType:  "pos_order",
		EntityID:    orderID.String(),
		Reason:      reason,
		After:       after,
	})
}
