package payments

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/roomfoliopayment"
)

// NormalizeManualCode applies the till's rules for a cashier-entered code to a payment taken
// outside the till (the hotel desk). It returns the canonical method and the tidied reference
// and label. An M-Pesa code must be present; MTN MoMo and Airtel Money need a transaction ID in
// the expected shape; Other needs a 2 to 40 character label. Codes are uppercased, and the label
// is kept only for Other. Every other method is returned as it came, without a label.
// The error message is ready to show to the cashier.
func NormalizeManualCode(method, ref, label string) (string, string, string, error) {
	req := RecordPaymentRequest{TenderMethod: canonicalTenderMethod(method), ExternalRef: ref, PaymentLabel: label}
	if req.TenderMethod == "mpesa_manual" {
		req.ExternalRef = strings.ToUpper(strings.TrimSpace(req.ExternalRef))
		if req.ExternalRef == "" {
			return "", "", "", errors.New("enter the M-Pesa transaction code")
		}
	}
	if err := normalizeManualTender(&req); err != nil {
		return "", "", "", errors.New(strings.TrimPrefix(err.Error(), "payments: "))
	}
	return req.TenderMethod, req.ExternalRef, req.PaymentLabel, nil
}

// ManualTenderName is the network's short name used in code messages: "M-Pesa", "MTN MoMo",
// "Airtel Money" or "Other".
func ManualTenderName(method string) string {
	return manualTenderName(canonicalTenderMethod(method))
}

// folioMethodSpellings are the RoomFolioPayment.method values that mean the given manual
// tender. The hotel desk has long stored a sighted M-Pesa code as "mpesa" or "manual".
func folioMethodSpellings(method string) []string {
	if method == "mpesa_manual" {
		return []string{"mpesa_manual", "mpesa", "manual"}
	}
	return []string{method}
}

// ManualCodeUsed reports whether an M-Pesa, MTN MoMo or Airtel Money code has already settled a
// completed till payment or a completed hotel folio payment of the same network in this tenant.
// Other (other_manual) carries a free reference and is never checked, as at the till.
// The comparison ignores case.
func ManualCodeUsed(ctx context.Context, client *ent.Client, tenantID uuid.UUID, method, code string) (bool, error) {
	method = canonicalTenderMethod(method)
	code = strings.TrimSpace(code)
	if code == "" || !isCodeCheckedManualTender(method) {
		return false, nil
	}
	rows, err := client.POSPayment.Query().
		Where(
			pospayment.ExternalReferenceEqualFold(code),
			pospayment.Status(StatusCompleted),
			pospayment.HasOrderWith(posorder.TenantID(tenantID)),
		).
		Select(pospayment.FieldPaymentData).
		All(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range rows {
		stored, _ := p.PaymentData["method"].(string)
		if sameManualMethod(stored, method) {
			return true, nil
		}
	}
	n, err := client.RoomFolioPayment.Query().
		Where(
			roomfoliopayment.TenantID(tenantID),
			roomfoliopayment.ReferenceEqualFold(code),
			roomfoliopayment.Status("completed"),
			roomfoliopayment.MethodIn(folioMethodSpellings(method)...),
		).
		Count(ctx)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
