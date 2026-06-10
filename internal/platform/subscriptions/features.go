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
