package handlers

import (
	"math"
	"strings"
)

// Variants (size, colour, ...) are their own inventory items: a child item with its own SKU,
// barcode, price and stock, linked to a display parent through metadata.variant_of. The catalog
// sweep asks inventory-api for include=variants, which hides the children from the list and
// returns each parent with variants[]. The helpers below keep the POS-side rules for those
// variants in one place.

// variantsMatchSearch reports whether any variant's SKU, barcode or name contains the
// (already lower-cased) search term, so a typed child SKU or barcode still finds its parent
// in the terminal's catalog search even though the child row itself is hidden.
func variantsMatchSearch(variants []inventoryProxyVariant, search string) bool {
	if search == "" {
		return false
	}
	for _, v := range variants {
		if strings.Contains(strings.ToLower(v.SKU), search) ||
			strings.Contains(strings.ToLower(v.Barcode), search) ||
			strings.Contains(strings.ToLower(v.Name), search) {
			return true
		}
	}
	return false
}

// applyVariantOverrides applies each child's POSCatalogOverride (keyed by the child SKU, the
// same row the inventory item.created sync writes for every item) to the variant list: a POS
// selling price replaces the inventory price, and a staff "unavailable" toggle marks the
// variant inactive so the picker disables it. Prices round up to whole units like every other
// catalog price. Stock never affects availability here (manual availability rule).
// Returns a new slice; the cached source is never mutated.
func applyVariantOverrides(variants []inventoryProxyVariant, overrides map[string]overrideEntry) []inventoryProxyVariant {
	if len(variants) == 0 {
		return variants
	}
	out := make([]inventoryProxyVariant, len(variants))
	for i, v := range variants {
		if o, ok := overrides[v.SKU]; ok {
			if o.sellingPrice != nil && *o.sellingPrice > 0 {
				v.Price = *o.sellingPrice
			}
			if !o.isAvailable {
				v.IsActive = false
			}
		}
		if v.Price > 0 {
			v.Price = math.Ceil(v.Price)
		}
		out[i] = v
	}
	return out
}

// lowestActiveVariantPrice is the "from" price of a variant parent: the cheapest active
// variant with a price, or 0 when none has one.
func lowestActiveVariantPrice(variants []inventoryProxyVariant) float64 {
	lowest := 0.0
	for _, v := range variants {
		if !v.IsActive || v.Price <= 0 {
			continue
		}
		if lowest == 0 || v.Price < lowest {
			lowest = v.Price
		}
	}
	return lowest
}
