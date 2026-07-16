package handlers

import (
	"context"

	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/modules/treasury"
)

// SetTreasuryClient wires the treasury S2S client used to backfill missing eTIMS fiscal
// identity on receipts (pull path for orders whose etims.invoice_transmitted event was
// missed — without it those receipts silently print without the KRA TIMS block).
func (h *ReceiptHandler) SetTreasuryClient(tc *treasury.Client) { h.treasury = tc }

// ensureEtimsFiscal backfills the order's eTIMS fiscal fields from treasury when they are
// empty. The normal path is the etims.invoice_transmitted event subscriber; this pull is
// the recovery path so a missed/late event never leaves a fiscal receipt unprintable.
// Best-effort: any failure returns the order unchanged.
func (h *ReceiptHandler) ensureEtimsFiscal(ctx context.Context, tenantSlug string, order *ent.POSOrder) *ent.POSOrder {
	if h.treasury == nil || order == nil || tenantSlug == "" {
		return order
	}
	// Already carries its fiscal identity (event path) — nothing to do.
	if order.EtimsInvoiceNumber != nil && *order.EtimsInvoiceNumber != "" {
		return order
	}
	// Only finalized sales can have been fiscalised.
	switch order.Status {
	case "completed", "paid", "closed":
	default:
		return order
	}

	fi, err := h.treasury.GetEtimsFiscal(ctx, tenantSlug, order.ID.String())
	if err != nil {
		h.log.Debug("receipt: etims fiscal backfill lookup failed", zap.Error(err))
		return order
	}
	if fi == nil || fi.ReceiptNo == "" { // not fiscalised (yet) — normal for most tenants
		return order
	}

	upd := h.client.POSOrder.UpdateOneID(order.ID).
		SetEtimsInvoiceNumber(fi.ReceiptNo).
		SetEtimsCuInvNo(fi.CuInvoiceNo).
		SetEtimsKraPin(fi.KraPin)
	if fi.DeviceSerial != "" {
		upd = upd.SetEtimsScuID(fi.DeviceSerial)
	}
	if fi.Signature != "" {
		upd = upd.SetEtimsRcptSign(fi.Signature)
	}
	if fi.QRURL != "" {
		upd = upd.SetEtimsQrCodeURL(fi.QRURL)
	}
	updated, uerr := upd.Save(ctx)
	if uerr != nil {
		h.log.Warn("receipt: etims fiscal backfill persist failed", zap.Error(uerr))
		// Still print with the fetched values this once.
		order.EtimsInvoiceNumber = &fi.ReceiptNo
		order.EtimsCuInvNo = &fi.CuInvoiceNo
		order.EtimsKraPin = &fi.KraPin
		if fi.DeviceSerial != "" {
			order.EtimsScuID = &fi.DeviceSerial
		}
		if fi.Signature != "" {
			order.EtimsRcptSign = &fi.Signature
		}
		if fi.QRURL != "" {
			order.EtimsQrCodeURL = &fi.QRURL
		}
		return order
	}
	h.log.Info("receipt: eTIMS fiscal identity backfilled from treasury",
		zap.String("order_id", order.ID.String()), zap.String("cu_inv_no", fi.CuInvoiceNo))
	updated.Edges = order.Edges // preserve loaded lines/payments
	return updated
}
