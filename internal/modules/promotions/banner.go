package promotions

import "encoding/json"

// MetadataBannerKey is the key under which the storefront banner config lives inside
// Promotion.metadata (e.g. metadata["banner"]). There is no dedicated schema column for
// this — banner fields are additive-but-non-schema-breaking, so they're normalized into
// the existing metadata JSON blob rather than a new Atlas migration.
const MetadataBannerKey = "banner"

// PromotionBannerConfig is the storefront-marketing-banner sub-object stored inside a
// Promotion's metadata JSON. A business flags a promotion to also surface as a banner on
// the customer-facing ordering storefront (a separate app); ordering-backend reads these
// via the S2SListBanners endpoint (promotions_s2s.go).
type PromotionBannerConfig struct {
	ShowOnStorefront bool     `json:"show_on_storefront"`
	BannerTitle      string   `json:"banner_title,omitempty"`
	BannerSubtitle   string   `json:"banner_subtitle,omitempty"`
	BannerImageURL   string   `json:"banner_image_url,omitempty"`
	CTALabel         string   `json:"cta_label,omitempty"`
	CTALink          string   `json:"cta_link,omitempty"`
	BannerColor      string   `json:"banner_color,omitempty"`
	TextColor        string   `json:"text_color,omitempty"`
	UseCases         []string `json:"use_cases,omitempty"` // empty = show for all outlet use_cases
	// IsFlashSale marks this banner as a time-boxed flash sale so storefronts render a
	// countdown to the promotion's existing start_at/end_at instead of a plain static
	// banner. Purely a presentation flag — the actual time window and discount value
	// already live on Promotion/PromotionRule; no new schema needed.
	IsFlashSale bool `json:"is_flash_sale,omitempty"`
}

// BannerFromMetadata is the single choke point for READING a promotion's storefront banner
// config back out of its metadata JSON. Returns the zero value (ShowOnStorefront=false) when
// the promotion never had a banner configured, so callers never need a nil check.
func BannerFromMetadata(meta map[string]any) PromotionBannerConfig {
	var cfg PromotionBannerConfig
	if meta == nil {
		return cfg
	}
	raw, ok := meta[MetadataBannerKey]
	if !ok || raw == nil {
		return cfg
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal(b, &cfg)
	return cfg
}

// MergeBannerMetadata is the single choke point for WRITING a promotion's storefront banner
// config into its metadata JSON. It read-merges: every other key already present in existing
// (e.g. discount_type/discount_value consumed by Service.calculateDiscount) is preserved
// untouched, and only metadata["banner"] is replaced. Returns a NEW map — callers pass the
// result straight to Promotion(Create|Update).SetMetadata.
func MergeBannerMetadata(existing map[string]any, banner PromotionBannerConfig) map[string]any {
	meta := make(map[string]any, len(existing)+1)
	for k, v := range existing {
		meta[k] = v
	}
	b, err := json.Marshal(banner)
	if err != nil {
		return meta
	}
	var asMap map[string]any
	if err := json.Unmarshal(b, &asMap); err != nil {
		return meta
	}
	meta[MetadataBannerKey] = asMap
	return meta
}
