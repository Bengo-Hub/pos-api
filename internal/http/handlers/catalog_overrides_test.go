package handlers

import (
	"testing"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/google/uuid"
)

func floatPtr(f float64) *float64 { return &f }

// TestMergeCatalogOverrides_OutletSpecificWinsOverTenantWide guards the reported cross-outlet
// catalog leak: an outlet's terminal showed another outlet's price/availability override for the
// same SKU, because the old merge picked whichever row Query().All() happened to return FIRST
// (undefined order) rather than the outlet actually asking. Since resolveCatalogOverrides now
// filters the query to only outlet-specific-to-THIS-outlet + tenant-wide rows, an override row
// belonging to a genuinely different, unrelated outlet should never even reach this merge step —
// these cases model what a caller (already correctly scoped) hands to mergeCatalogOverrides.
func TestMergeCatalogOverrides_OutletSpecificWinsOverTenantWide(t *testing.T) {
	outletA := uuid.New()

	tenantWide := &ent.POSCatalogOverride{
		OutletID:     nil,
		InventorySku: "SKU-1",
		SellingPrice: floatPtr(100),
		IsAvailable:  true,
	}
	outletASpecific := &ent.POSCatalogOverride{
		OutletID:     &outletA,
		InventorySku: "SKU-1",
		SellingPrice: floatPtr(150),
		IsAvailable:  false,
	}

	t.Run("tenant-wide then outlet-specific: outlet-specific wins", func(t *testing.T) {
		got := mergeCatalogOverrides([]*ent.POSCatalogOverride{tenantWide, outletASpecific})
		entry, ok := got["SKU-1"]
		if !ok {
			t.Fatal("expected SKU-1 in merged map")
		}
		if *entry.sellingPrice != 150 || entry.isAvailable != false {
			t.Fatalf("got price=%.0f available=%v, want outlet-specific (150, false)", *entry.sellingPrice, entry.isAvailable)
		}
	})

	t.Run("outlet-specific then tenant-wide: outlet-specific still wins (order must not matter)", func(t *testing.T) {
		got := mergeCatalogOverrides([]*ent.POSCatalogOverride{outletASpecific, tenantWide})
		entry, ok := got["SKU-1"]
		if !ok {
			t.Fatal("expected SKU-1 in merged map")
		}
		if *entry.sellingPrice != 150 || entry.isAvailable != false {
			t.Fatalf("got price=%.0f available=%v, want outlet-specific (150, false) regardless of row order", *entry.sellingPrice, entry.isAvailable)
		}
	})

	t.Run("only tenant-wide present: tenant-wide value used", func(t *testing.T) {
		got := mergeCatalogOverrides([]*ent.POSCatalogOverride{tenantWide})
		entry, ok := got["SKU-1"]
		if !ok {
			t.Fatal("expected SKU-1 in merged map")
		}
		if *entry.sellingPrice != 100 || entry.isAvailable != true {
			t.Fatalf("got price=%.0f available=%v, want tenant-wide (100, true)", *entry.sellingPrice, entry.isAvailable)
		}
	})
}
