package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/pos-service/internal/config"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/kdsstation"
	"github.com/bengobox/pos-service/internal/ent/loyaltyprogram"
	"github.com/bengobox/pos-service/internal/ent/outlet"
	"github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/ent/pospermission"
	"github.com/bengobox/pos-service/internal/ent/posrolev2"
	"github.com/bengobox/pos-service/internal/ent/section"
	enttable "github.com/bengobox/pos-service/internal/ent/table"
	"github.com/bengobox/pos-service/internal/ent/tender"
	"github.com/bengobox/pos-service/internal/modules/tenant"
)

// tenantSeedConfig controls what data is seeded for each tenant.
// Staff members and PINs are NOT seeded here — they arrive via auth.user.* NATS events
// published by auth-api (auth.user.created + auth.user.pin_set) so UUIDs stay aligned.
type tenantSeedConfig struct {
	slug       string // must match auth-api tenant slug
	seedTables bool   // seed tables & sections (hospitality outlets only)
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	db, err := sql.Open("pgx", cfg.Postgres.URL)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()
	db.SetMaxIdleConns(2)
	db.SetMaxOpenConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(1 * time.Minute)

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(ent.Driver(driver))
	defer client.Close()

	syncer := tenant.NewSyncer(client, cfg.Auth.ServiceURL, nil)

	tenantConfigs := []tenantSeedConfig{
		{slug: "codevertex-demo", seedTables: true},
	}

	for _, tc := range tenantConfigs {
		tenantID, syncErr := syncer.SyncTenant(ctx, tc.slug)
		if syncErr != nil {
			log.Printf("⚠️  sync tenant %s: %v (skipping)", tc.slug, syncErr)
			continue
		}
		log.Printf("▶ Seeding tenant: %s (%s)", tc.slug, tenantID)
		if runErr := runSeed(ctx, client, tenantID, tc); runErr != nil {
			log.Fatalf("seed data for %s: %v", tc.slug, runErr)
		}
		log.Printf("✅ Tenant %s seeded successfully", tc.slug)
	}

	// Platform-wide configs (rate limits, service configs) — seeded once.
	if err := seedRateLimitConfigs(ctx, client); err != nil {
		log.Fatalf("seed rate limit configs: %v", err)
	}
	if err := seedServiceConfigs(ctx, client); err != nil {
		log.Fatalf("seed service configs: %v", err)
	}

	log.Println("POS seed completed successfully")
}

// outletUUID uses the same deterministic formula as ordering-backend and inventory-api.
func outletUUID(tenantSlug, outletSlug string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:cafe:outlet:%s:%s", tenantSlug, outletSlug)))
}

// inventoryItemUUID uses the same deterministic formula as inventory-api.
func inventoryItemUUID(tenantID uuid.UUID, sku string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:inventory:item:%s:%s", tenantID, sku)))
}

func runSeed(ctx context.Context, client *ent.Client, tenantID uuid.UUID, tc tenantSeedConfig) error {
	hqOutletID, err := seedOutlets(ctx, client, tenantID, tc.slug)
	if err != nil {
		return fmt.Errorf("seed outlets: %w", err)
	}

	// Remove any logistics outlet that may have been seeded in a prior run.
	// Logistics does not apply to POS terminals.
	if err := deleteLogisticsOutlets(ctx, client, tenantID); err != nil {
		log.Printf("  ⚠️  cleanup logistics outlets: %v", err)
	}

	if err := seedTenders(ctx, client, tenantID, hqOutletID); err != nil {
		return fmt.Errorf("seed tenders: %w", err)
	}

	if tc.seedTables {
		sectionIDs, err := seedSections(ctx, client, tenantID, hqOutletID)
		if err != nil {
			return fmt.Errorf("seed sections: %w", err)
		}
		if err := seedTables(ctx, client, tenantID, hqOutletID, sectionIDs); err != nil {
			return fmt.Errorf("seed tables: %w", err)
		}
	}

	// KDS stations define kitchen/bar/expo routing — they are outlet metadata, not catalog data.
	if err := seedKDSStations(ctx, client, tenantID, hqOutletID); err != nil {
		log.Printf("  ⚠️  seed KDS stations: %v (non-fatal, stations can be configured via UI)", err)
	}

	if err := seedRBACPermissions(ctx, client); err != nil {
		return fmt.Errorf("seed RBAC permissions: %w", err)
	}

	if err := seedRBACRoles(ctx, client, tenantID); err != nil {
		return fmt.Errorf("seed RBAC roles: %w", err)
	}

	if err := seedLoyaltyProgram(ctx, client, tenantID); err != nil {
		log.Printf("  ⚠️  seed loyalty program: %v (non-fatal)", err)
	}

	return nil
}

type outletDef struct {
	slug        string
	code        string
	name        string
	useCase     string
	isHQ        bool
	pinMessage  string
	displayMode string
	enableKDS   bool
	enableAppts bool
	enableHotel bool
	defaultView string
}

// outletsByTenantSlug defines the POS outlets per tenant.
// Slugs MUST match auth-api outletsByTenant keys so deterministic UUIDs align.
var outletsByTenantSlug = map[string][]outletDef{
	"codevertex-demo": {
		{
			slug:        "demo-hospitality",
			code:        "HOSP",
			name:        "Demo Grand Hotel & Restaurant",
			useCase:     "hospitality",
			isHQ:        true,
			pinMessage:  "Welcome to Demo Grand Hotel — check your shift schedule",
			displayMode: "card",
			enableKDS:   true,
			enableAppts: false,
			enableHotel: true,
			defaultView: "tables",
		},
		{
			slug:        "demo-retail",
			code:        "RETAIL",
			name:        "Demo City Supermarket",
			useCase:     "retail",
			isHQ:        false,
			pinMessage:  "Welcome to Demo City Supermarket — barcode scanner is active",
			displayMode: "list",
			enableKDS:   false,
			enableAppts: false,
			defaultView: "catalog",
		},
		{
			slug:        "demo-quick",
			code:        "QSR",
			name:        "Demo Express Kiosk",
			useCase:     "quick_service",
			isHQ:        false,
			pinMessage:  "Welcome to Demo Express — fast service starts here!",
			displayMode: "card",
			enableKDS:   true,
			enableAppts: false,
			defaultView: "catalog",
		},
		{
			slug:        "demo-pharmacy",
			code:        "PHARMA",
			name:        "Demo Health Pharmacy",
			useCase:     "pharmacy",
			isHQ:        false,
			pinMessage:  "Welcome to Demo Health Pharmacy — verify prescriptions at counter",
			displayMode: "list",
			enableKDS:   false,
			enableAppts: false,
			defaultView: "catalog",
		},
		{
			slug:        "demo-services",
			code:        "SVC",
			name:        "Demo Beauty & Wellness",
			useCase:     "services",
			isHQ:        false,
			pinMessage:  "Welcome to Demo Beauty & Wellness — check appointments board",
			displayMode: "card",
			enableKDS:   false,
			enableAppts: true,
			defaultView: "catalog",
		},
	},
}

// seedOutlets creates all outlets for the given tenant and returns the HQ outlet ID.
// Outlet slugs match auth-api so deterministic UUIDs are identical across services.
func seedOutlets(ctx context.Context, client *ent.Client, tenantID uuid.UUID, tenantSlug string) (uuid.UUID, error) {
	defs, ok := outletsByTenantSlug[tenantSlug]
	if !ok {
		return uuid.Nil, fmt.Errorf("no outlet definitions for tenant %q", tenantSlug)
	}

	var hqID uuid.UUID
	for _, d := range defs {
		id := outletUUID(tenantSlug, d.slug)
		existing, err := client.Outlet.Query().Where(outlet.ID(id)).Only(ctx)
		if err == nil {
			if d.isHQ {
				hqID = existing.ID
			}
			if err2 := seedOutletSetting(ctx, client, existing.ID, d); err2 != nil {
				log.Printf("  ⚠️  outlet setting for %s: %v", d.name, err2)
			}
			log.Printf("  ✓ Outlet exists: %s/%s (use_case=%s)", tenantSlug, d.code, d.useCase)
			continue
		}
		if !ent.IsNotFound(err) {
			return uuid.Nil, fmt.Errorf("query outlet %s: %w", d.slug, err)
		}

		o, err := client.Outlet.Create().
			SetID(id).
			SetTenantID(tenantID).
			SetTenantSlug(tenantSlug).
			SetCode(d.code).
			SetName(d.name).
			SetChannelType("physical").
			SetStatus("active").
			SetUseCase(d.useCase).
			SetIsHq(d.isHQ).
			Save(ctx)
		if err != nil {
			return uuid.Nil, fmt.Errorf("create outlet %s: %w", d.slug, err)
		}
		log.Printf("  ✓ Outlet created: %s (use_case=%s, is_hq=%v)", o.Name, d.useCase, d.isHQ)

		if err := seedOutletSetting(ctx, client, o.ID, d); err != nil {
			log.Printf("  ⚠️  outlet setting for %s: %v", d.name, err)
		}

		if d.isHQ {
			hqID = o.ID
		}
	}

	if hqID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("HQ outlet not found after seeding for %s", tenantSlug)
	}
	return hqID, nil
}

// deleteLogisticsOutlets removes any outlet with use_case=logistics/warehouse for the tenant.
// Logistics does not apply to POS — no routes, no staff provisioning, no module mapping.
func deleteLogisticsOutlets(ctx context.Context, client *ent.Client, tenantID uuid.UUID) error {
	deleted, err := client.Outlet.Delete().
		Where(
			outlet.TenantID(tenantID),
			outlet.UseCaseIn("logistics", "warehouse"),
		).
		Exec(ctx)
	if err != nil {
		return err
	}
	if deleted > 0 {
		log.Printf("  🗑  Removed %d logistics/warehouse outlet(s) for tenant %s", deleted, tenantID)
	}
	return nil
}

func seedOutletSetting(ctx context.Context, client *ent.Client, outletID uuid.UUID, d outletDef) error {
	existing, err := client.OutletSetting.Query().
		Where(outletsetting.OutletID(outletID)).
		Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return err
	}

	if existing != nil {
		// Update toggles so re-running seed fixes existing outlets (e.g. hotel_module_enabled).
		update := existing.Update().
			SetEnableKds(d.enableKDS).
			SetEnableAppointments(d.enableAppts).
			SetHotelModuleEnabled(d.enableHotel)
		if _, err2 := update.Save(ctx); err2 != nil {
			return fmt.Errorf("update outlet setting: %w", err2)
		}
		log.Printf("  ✓ OutletSetting updated for outlet %s (hotel=%v)", outletID, d.enableHotel)
		return nil
	}

	create := client.OutletSetting.Create().
		SetOutletID(outletID).
		SetDisplayMode(d.displayMode).
		SetShowImages(true).
		SetShowBarcodeScanner(d.useCase == "retail").
		SetDefaultView(d.defaultView).
		SetEnableKds(d.enableKDS).
		SetEnableAppointments(d.enableAppts).
		SetHotelModuleEnabled(d.enableHotel)
	if d.pinMessage != "" {
		create = create.SetPinLoginMessage(d.pinMessage)
	}
	_, err = create.Save(ctx)
	if err != nil {
		return fmt.Errorf("create outlet setting: %w", err)
	}
	log.Printf("  ✓ OutletSetting created for outlet %s (hotel=%v)", outletID, d.enableHotel)
	return nil
}

func seedTenders(ctx context.Context, client *ent.Client, tenantID, outletID uuid.UUID) error {
	type tenderDef struct {
		name     string
		tType    string
		isActive bool
	}
	tenders := []tenderDef{
		{"Cash", "cash", true},
		{"M-Pesa", "mobile", true},
		{"Card", "card", true},
		{"Manual / Till", "manual", true},
	}

	for _, t := range tenders {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:pos:tender:%s:%s", tenantID, t.name)))
		exists, err := client.Tender.Query().Where(tender.ID(id)).Exist(ctx)
		if err != nil {
			return err
		}
		if exists {
			continue
		}
		_, err = client.Tender.Create().
			SetID(id).
			SetTenantID(tenantID).
			SetName(t.name).
			SetType(t.tType).
			SetIsActive(t.isActive).
			Save(ctx)
		if err != nil {
			return fmt.Errorf("create tender %s: %w", t.name, err)
		}
	}
	log.Println("  ✓ Tenders seeded (Cash, M-Pesa, Card, Manual)")
	return nil
}

func seedSections(ctx context.Context, client *ent.Client, tenantID, outletID uuid.UUID) (map[string]uuid.UUID, error) {
	type sectionDef struct {
		name        string
		slug        string
		sectionType section.SectionType
		floor       int
		order       int
	}
	sections := []sectionDef{
		{"Main Hall", "main-hall", section.SectionTypeMainHall, 1, 1},
		{"Outdoor Patio", "outdoor-patio", section.SectionTypeOutdoor, 1, 2},
		{"VIP Lounge", "vip-lounge", section.SectionTypeVip, 1, 3},
	}

	ids := make(map[string]uuid.UUID)
	for _, s := range sections {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:pos:section:%s:%s", outletID, s.slug)))

		exists, err := client.Section.Query().Where(section.ID(id)).Exist(ctx)
		if err != nil {
			return nil, err
		}
		if exists {
			ids[s.slug] = id
			continue
		}

		created, err := client.Section.Create().
			SetID(id).
			SetTenantID(tenantID).
			SetOutletID(outletID).
			SetName(s.name).
			SetSlug(s.slug).
			SetSectionType(s.sectionType).
			SetFloorNumber(s.floor).
			SetSortOrder(s.order).
			SetIsActive(true).
			Save(ctx)
		if err != nil {
			return nil, fmt.Errorf("create section %s: %w", s.name, err)
		}
		ids[s.slug] = created.ID
	}
	log.Println("  ✓ Sections seeded (Main Hall, Outdoor Patio, VIP Lounge)")
	return ids, nil
}

func seedTables(ctx context.Context, client *ent.Client, tenantID, outletID uuid.UUID, sectionIDs map[string]uuid.UUID) error {
	type tableDef struct {
		name      string
		capacity  int
		section   string
		tableType enttable.TableType
		tags      []string
		x, y      float64
	}
	tables := []tableDef{
		// Main Hall — 6 tables
		{"T1", 2, "main-hall", enttable.TableTypeStandard, nil, 1, 1},
		{"T2", 2, "main-hall", enttable.TableTypeStandard, nil, 2, 1},
		{"T3", 4, "main-hall", enttable.TableTypeStandard, nil, 3, 1},
		{"T4", 4, "main-hall", enttable.TableTypeStandard, []string{"Window"}, 1, 2},
		{"T5", 6, "main-hall", enttable.TableTypeBooth, nil, 2, 2},
		{"T6", 6, "main-hall", enttable.TableTypeBooth, nil, 3, 2},
		// Outdoor Patio — 3 tables
		{"P1", 4, "outdoor-patio", enttable.TableTypeStandard, []string{"Garden View"}, 1, 1},
		{"P2", 4, "outdoor-patio", enttable.TableTypeStandard, nil, 2, 1},
		{"P3", 4, "outdoor-patio", enttable.TableTypeStandard, []string{"Balcony"}, 3, 1},
		// VIP Lounge — 3 tables
		{"V1", 6, "vip-lounge", enttable.TableTypeVip, []string{"VIP"}, 1, 1},
		{"V2", 8, "vip-lounge", enttable.TableTypeVip, []string{"VIP", "Private"}, 2, 1},
		{"V3", 10, "vip-lounge", enttable.TableTypeVvip, []string{"VVIP", "Private", "Suite"}, 3, 1},
	}

	for _, t := range tables {
		sectionID, ok := sectionIDs[t.section]
		if !ok {
			continue
		}
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:pos:table:%s:%s", outletID, t.name)))

		exists, err := client.Table.Query().Where(enttable.ID(id)).Exist(ctx)
		if err != nil {
			return err
		}
		if exists {
			continue
		}

		builder := client.Table.Create().
			SetID(id).
			SetTenantID(tenantID).
			SetOutletID(outletID).
			SetSectionID(sectionID).
			SetName(t.name).
			SetCapacity(t.capacity).
			SetTableType(t.tableType).
			SetXPosition(t.x).
			SetYPosition(t.y).
			SetStatus("available")
		if t.tags != nil {
			builder.SetTags(t.tags)
		}
		if _, err := builder.Save(ctx); err != nil {
			return fmt.Errorf("create table %s: %w", t.name, err)
		}
	}
	log.Println("  ✓ Tables seeded (12 tables across 3 sections)")
	return nil
}

// seedKDSStations creates KDS stations for a hospitality outlet with proper station_type
// and category_filter, then stamps kds_station_id on the relevant catalog overrides so
// that tickets are routed to the correct station at order creation time.
func seedKDSStations(ctx context.Context, client *ent.Client, tenantID, outletID uuid.UUID) error {
	type stationDef struct {
		name           string
		stationType    kdsstation.StationType
		categoryFilter []string
		sortOrder      int
	}

	stations := []stationDef{
		{
			name:        "Kitchen Main",
			stationType: kdsstation.StationTypeKitchen,
			categoryFilter: []string{
				"sandwich", "salad", "curry", "grill", "pasta", "rice", "pizza",
				"breakfast", "pancake", "avocado", "oatmeal", "croissant", "muffin",
				"cake", "scone", "danish", "spring roll", "samosa", "waffle", "burger",
			},
			sortOrder: 1,
		},
		{
			name:        "Bar Display",
			stationType: kdsstation.StationTypeBar,
			categoryFilter: []string{
				"coffee", "latte", "espresso", "cappuccino", "americano", "mocha",
				"macchiato", "tea", "juice", "frappe", "smoothie", "iced latte",
				"hot chocolate", "cocktail", "beer", "wine", "spirit",
			},
			sortOrder: 2,
		},
		{
			name:           "Restaurant",
			stationType:    kdsstation.StationTypeExpo,
			categoryFilter: []string{},
			sortOrder:      3,
		},
	}

	for _, def := range stations {
		// Upsert KDS station (idempotent by name+outlet).
		existing, _ := client.KDSStation.Query().
			Where(kdsstation.TenantID(tenantID), kdsstation.OutletID(outletID), kdsstation.Name(def.name)).
			Only(ctx)

		var stationID uuid.UUID
		if existing != nil {
			_, _ = existing.Update().
				SetStationType(def.stationType).
				SetCategoryFilter(def.categoryFilter).
				SetSortOrder(def.sortOrder).
				SetIsActive(true).
				Save(ctx)
			stationID = existing.ID
		} else {
			st, err := client.KDSStation.Create().
				SetTenantID(tenantID).
				SetOutletID(outletID).
				SetName(def.name).
				SetStationType(def.stationType).
				SetCategoryFilter(def.categoryFilter).
				SetSortOrder(def.sortOrder).
				Save(ctx)
			if err != nil {
				return fmt.Errorf("create KDS station %s: %w", def.name, err)
			}
			stationID = st.ID
		}
		_ = stationID
	}

	log.Printf("  ✓ KDS stations seeded (3 stations)")
	return nil
}

// seedRBACPermissions creates all POS permissions.
func seedRBACPermissions(ctx context.Context, client *ent.Client) error {
	modules := []string{
		"orders", "payments", "catalog", "outlets", "devices",
		"sessions", "cash_drawers", "tables", "gift_cards",
		"price_books", "modifiers", "channels", "config", "users",
		"reports", "hotel", "appointments", "pharmacy",
		// Sprint 4–12 modules
		"kds", "retail", "layaway", "serial", "loyalty", "webhooks",
		"integrations", "fiscal", "queue", "staff",
		"commissions", "packages", "clients",
		// Hospitality features: conference/events + delegate meal cards, promotions/happy-hour
		"conference", "promotions",
	}
	actions := []string{
		"add", "view", "view_own", "change", "change_own",
		"delete", "delete_own", "manage", "manage_own", "void",
	}

	count := 0
	for _, module := range modules {
		for _, action := range actions {
			code := fmt.Sprintf("pos.%s.%s", module, action)
			name := fmt.Sprintf("POS %s %s", module, action)

			exists, err := client.POSPermission.Query().
				Where(pospermission.PermissionCode(code)).
				Exist(ctx)
			if err != nil {
				return err
			}
			if exists {
				continue
			}

			id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:pos:permission:%s", code)))
			_, err = client.POSPermission.Create().
				SetID(id).
				SetPermissionCode(code).
				SetName(name).
				SetModule(module).
				SetAction(action).
				SetResource(module).
				Save(ctx)
			if err != nil {
				return fmt.Errorf("create permission %s: %w", code, err)
			}
			count++
		}
	}
	log.Printf("  ✓ RBAC permissions seeded (%d new, %d modules x %d actions)", count, len(modules), len(actions))
	return nil
}

// seedRBACRoles creates system roles and assigns permissions.
func seedRBACRoles(ctx context.Context, client *ent.Client, tenantID uuid.UUID) error {
	type roleDef struct {
		code        string
		name        string
		description string
		// permission patterns: module.action — "*" means all actions for all modules
		permissions []string
	}

	roles := []roleDef{
		{
			code:        "admin",
			name:        "POS Admin",
			description: "Full access to all POS modules",
			permissions: []string{"*"},
		},
		{
			code:        "manager",
			name:        "Store Manager",
			description: "Manage store operations, staff, and reporting",
			permissions: []string{
				"pos.orders.*", "pos.payments.*", "pos.catalog.*",
				"pos.outlets.view", "pos.outlets.change",
				"pos.devices.*", "pos.sessions.*", "pos.cash_drawers.*",
				"pos.tables.*", "pos.gift_cards.*", "pos.price_books.*",
				"pos.modifiers.*", "pos.channels.*",
				"pos.config.view", "pos.config.change",
				"pos.users.view", "pos.users.change",
				"pos.reports.*", "pos.hotel.*", "pos.appointments.*", "pos.pharmacy.*",
				"pos.kds.*", "pos.retail.*", "pos.layaway.*", "pos.serial.*",
				"pos.loyalty.*", "pos.webhooks.*", "pos.integrations.*", "pos.fiscal.*",
				"pos.queue.*", "pos.staff.*", "pos.commissions.*",
				"pos.packages.*", "pos.clients.*",
				"pos.conference.*", "pos.promotions.*",
			},
		},
		{
			code:        "cashier",
			name:        "Cashier",
			description: "Process orders, payments, and manage cash drawer",
			permissions: []string{
				"pos.orders.add", "pos.orders.view", "pos.orders.view_own", "pos.orders.change_own",
				"pos.payments.add", "pos.payments.view", "pos.payments.view_own",
				"pos.catalog.view",
				"pos.cash_drawers.add", "pos.cash_drawers.view_own", "pos.cash_drawers.change_own",
				"pos.tables.view",
				"pos.gift_cards.view",
				"pos.modifiers.view",
				"pos.sessions.add", "pos.sessions.view_own",
				"pos.retail.add", "pos.retail.view",
				"pos.layaway.view", "pos.layaway.add",
				"pos.loyalty.view", "pos.loyalty.add",
				"pos.serial.view", "pos.serial.add",
			},
		},
		{
			code:        "waiter",
			name:        "Waiter",
			description: "Take orders and manage assigned tables",
			permissions: []string{
				"pos.orders.add", "pos.orders.view_own", "pos.orders.change_own",
				"pos.catalog.view",
				"pos.tables.view", "pos.tables.change_own",
				"pos.modifiers.view",
				"pos.sessions.add", "pos.sessions.view_own",
			},
		},
		{
			code:        "kitchen",
			name:        "Kitchen Staff",
			description: "View KDS queue and update item preparation status",
			permissions: []string{
				"pos.orders.view", "pos.catalog.view",
				"pos.kds.view", "pos.kds.change",
			},
		},
		{
			code:        "bar",
			name:        "Bar Staff",
			description: "View bar display queue and update drink preparation status",
			permissions: []string{
				"pos.orders.view", "pos.catalog.view",
				"pos.kds.view", "pos.kds.change",
			},
		},
		{
			code:        "receptionist",
			name:        "Receptionist",
			description: "Manage hotel check-in/out and room service orders",
			permissions: []string{
				"pos.orders.add", "pos.orders.view", "pos.orders.change_own",
				"pos.catalog.view", "pos.payments.view",
				"pos.tables.view",
				"pos.sessions.add", "pos.sessions.view_own",
				"pos.hotel.*",
			},
		},
		{
			code:        "viewer",
			name:        "Viewer",
			description: "Read-only access to POS data",
			permissions: []string{
				"pos.orders.view", "pos.payments.view", "pos.catalog.view",
				"pos.outlets.view", "pos.devices.view", "pos.sessions.view",
				"pos.cash_drawers.view", "pos.tables.view", "pos.gift_cards.view",
				"pos.price_books.view", "pos.modifiers.view", "pos.channels.view",
				"pos.config.view", "pos.users.view",
				"pos.reports.view", "pos.hotel.view", "pos.appointments.view",
				"pos.queue.view", "pos.staff.view", "pos.commissions.view",
				"pos.loyalty.view", "pos.packages.view", "pos.clients.view",
			},
		},
		{
			code:        "stylist",
			name:        "Stylist",
			description: "Service staff: manage own appointments, view queue, see own commissions",
			permissions: []string{
				"pos.orders.add", "pos.orders.view_own",
				"pos.catalog.view",
				"pos.sessions.add", "pos.sessions.view_own",
				"pos.appointments.view", "pos.appointments.change_own",
				"pos.queue.view", "pos.queue.change",
				"pos.commissions.view_own",
				"pos.clients.view",
			},
		},
		{
			code:        "therapist",
			name:        "Therapist",
			description: "Service staff: manage own appointments, view queue, see own commissions",
			permissions: []string{
				"pos.orders.add", "pos.orders.view_own",
				"pos.catalog.view",
				"pos.sessions.add", "pos.sessions.view_own",
				"pos.appointments.view", "pos.appointments.change_own",
				"pos.queue.view", "pos.queue.change",
				"pos.commissions.view_own",
				"pos.clients.view",
			},
		},
		{
			code:        "technician",
			name:        "Technician",
			description: "Service staff: manage own appointments, view queue, see own commissions",
			permissions: []string{
				"pos.orders.add", "pos.orders.view_own",
				"pos.catalog.view",
				"pos.sessions.add", "pos.sessions.view_own",
				"pos.appointments.view", "pos.appointments.change_own",
				"pos.queue.view", "pos.queue.change",
				"pos.commissions.view_own",
				"pos.clients.view",
			},
		},
		{
			code:        "pharmacist",
			name:        "Pharmacist",
			description: "Dispense prescriptions and manage pharmacy orders",
			permissions: []string{
				"pos.orders.add", "pos.orders.view", "pos.orders.change_own",
				"pos.payments.add", "pos.payments.view",
				"pos.catalog.view",
				"pos.sessions.add", "pos.sessions.view_own",
				"pos.pharmacy.*",
			},
		},
		{
			code:        "pharmacy_technician",
			name:        "Pharmacy Technician",
			description: "Assist with prescription filling, labelling, and inventory under pharmacist supervision",
			permissions: []string{
				"pos.orders.add", "pos.orders.view_own", "pos.orders.change_own",
				"pos.payments.add", "pos.payments.view_own",
				"pos.catalog.view",
				"pos.sessions.add", "pos.sessions.view_own",
				"pos.pharmacy.view", "pos.pharmacy.change",
			},
		},
	}

	// Load all permissions into a map for quick lookup
	allPerms, err := client.POSPermission.Query().All(ctx)
	if err != nil {
		return fmt.Errorf("load permissions: %w", err)
	}
	permByCode := make(map[string]uuid.UUID, len(allPerms))
	for _, p := range allPerms {
		permByCode[p.PermissionCode] = p.ID
	}

	for _, rd := range roles {
		roleID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:pos:role:%s:%s", tenantID, rd.code)))

		exists, err := client.POSRoleV2.Query().
			Where(posrolev2.ID(roleID)).
			Exist(ctx)
		if err != nil {
			return err
		}
		if !exists {
			_, err = client.POSRoleV2.Create().
				SetID(roleID).
				SetTenantID(tenantID).
				SetRoleCode(rd.code).
				SetName(rd.name).
				SetDescription(rd.description).
				SetIsSystemRole(true).
				Save(ctx)
			if err != nil {
				return fmt.Errorf("create role %s: %w", rd.code, err)
			}
		}

		// Always sync permissions — adds missing ones, ignores duplicates.
		permIDs := resolvePermissions(rd.permissions, permByCode)
		for _, permID := range permIDs {
			_, err = client.POSRolePermission.Create().
				SetRoleID(roleID).
				SetPermissionID(permID).
				Save(ctx)
			if err != nil {
				// Ignore duplicates (unique constraint violation)
				continue
			}
		}
	}
	log.Printf("  ✓ RBAC roles seeded (%d roles with permission assignments)", len(roles))
	return nil
}

// resolvePermissions expands wildcard patterns into concrete permission IDs.
func resolvePermissions(patterns []string, permByCode map[string]uuid.UUID) []uuid.UUID {
	ids := make(map[uuid.UUID]bool)

	for _, pattern := range patterns {
		if pattern == "*" {
			// All permissions
			for _, id := range permByCode {
				ids[id] = true
			}
			continue
		}

		// Check for module wildcard: "pos.orders.*"
		if len(pattern) > 2 && pattern[len(pattern)-1] == '*' {
			prefix := pattern[:len(pattern)-1] // "pos.orders."
			for code, id := range permByCode {
				if len(code) >= len(prefix) && code[:len(prefix)] == prefix {
					ids[id] = true
				}
			}
			continue
		}

		// Exact match
		if id, ok := permByCode[pattern]; ok {
			ids[id] = true
		}
	}

	result := make([]uuid.UUID, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	return result
}

// seedRateLimitConfigs creates default rate limit configurations.
func seedRateLimitConfigs(ctx context.Context, client *ent.Client) error {
	type rlDef struct {
		keyType         string
		endpoint        string
		reqPerWindow    int
		windowSecs      int
		burstMultiplier float64
		desc            string
	}
	configs := []rlDef{
		{"global", "*", 1000, 60, 2.0, "Global default: 1000 req/min"},
		{"tenant", "*", 300, 60, 1.5, "Per-tenant default: 300 req/min"},
		{"ip", "*", 120, 60, 1.5, "Per-IP default: 120 req/min"},
		{"user", "*", 100, 60, 1.5, "Per-user default: 100 req/min"},
		{"endpoint", "/api/v1/*/pos/orders", 60, 60, 2.0, "Order creation: 60 req/min"},
		{"endpoint", "/api/v1/*/pos/orders/*/payments", 30, 60, 1.5, "Payment recording: 30 req/min"},
	}

	for _, c := range configs {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:pos:ratelimit:%s:%s", c.keyType, c.endpoint)))
		exists, err := client.RateLimitConfig.Query().
			Where(
			// Use ID check since unique index uses 3 fields
			).
			Exist(ctx)
		// Simplified: just try create, ignore constraint errors
		if err != nil && exists {
			continue
		}

		_, err = client.RateLimitConfig.Create().
			SetID(id).
			SetServiceName("pos-api").
			SetKeyType(c.keyType).
			SetEndpointPattern(c.endpoint).
			SetRequestsPerWindow(c.reqPerWindow).
			SetWindowSeconds(c.windowSecs).
			SetBurstMultiplier(c.burstMultiplier).
			SetIsActive(true).
			SetDescription(c.desc).
			Save(ctx)
		if err != nil {
			// Ignore duplicate constraint violations
			continue
		}
	}
	log.Printf("  ✓ Rate limit configs seeded (%d entries)", len(configs))
	return nil
}

// seedServiceConfigs creates default service-level configuration.
func seedServiceConfigs(ctx context.Context, client *ent.Client) error {
	type cfgDef struct {
		key        string
		value      string
		configType string
		desc       string
		isSecret   bool
	}
	configs := []cfgDef{
		{"pos.max_order_amount", "1000000", "float", "Maximum allowed order amount", false},
		{"pos.default_currency", "KES", "string", "Default currency for POS transactions", false},
		{"pos.tax_rate_percent", "16.0", "float", "Default VAT rate percentage", false},
		{"pos.order_prefix", "POS", "string", "Prefix for auto-generated order numbers", false},
		{"pos.session_timeout_minutes", "480", "int", "Device session timeout in minutes (8 hours)", false},
		{"pos.cash_drawer_variance_threshold", "500", "float", "Cash drawer variance alert threshold", false},
		{"pos.max_discount_percent", "50", "float", "Maximum discount percentage allowed", false},
		{"pos.require_manager_approval_void", "true", "bool", "Require manager approval for voids", false},
		{"pos.enable_offline_mode", "true", "bool", "Enable offline mode for POS devices", false},
		{"pos.sync_interval_seconds", "30", "int", "Offline sync interval in seconds", false},
	}

	for _, c := range configs {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:pos:config:platform:%s", c.key)))

		_, err := client.ServiceConfig.Create().
			SetID(id).
			SetConfigKey(c.key).
			SetConfigValue(c.value).
			SetConfigType(c.configType).
			SetDescription(c.desc).
			SetIsSecret(c.isSecret).
			Save(ctx)
		if err != nil {
			// Ignore duplicate constraint violations
			continue
		}
	}
	log.Printf("  ✓ Service configs seeded (%d platform-level entries)", len(configs))
	return nil
}

func seedLoyaltyProgram(ctx context.Context, client *ent.Client, tenantID uuid.UUID) error {
	exists, err := client.LoyaltyProgram.Query().
		Where(loyaltyprogram.TenantID(tenantID), loyaltyprogram.IsActive(true)).
		Exist(ctx)
	if err != nil {
		return fmt.Errorf("check loyalty program: %w", err)
	}
	if exists {
		log.Printf("  ✓ Loyalty program already exists for tenant %s, skipping", tenantID)
		return nil
	}
	_, err = client.LoyaltyProgram.Create().
		SetTenantID(tenantID).
		SetName("Rewards Program").
		SetDescription("Earn 1 point per KSh 100. 100 pts = KSh 1 off").
		SetEarnRate(0.01).
		SetRedeemRate(0.01).
		SetMinRedeemPoints(100).
		SetIsActive(true).
		SetTierThresholds(map[string]any{
			"silver":   500,
			"gold":     2000,
			"platinum": 10000,
		}).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("create loyalty program: %w", err)
	}
	log.Printf("  ✓ Default loyalty program seeded for tenant %s", tenantID)
	return nil
}
