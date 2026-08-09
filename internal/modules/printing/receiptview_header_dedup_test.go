package printing

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
)

// TestHeaderRepeatsOutletIdentity covers the BOI Enterprises case where a tenant's custom
// receipt-header text was configured to literally repeat the DISPLAY name (not just the outlet's
// own name/address) — a duplicate the dedup used to miss because it only compared against
// OutletName/OutletAddress, never DisplayName (the value that actually prints as the headline).
func TestHeaderRepeatsOutletIdentity(t *testing.T) {
	cases := []struct {
		name          string
		header        string
		displayName   string
		tenantName    string
		outletName    string
		outletAddress string
		want          bool
	}{
		{
			name:        "header exactly repeats the display name",
			header:      "BOI Enterprises",
			displayName: "BOI Enterprises",
			tenantName:  "BOI Enterprises",
			outletName:  "Nelly Store",
			want:        true,
		},
		{
			name:        "header embeds the display name in a fuller description",
			header:      "Proudly part of BOI Enterprises",
			displayName: "BOI Enterprises",
			tenantName:  "BOI Enterprises",
			outletName:  "Nelly Store",
			want:        true,
		},
		{
			name:          "header exactly repeats the outlet address",
			header:        "Westlands, Nairobi",
			displayName:   "BOI Enterprises",
			tenantName:    "BOI Enterprises",
			outletName:    "Nelly Store",
			outletAddress: "Westlands, Nairobi",
			want:          true,
		},
		{
			name:        "header embeds the outlet name",
			header:      "Red Hill - Westbay Mall, Gachie",
			displayName: "Gachie",
			outletName:  "Gachie",
			want:        true,
		},
		{
			name:        "non-HQ outlet hiding the tenant name still leaks it back in via a custom header",
			header:      "BOI ENTERPRISES",
			displayName: "Nelly Store", // toggle off: outlet's own name resolved as the headline
			tenantName:  "BOI Enterprises",
			outletName:  "Nelly Store",
			want:        true,
		},
		{
			name:        "genuinely distinct header/slogan is kept",
			header:      "Thank you for shopping with us!",
			displayName: "BOI Enterprises",
			tenantName:  "BOI Enterprises",
			outletName:  "Nelly Store",
			want:        false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := headerRepeatsOutletIdentity(tc.header, tc.displayName, tc.tenantName, tc.outletName, tc.outletAddress)
			if got != tc.want {
				t.Errorf("headerRepeatsOutletIdentity(%q, %q, %q, %q, %q) = %v, want %v",
					tc.header, tc.displayName, tc.tenantName, tc.outletName, tc.outletAddress, got, tc.want)
			}
		})
	}
}

// TestBuildReceiptViewClearsHeaderRepeatingDisplayName is an end-to-end check (no DB) that
// BuildReceiptView itself — not just the helper — clears a ReceiptHeader configured to repeat
// the resolved DisplayName, on a non-HQ outlet that turned the tenant-name toggle off (so
// DisplayName resolves to the outlet's own name, matching the header text).
func TestBuildReceiptViewClearsHeaderRepeatingDisplayName(t *testing.T) {
	order := &ent.POSOrder{OrderNumber: "000173", Currency: "KES"}
	outlet := &ent.Outlet{Name: "Nelly Store", IsHq: false}
	header := "Nelly Store"
	setting := &ent.OutletSetting{
		ReceiptHeader: &header,
		Metadata:      map[string]any{"receipt_show_tenant_name": false},
	}

	v := BuildReceiptView(order, nil, outlet, setting, ReceiptViewOpts{TenantName: "BOI Enterprises"})

	if v.DisplayName != "Nelly Store" {
		t.Fatalf("DisplayName = %q, want %q", v.DisplayName, "Nelly Store")
	}
	if v.ReceiptHeader != "" {
		t.Errorf("ReceiptHeader = %q, want cleared (repeats DisplayName)", v.ReceiptHeader)
	}
}

// TestBuildReceiptViewClearsHeaderRepeatingHiddenTenantName reproduces the live BOI Enterprises
// report: Nelly Store (non-HQ) turned "Show Business Name on Receipt" off, so DisplayName
// correctly resolves to "Nelly Store" — but the tenant had ALSO configured a custom
// ReceiptHeader literally set to "BOI ENTERPRISES", which kept printing regardless of the
// toggle (a side-channel that defeated the whole point of turning it off). BuildReceiptView must
// clear it too.
func TestBuildReceiptViewClearsHeaderRepeatingHiddenTenantName(t *testing.T) {
	order := &ent.POSOrder{OrderNumber: "000173", Currency: "KES"}
	outlet := &ent.Outlet{Name: "Nelly Store", IsHq: false}
	header := "BOI ENTERPRISES"
	setting := &ent.OutletSetting{
		ReceiptHeader: &header,
		Metadata:      map[string]any{"receipt_show_tenant_name": false},
	}

	v := BuildReceiptView(order, nil, outlet, setting, ReceiptViewOpts{TenantName: "BOI Enterprises"})

	if v.DisplayName != "Nelly Store" {
		t.Fatalf("DisplayName = %q, want %q", v.DisplayName, "Nelly Store")
	}
	if v.ReceiptHeader != "" {
		t.Errorf("ReceiptHeader = %q, want cleared (leaks the hidden tenant name back in)", v.ReceiptHeader)
	}
}
