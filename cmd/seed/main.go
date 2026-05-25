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
	"golang.org/x/crypto/bcrypt"

	"github.com/bengobox/pos-service/internal/config"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/catalogitem"
	"github.com/bengobox/pos-service/internal/ent/outlet"
	"github.com/bengobox/pos-service/internal/ent/outletsetting"
	"github.com/bengobox/pos-service/internal/ent/pospermission"
	"github.com/bengobox/pos-service/internal/ent/posrolev2"
	"github.com/bengobox/pos-service/internal/ent/section"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	enttable "github.com/bengobox/pos-service/internal/ent/table"
	"github.com/bengobox/pos-service/internal/ent/tender"
	"github.com/bengobox/pos-service/internal/modules/tenant"
)

// tenantSeedConfig controls what data is seeded for each tenant.
type tenantSeedConfig struct {
	slug        string // must match auth-api tenant slug
	seedStaff   bool   // seed demo staff members (PINs)
	seedTables  bool   // seed tables & sections (hospitality outlets only)
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

	// Tenants to seed — order matters: urban-loft first (real client), then demo.
	tenantConfigs := []tenantSeedConfig{
		// urban-loft: real hospitality client — BUSIA outlet only; no demo staff.
		{slug: "urban-loft", seedStaff: false, seedTables: true},
		// codevertex-demo: cross-platform demo tenant — 5 use-case outlets + demo staff.
		{slug: "codevertex-demo", seedStaff: true, seedTables: true},
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

	if err := seedCatalogItems(ctx, client, tenantID, tc.slug); err != nil {
		return fmt.Errorf("seed catalog items: %w", err)
	}

	if err := seedRBACPermissions(ctx, client); err != nil {
		return fmt.Errorf("seed RBAC permissions: %w", err)
	}

	if err := seedRBACRoles(ctx, client, tenantID); err != nil {
		return fmt.Errorf("seed RBAC roles: %w", err)
	}

	if tc.seedStaff {
		if err := seedStaffMembers(ctx, client, tenantID, hqOutletID); err != nil {
			return fmt.Errorf("seed staff members: %w", err)
		}
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
	"urban-loft": {
		// Single hospitality HQ — hotel, bar, grill, cafe, accommodations.
		{
			slug:        "busia",
			code:        "BUSIA",
			name:        "Urban Loft Cafe Busia",
			useCase:     "hospitality",
			isHQ:        true,
			pinMessage:  "Welcome to Urban Loft Cafe — Shift starts 7:00 AM",
			displayMode: "card",
			enableKDS:   true,
			enableAppts: false,
			enableHotel: true,
			defaultView: "tables",
		},
	},
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

// seedCatalogItems creates outlet-scoped catalog items per use case.
// Each outlet gets its own items so that, e.g., a retail cashier sees grocery
// items and a services staff member sees appointment services — not cafe menus.
// For tenants without per-use-case outlets (e.g. urban-loft), items are created
// tenant-wide (no outlet_id) so they appear at all outlets.
func seedCatalogItems(ctx context.Context, client *ent.Client, tenantID uuid.UUID, tenantSlug string) error {
	type itemDef struct {
		sku                  string
		name                 string
		category             string
		itemType             string // GOODS | SERVICE | RECIPE
		outletSlug           string // empty = tenant-wide (no outlet_id)
		durationMinutes      int    // services only
		isControlledSubstance bool  // pharmacy scheduled drugs
	}

	// Derive outlet UUIDs deterministically — same formula used in seedOutlets.
	outletID := func(slug string) *uuid.UUID {
		if slug == "" {
			return nil
		}
		id := outletUUID(tenantSlug, slug)
		return &id
	}

	// ── Hospitality (demo-hospitality / HOSP) ────────────────────────────────
	// Classic hotel & restaurant menu, scoped to the hospitality outlet.
	hospSlug := "demo-hospitality"
	if tenantSlug == "urban-loft" {
		hospSlug = "busia"
	}

	items := []itemDef{
		// Hot beverages
		{"BEV-ESP-001", "Espresso", "hot-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-ESP-002", "Double Espresso", "hot-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-LAT-001", "Caffe Latte", "hot-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-CAP-001", "Cappuccino", "hot-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-AME-001", "Americano", "hot-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-MOC-001", "Mocha", "hot-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-MAC-001", "Macchiato", "hot-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-TEA-001", "Kenya AA Black Tea", "hot-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-TEA-002", "Masala Chai", "hot-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-HOT-001", "Hot Chocolate", "hot-beverages", "GOODS", hospSlug, 0, false},
		// Cold beverages
		{"BEV-ICE-001", "Iced Latte", "cold-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-ICE-002", "Iced Americano", "cold-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-FRP-001", "Caramel Frappe", "cold-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-FRP-002", "Vanilla Frappe", "cold-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-SMO-001", "Mango Smoothie", "cold-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-SMO-002", "Mixed Berry Smoothie", "cold-beverages", "GOODS", hospSlug, 0, false},
		{"BEV-JCE-001", "Fresh Orange Juice", "cold-beverages", "GOODS", hospSlug, 0, false},
		// Pastries
		{"PST-CRO-001", "Butter Croissant", "pastries", "GOODS", hospSlug, 0, false},
		{"PST-CRO-002", "Chocolate Croissant", "pastries", "GOODS", hospSlug, 0, false},
		{"PST-MUF-001", "Blueberry Muffin", "pastries", "GOODS", hospSlug, 0, false},
		{"PST-MUF-002", "Banana Walnut Muffin", "pastries", "GOODS", hospSlug, 0, false},
		{"PST-CKE-001", "Carrot Cake Slice", "pastries", "GOODS", hospSlug, 0, false},
		{"PST-CKE-002", "Red Velvet Cake Slice", "pastries", "GOODS", hospSlug, 0, false},
		{"PST-CKE-003", "Chocolate Fudge Cake Slice", "pastries", "GOODS", hospSlug, 0, false},
		{"PST-DAN-001", "Danish Pastry", "pastries", "GOODS", hospSlug, 0, false},
		{"PST-SCO-001", "Classic Scone", "pastries", "GOODS", hospSlug, 0, false},
		// Sandwiches & salads
		{"SND-CLB-001", "Club Sandwich", "sandwiches", "GOODS", hospSlug, 0, false},
		{"SND-GRL-001", "Grilled Chicken Panini", "sandwiches", "GOODS", hospSlug, 0, false},
		{"SND-VEG-001", "Veggie Wrap", "sandwiches", "GOODS", hospSlug, 0, false},
		{"SND-BLT-001", "BLT Sandwich", "sandwiches", "GOODS", hospSlug, 0, false},
		{"SND-TUN-001", "Tuna Melt", "sandwiches", "GOODS", hospSlug, 0, false},
		{"SAL-CES-001", "Caesar Salad", "salads", "GOODS", hospSlug, 0, false},
		{"SAL-GRK-001", "Greek Salad", "salads", "GOODS", hospSlug, 0, false},
		// Light bites & mains
		{"BTE-SAM-001", "Samosa (3pc)", "light-bites", "GOODS", hospSlug, 0, false},
		{"BTE-SPR-001", "Spring Rolls (4pc)", "light-bites", "GOODS", hospSlug, 0, false},
		{"MIN-GRL-001", "Grilled Beef Fillet", "main-courses", "GOODS", hospSlug, 0, false},
		{"MIN-GRL-002", "Grilled Chicken Breast", "main-courses", "GOODS", hospSlug, 0, false},
		{"MIN-CUR-001", "Chicken Curry", "main-courses", "GOODS", hospSlug, 0, false},
		{"MIN-CUR-002", "Beef Stew", "main-courses", "GOODS", hospSlug, 0, false},
		{"MIN-SEA-001", "Fish and Chips", "main-courses", "GOODS", hospSlug, 0, false},
		{"MIN-PAS-001", "Spaghetti Bolognese", "main-courses", "GOODS", hospSlug, 0, false},
		{"MIN-RIC-001", "Pilau Rice Bowl", "main-courses", "GOODS", hospSlug, 0, false},
		// Breakfast & pizza
		{"BRK-FUL-001", "Full English Breakfast", "breakfast", "GOODS", hospSlug, 0, false},
		{"BRK-PAN-001", "Pancake Stack", "breakfast", "GOODS", hospSlug, 0, false},
		{"BRK-AVT-001", "Avocado Toast", "breakfast", "GOODS", hospSlug, 0, false},
		{"BRK-OAT-001", "Overnight Oats", "breakfast", "GOODS", hospSlug, 0, false},
		{"PIZ-MAR-001", "Margherita Pizza", "pizza", "GOODS", hospSlug, 0, false},
		{"PIZ-PEP-001", "Pepperoni Pizza", "pizza", "GOODS", hospSlug, 0, false},
	}

	// ── Retail (demo-retail / RETAIL) — only seeded for codevertex-demo ────────
	if tenantSlug == "codevertex-demo" {
		items = append(items, []itemDef{
			{"RTL-MLK-001", "Whole Milk 1L", "dairy", "GOODS", "demo-retail", 0, false},
			{"RTL-BRD-001", "White Bread Loaf", "bakery", "GOODS", "demo-retail", 0, false},
			{"RTL-RIC-001", "Basmati Rice 2kg", "grains", "GOODS", "demo-retail", 0, false},
			{"RTL-OIL-001", "Sunflower Cooking Oil 1L", "cooking-essentials", "GOODS", "demo-retail", 0, false},
			{"RTL-SGR-001", "White Sugar 1kg", "cooking-essentials", "GOODS", "demo-retail", 0, false},
			{"RTL-EGG-001", "Eggs Tray 30pc", "dairy", "GOODS", "demo-retail", 0, false},
			{"RTL-DET-001", "Washing Powder 500g", "household", "GOODS", "demo-retail", 0, false},
		}...)

		// ── Quick Service (demo-quick / QSR) ─────────────────────────────────
		items = append(items, []itemDef{
			{"QSR-BUR-001", "Classic Beef Burger", "burgers", "GOODS", "demo-quick", 0, false},
			{"QSR-CHK-001", "Crispy Chicken Burger", "burgers", "GOODS", "demo-quick", 0, false},
			{"QSR-FRI-001", "Large Fries", "sides", "GOODS", "demo-quick", 0, false},
			{"QSR-SOD-001", "Soft Drink 500ml", "beverages", "GOODS", "demo-quick", 0, false},
			{"QSR-HOT-001", "Beef Hot Dog", "snacks", "GOODS", "demo-quick", 0, false},
			{"QSR-PIZ-001", "Slice Pizza", "snacks", "GOODS", "demo-quick", 0, false},
			{"QSR-COM-001", "Burger + Fries Combo", "combos", "RECIPE", "demo-quick", 0, false},
		}...)

		// ── Pharmacy (demo-pharmacy / PHARMA) ────────────────────────────────
		items = append(items, []itemDef{
			{"PHM-PAR-001", "Paracetamol 500mg 24pk", "otc-pain-relief", "GOODS", "demo-pharmacy", 0, false},
			{"PHM-IBU-001", "Ibuprofen 400mg 16pk", "otc-pain-relief", "GOODS", "demo-pharmacy", 0, false},
			{"PHM-ANT-001", "Amoxicillin 250mg 21pk", "antibiotics", "GOODS", "demo-pharmacy", 0, true},
			{"PHM-VIT-001", "Vitamin C 1000mg 30pk", "vitamins-supplements", "GOODS", "demo-pharmacy", 0, false},
			{"PHM-ORS-001", "ORS Sachets 6pk", "rehydration", "GOODS", "demo-pharmacy", 0, false},
			{"PHM-MAS-001", "Surgical Face Mask 10pk", "medical-supplies", "GOODS", "demo-pharmacy", 0, false},
			{"PHM-CON-001", "Pharmacist Consultation", "consultations", "SERVICE", "demo-pharmacy", 15, false},
		}...)

		// ── Services (demo-services / SVC) ───────────────────────────────────
		items = append(items, []itemDef{
			{"SVC-HCT-001", "Ladies Haircut & Style", "hair", "SERVICE", "demo-services", 60, false},
			{"SVC-HCM-001", "Mens Haircut", "hair", "SERVICE", "demo-services", 30, false},
			{"SVC-MAN-001", "Classic Manicure", "nails", "SERVICE", "demo-services", 45, false},
			{"SVC-PED-001", "Classic Pedicure", "nails", "SERVICE", "demo-services", 60, false},
			{"SVC-FAC-001", "Deep Cleansing Facial", "skin", "SERVICE", "demo-services", 60, false},
			{"SVC-MSG-001", "Swedish Massage 60min", "massage", "SERVICE", "demo-services", 60, false},
			{"SVC-EYB-001", "Eyebrow Threading", "hair", "SERVICE", "demo-services", 20, false},
		}...)
	}

	created := 0
	updated := 0
	for _, it := range items {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:pos:item:%s:%s", tenantID, it.sku)))
		oid := outletID(it.outletSlug)

		existing, err := client.CatalogItem.Query().Where(catalogitem.ID(id)).Only(ctx)
		if err != nil && !ent.IsNotFound(err) {
			return err
		}

		if existing != nil {
			// Update outlet_id and item_type on existing records for idempotency.
			upd := client.CatalogItem.UpdateOneID(id).
				SetItemType(it.itemType)
			if oid != nil {
				upd = upd.SetOutletID(*oid)
			}
			if it.isControlledSubstance {
				upd = upd.SetIsControlledSubstance(true)
			}
			if it.durationMinutes > 0 {
				upd = upd.SetDurationMinutes(it.durationMinutes)
			}
			if _, err := upd.Save(ctx); err != nil {
				log.Printf("  ⚠️  update catalog item %s: %v", it.sku, err)
			} else {
				updated++
			}
			continue
		}

		create := client.CatalogItem.Create().
			SetID(id).
			SetTenantID(tenantID).
			SetSku(it.sku).
			SetName(it.name).
			SetCategory(it.category).
			SetItemType(it.itemType).
			SetTaxStatus("taxable").
			SetStatus("active")
		if oid != nil {
			create = create.SetOutletID(*oid)
		}
		if it.isControlledSubstance {
			create = create.SetIsControlledSubstance(true)
		}
		if it.durationMinutes > 0 {
			create = create.SetDurationMinutes(it.durationMinutes)
		}

		if _, err := create.Save(ctx); err != nil {
			return fmt.Errorf("create catalog item %s: %w", it.name, err)
		}
		created++
	}
	log.Printf("  ✓ Catalog items: %d created, %d updated (%d total)", created, updated, len(items))
	return nil
}

// seedRBACPermissions creates all POS permissions.
func seedRBACPermissions(ctx context.Context, client *ent.Client) error {
	modules := []string{
		"orders", "payments", "catalog", "outlets", "devices",
		"sessions", "cash_drawers", "tables", "gift_cards",
		"price_books", "modifiers", "channels", "config", "users",
		"reports", "hotel", "appointments", "pharmacy",
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
				"pos.modifiers.*", "pos.channels.view",
				"pos.config.view", "pos.config.change",
				"pos.users.view", "pos.users.change",
				"pos.reports.*", "pos.hotel.*", "pos.appointments.*", "pos.pharmacy.*",
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
			},
		},
		{
			code:        "bar",
			name:        "Bar Staff",
			description: "View bar display queue and update drink preparation status",
			permissions: []string{
				"pos.orders.view", "pos.catalog.view",
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
		if exists {
			continue
		}

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

		// Resolve permission codes and assign
		permIDs := resolvePermissions(rd.permissions, permByCode)
		for _, permID := range permIDs {
			_, err = client.POSRolePermission.Create().
				SetRoleID(roleID).
				SetPermissionID(permID).
				Save(ctx)
			if err != nil {
				// Ignore duplicates
				continue
			}
		}
	}
	log.Printf("  ✓ RBAC roles seeded (%d roles with permission assignments)", len(roles))
	return nil
}

func seedStaffMembers(ctx context.Context, client *ent.Client, tenantID, outletID uuid.UUID) error {
	type staffDef struct {
		name string
		role string
	}
	staff := []staffDef{
		{"Alice Manager", "manager"},
		{"Bob Cashier", "cashier"},
		{"Carol Waiter", "waiter"},
		{"David Kitchen", "kitchen"},
		{"Eve Bartender", "bar"},
		{"Frank Receptionist", "receptionist"},
		{"Grace Pharmacist", "pharmacist"},
	}

	// All demo staff share PIN "1234" so the kiosk is immediately usable.
	pinHashBytes, err := bcrypt.GenerateFromPassword([]byte("1234"), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("generate demo pin hash: %w", err)
	}
	pinHash := string(pinHashBytes)

	for _, s := range staff {
		uid := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:pos:staff:%s:%s", tenantID, s.name)))

		existing, err := client.StaffMember.Query().
			Where(entstaff.TenantID(tenantID), entstaff.UserID(uid)).
			Only(ctx)
		if err != nil && !ent.IsNotFound(err) {
			log.Printf("  ⚠️  query staff %s: %v", s.name, err)
			continue
		}

		if existing != nil {
			// Update pin_hash on existing record so re-runs always set the demo PIN.
			_, err = client.StaffMember.UpdateOneID(existing.ID).
				SetPinHash(pinHash).
				SetPinFailedAttempts(0).
				ClearPinLockedUntil().
				Save(ctx)
			if err != nil {
				log.Printf("  ⚠️  update pin for %s: %v", s.name, err)
			} else {
				log.Printf("  ✓ Staff PIN refreshed: %s (%s)", s.name, s.role)
			}
			continue
		}

		_, err = client.StaffMember.Create().
			SetTenantID(tenantID).
			SetOutletID(outletID).
			SetUserID(uid).
			SetName(s.name).
			SetRole(s.role).
			SetIsActive(true).
			SetPinHash(pinHash).
			SetPinFailedAttempts(0).
			Save(ctx)
		if err != nil {
			log.Printf("  ⚠️  seed staff %s: %v", s.name, err)
		} else {
			log.Printf("  ✓ Staff seeded: %s (%s) [PIN=1234]", s.name, s.role)
		}
	}
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
