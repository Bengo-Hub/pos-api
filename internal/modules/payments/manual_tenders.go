package payments

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Manual (offline) tenders recorded at the till from a code the cashier reads off the
// customer's phone, the same way as mpesa_manual. They settle immediately and never call a gateway.
const (
	TenderMTNMoMoManual     = "mtn_momo_manual"
	TenderAirtelMoneyManual = "airtel_money_manual"
	TenderOtherManual       = "other_manual"
)

// manualTxnIDPattern is the transaction ID shape for MTN MoMo and Airtel Money codes, checked
// after trimming and uppercasing.
var manualTxnIDPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.\-]{5,29}$`)

const (
	paymentLabelMin   = 2
	paymentLabelMax   = 40
	otherManualRefMax = 40
)

// isCodeCheckedManualTender reports the manual tenders whose code must not settle two sales.
func isCodeCheckedManualTender(method string) bool {
	switch method {
	case "mpesa_manual", TenderMTNMoMoManual, TenderAirtelMoneyManual:
		return true
	}
	return false
}

// manualTenderName is the short name used in validation and duplicate-code messages.
func manualTenderName(method string) string {
	switch method {
	case TenderMTNMoMoManual:
		return "MTN MoMo"
	case TenderAirtelMoneyManual:
		return "Airtel Money"
	case TenderOtherManual:
		return "Other"
	default:
		return "M-Pesa"
	}
}

// normalizeManualTender checks and tidies the reference and label of a manual tender in place.
// MTN MoMo and Airtel Money need a transaction ID in the expected shape; Other needs a short
// label and takes an optional reference. Every other tender is left as it was.
func normalizeManualTender(req *RecordPaymentRequest) error {
	switch req.TenderMethod {
	case TenderMTNMoMoManual, TenderAirtelMoneyManual:
		ref := strings.ToUpper(strings.TrimSpace(req.ExternalRef))
		if ref == "" {
			return errors.New("payments: enter the " + manualTenderName(req.TenderMethod) + " transaction ID")
		}
		if !manualTxnIDPattern.MatchString(ref) {
			return errors.New("payments: " + manualTenderName(req.TenderMethod) +
				" transaction ID must be 6 to 30 letters or digits (dots and dashes allowed)")
		}
		req.ExternalRef = ref
		req.PaymentLabel = ""
	case TenderOtherManual:
		label := strings.TrimSpace(req.PaymentLabel)
		n := utf8.RuneCountInString(label)
		if n < paymentLabelMin || n > paymentLabelMax {
			return errors.New("payments: say how the customer paid (2 to 40 characters)")
		}
		ref := strings.TrimSpace(req.ExternalRef)
		if utf8.RuneCountInString(ref) > otherManualRefMax {
			return errors.New("payments: the reference can be at most 40 characters")
		}
		req.PaymentLabel = label
		req.ExternalRef = ref
	default:
		req.PaymentLabel = ""
	}
	return nil
}

// sameManualMethod reports whether a stored payment_data.method counts as the given manual
// tender for the duplicate-code check. Rows with no method predate method stamping and only
// ever came from the M-Pesa Code tender, so they count as mpesa_manual.
func sameManualMethod(stored, method string) bool {
	stored = canonicalTenderMethod(stored)
	if stored == "" {
		return method == "mpesa_manual"
	}
	return stored == method
}
