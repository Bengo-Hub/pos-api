package printing

import (
	"strings"

	"github.com/bengobox/pos-service/internal/ent"
)

// applyOutletContext stamps every outlet-, settings- and tenant-derived field onto a ReceiptView:
// the printed business identity (outlet name/address/phones/email/timezone/use case, the
// tenant-vs-outlet DisplayName rule), the configured receipt header/footer, VAT/paper settings,
// the "HOW TO PAY" block, and the two identity de-duplication guards.
//
// It is the ONE place that logic lives so every document type built in this package — the sale
// receipt (BuildReceiptView), the layaway payment receipt (BuildLayawayReceiptView) and the
// refund/return receipt (BuildReturnReceiptView) — prints the identical business header. Extracted
// verbatim out of BuildReceiptView; nil outlet/setting are both tolerated.
func applyOutletContext(v *ReceiptView, outlet *ent.Outlet, setting *ent.OutletSetting, tenantName string) {
	if outlet != nil {
		v.OutletName = outlet.Name
		v.Timezone = outlet.Timezone
		if outlet.UseCase != nil {
			v.UseCase = *outlet.UseCase
		}
		if addr := outlet.AddressJSON; addr != nil {
			if street, ok := addr["street"].(string); ok && street != "" {
				v.OutletAddress = street
			} else if city, ok := addr["city"].(string); ok {
				v.OutletAddress = city
			}
			v.OutletPhones = formatContactPhones(addr["contact_phones"])
			if email, ok := addr["contact_email"].(string); ok {
				v.OutletEmail = strings.TrimSpace(email)
			}
		}
	}

	// De-duplicate: when the outlet's address was set to the same text as its name (a common
	// mis-configuration — see "Urban Loft Cafe Busia" printed twice), drop the address so the
	// receipt shows each piece of information exactly once.
	if strings.EqualFold(strings.TrimSpace(v.OutletAddress), strings.TrimSpace(v.OutletName)) {
		v.OutletAddress = ""
	}

	// Multi-outlet tenant name vs outlet name (BOI Enterprises case: several branches under one
	// tenant) — see resolveDisplayName's doc comment for the exact rule.
	var meta map[string]any
	isHQ := true // no outlet loaded => never override away from the tenant name
	if outlet != nil {
		isHQ = outlet.IsHq
	}
	if setting != nil {
		meta = setting.Metadata
	}
	v.DisplayName = resolveDisplayName(tenantName, v.OutletName, isHQ, meta)

	if setting != nil {
		if setting.ReceiptHeader != nil {
			v.ReceiptHeader = *setting.ReceiptHeader
		}
		if setting.ReceiptFooter != nil {
			v.ReceiptFooter = *setting.ReceiptFooter
		}
		v.VatEnabled = setting.VatEnabled
		v.VatRate = setting.VatRate
		v.PaperWidth = setting.PaperWidth
		// Receipt & Printing → "Show logo" toggle (freeform metadata; absent = true).
		if b, ok := setting.Metadata["receipt_show_logo"].(bool); ok {
			v.ShowLogo = b
		}

		if setting.ShowPaymentInfoOnReceipt {
			pm := &ReceiptPaymentMethods{}
			if setting.MpesaPaybill != nil {
				pm.MpesaPaybill = *setting.MpesaPaybill
			}
			if setting.MpesaAccountReference != nil {
				pm.MpesaAccountRef = *setting.MpesaAccountReference
			}
			if setting.MpesaTill != nil {
				pm.MpesaTill = *setting.MpesaTill
			}
			if setting.MpesaPochi != nil {
				pm.MpesaPochi = *setting.MpesaPochi
			}
			if setting.AirtelMoneyNumber != nil {
				pm.AirtelMoneyNumber = *setting.AirtelMoneyNumber
			}
			if setting.MtnMomoNumber != nil {
				pm.MtnMomoNumber = *setting.MtnMomoNumber
			}
			if setting.BankName != nil {
				pm.BankName = *setting.BankName
			}
			if setting.BankAccountNumber != nil {
				pm.BankAccountNumber = *setting.BankAccountNumber
			}
			if setting.BankAccountName != nil {
				pm.BankAccountName = *setting.BankAccountName
			}
			if pm.HasAny() {
				v.PaymentMethods = pm
			}
		}
	}

	// De-duplicate: a custom receipt header that was configured to just repeat the business
	// name/outlet identity already printed above it (the "Urban Loft Cafe Busia" printed twice
	// report, later the "Gachie" outlet printing THREE location-ish lines, and the BOI
	// Enterprises case where a header set to the tenant's own name repeated v.DisplayName)
	// prints the same information again. Exact-match covers a header set to literally the
	// outlet name/address; the SUBSTRING check catches a richer free-text header that embeds
	// the name in a fuller description (e.g. header "Red Hill - Westbay Mall, Gachie" when the
	// outlet is named "Gachie") — a case exact-match alone would miss. Checked against BOTH
	// DisplayName (what actually prints as the headline) AND the raw tenantName: on a
	// non-HQ outlet that turned "Show Business Name on Receipt" OFF, DisplayName is the outlet's
	// own name (so the outlet-name check alone would miss it), yet a header still containing the
	// literal tenant name (e.g. "BOI ENTERPRISES") would silently defeat the whole point of that
	// toggle — the outlet chose to hide the parent tenant's identity, so a side-channel free-text
	// field can't be allowed to keep printing it. This is the single canonical builder, so the
	// fix applies to the JSON API, server HTML/PDF, and ESC/POS thermal receipt at once.
	if h := strings.TrimSpace(v.ReceiptHeader); h != "" && headerRepeatsOutletIdentity(h, v.DisplayName, tenantName, v.OutletName, v.OutletAddress) {
		v.ReceiptHeader = ""
	}

}
