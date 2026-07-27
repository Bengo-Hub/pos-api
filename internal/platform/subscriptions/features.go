package subscriptions

// Feature codes — the backend twin of pos-ui's feature-catalog.ts. Every value MUST be a
// real code seeded by subscription-service (cmd/seed/plans_bundles.go, plans_pos.go,
// feature_catalog.go). Used with RequireFeature() to gate route groups.
const (
	FeatureTableManagement = "table_management"
	FeatureKDS             = "kds"
	FeatureHotelModule     = "hotel_module"
	FeatureConference      = "conference_events"
	FeatureHappyHour       = "happy_hour"
	FeatureLoyalty         = "loyalty_program"
	FeatureOnlineOrdering  = "online_ordering"
	FeatureShiftReports    = "shift_reports"
	// FeatureFacilityBooking gates bookable-space management (co-working desks, conference/
	// meeting rooms) — sell + capacity-manage a Facility from the till. Seeded on POS_HOSP_PRO
	// and up (cmd/seed/plans_pos_lines.go); Starter does not include it. Deliberately decoupled
	// from FeatureHotelModule (rooms/check-in/folio) — a cafe with spare floor space shouldn't
	// need the full hotel PMS just to sell co-working.
	FeatureFacilityBooking = "facility_booking"
	// FeatureLayaway gates layaway plans & payments (retail T2+ per the use-case
	// PowerSuite matrix); FeatureCommissions gates commission records/rules/payout
	// (retail/services). Both codes are seeded in feature_catalog.go and on the
	// POWERSUITE_DUKA_* tiers.
	FeatureLayaway     = "layaway"
	FeatureCommissions = "commissions"
	// FeatureStorefrontBanner gates the storefront promotions banner (Promotion.metadata
	// ["banner"].show_on_storefront, see internal/modules/promotions/banner.go), surfaced to
	// the customer-facing ordering storefront via S2SListBanners. Checked inline at the point
	// of write (createPromotionFromInput's callers in promotions.go/promotions_s2s.go) and at
	// the point of read (S2SListBanners) rather than as a route-group middleware, since
	// POST/PUT /pos/promotions handles both gated-banner and ungated-plain-discount fields in
	// one payload. Seeded on POWERSUITE_{HOSP|DUKA|DAWA}_{PRO|GOLD} in subscriptions-api
	// (cmd/seed/plans_powersuite_usecase.go); deliberately absent from BASIC.
	FeatureStorefrontBanner = "storefront_banner"
)

// Structural plan-limit keys (hard-block, no overage — require a plan upgrade).
const (
	LimitDevices  = "max_devices"
	LimitTables   = "max_tables"
	LimitCashiers = "max_cashiers"
	LimitOutlets  = "max_outlets"
	LimitStaff    = "max_staff"
	LimitRooms    = "max_rooms"
)

// Metered usage metric names reported to subscription-service /usage/report.
const (
	MetricOrders       = "orders"
	MetricTransactions = "transactions"
)
