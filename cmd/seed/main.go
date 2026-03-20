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
	"github.com/bengobox/pos-service/internal/ent/catalogitem"
	"github.com/bengobox/pos-service/internal/ent/outlet"
	"github.com/bengobox/pos-service/internal/ent/section"
	enttable "github.com/bengobox/pos-service/internal/ent/table"
	"github.com/bengobox/pos-service/internal/ent/tender"
	"github.com/bengobox/pos-service/internal/modules/tenant"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
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
	db.SetMaxIdleConns(10)
	db.SetMaxOpenConns(25)
	db.SetConnMaxIdleTime(5 * time.Minute)

	driver := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(ent.Driver(driver))
	defer client.Close()

	// Sync tenant from auth-api
	syncer := tenant.NewSyncer(client)
	tenantID, err := syncer.SyncTenant(ctx, "urban-loft")
	if err != nil {
		log.Fatalf("sync tenant: %v", err)
	}

	if err := runSeed(ctx, client, tenantID); err != nil {
		log.Fatalf("seed data: %v", err)
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

func runSeed(ctx context.Context, client *ent.Client, tenantID uuid.UUID) error {
	outletID, err := seedOutlet(ctx, client, tenantID)
	if err != nil {
		return fmt.Errorf("seed outlet: %w", err)
	}

	if err := seedTenders(ctx, client, tenantID, outletID); err != nil {
		return fmt.Errorf("seed tenders: %w", err)
	}

	sectionIDs, err := seedSections(ctx, client, tenantID, outletID)
	if err != nil {
		return fmt.Errorf("seed sections: %w", err)
	}

	if err := seedTables(ctx, client, tenantID, outletID, sectionIDs); err != nil {
		return fmt.Errorf("seed tables: %w", err)
	}

	if err := seedCatalogItems(ctx, client, tenantID); err != nil {
		return fmt.Errorf("seed catalog items: %w", err)
	}

	return nil
}

func seedOutlet(ctx context.Context, client *ent.Client, tenantID uuid.UUID) (uuid.UUID, error) {
	outletID := outletUUID("urban-loft", "busia")

	existing, err := client.Outlet.Query().Where(outlet.ID(outletID)).Only(ctx)
	if err == nil {
		return existing.ID, nil
	}
	if !ent.IsNotFound(err) {
		return uuid.Nil, err
	}

	o, err := client.Outlet.Create().
		SetID(outletID).
		SetTenantID(tenantID).
		SetTenantSlug("urban-loft").
		SetCode("BUSIA").
		SetName("Urban Loft Cafe Busia").
		SetChannelType("physical").
		SetStatus("active").
		SetUseCase("hospitality").
		Save(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	log.Printf("  ✓ Outlet created: %s (ID=%s)", o.Name, o.ID)
	return o.ID, nil
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

// seedCatalogItems creates catalog items as projections from inventory-api master data.
// Uses the same deterministic UUID formula as inventory-api for inventory_item_id alignment.
func seedCatalogItems(ctx context.Context, client *ent.Client, tenantID uuid.UUID) error {
	type itemDef struct {
		sku      string
		name     string
		category string
	}
	// Items aligned with inventory-api catalogItemDefs — same SKUs and names.
	items := []itemDef{
		{"BEV-ESP-001", "Espresso", "hot-beverages"},
		{"BEV-ESP-002", "Double Espresso", "hot-beverages"},
		{"BEV-LAT-001", "Caffe Latte", "hot-beverages"},
		{"BEV-CAP-001", "Cappuccino", "hot-beverages"},
		{"BEV-AME-001", "Americano", "hot-beverages"},
		{"BEV-MOC-001", "Mocha", "hot-beverages"},
		{"BEV-MAC-001", "Macchiato", "hot-beverages"},
		{"BEV-TEA-001", "Kenya AA Black Tea", "hot-beverages"},
		{"BEV-TEA-002", "Masala Chai", "hot-beverages"},
		{"BEV-HOT-001", "Hot Chocolate", "hot-beverages"},
		{"BEV-ICE-001", "Iced Latte", "cold-beverages"},
		{"BEV-ICE-002", "Iced Americano", "cold-beverages"},
		{"BEV-FRP-001", "Caramel Frappe", "cold-beverages"},
		{"BEV-FRP-002", "Vanilla Frappe", "cold-beverages"},
		{"BEV-SMO-001", "Mango Smoothie", "cold-beverages"},
		{"BEV-SMO-002", "Mixed Berry Smoothie", "cold-beverages"},
		{"BEV-JCE-001", "Fresh Orange Juice", "cold-beverages"},
		{"PST-CRO-001", "Butter Croissant", "pastries"},
		{"PST-CRO-002", "Chocolate Croissant", "pastries"},
		{"PST-MUF-001", "Blueberry Muffin", "pastries"},
		{"PST-MUF-002", "Banana Walnut Muffin", "pastries"},
		{"PST-CKE-001", "Carrot Cake Slice", "pastries"},
		{"PST-CKE-002", "Red Velvet Cake Slice", "pastries"},
		{"PST-CKE-003", "Chocolate Fudge Cake Slice", "pastries"},
		{"PST-DAN-001", "Danish Pastry", "pastries"},
		{"PST-SCO-001", "Classic Scone", "pastries"},
		{"SND-CLB-001", "Club Sandwich", "sandwiches"},
		{"SND-GRL-001", "Grilled Chicken Panini", "sandwiches"},
		{"SND-VEG-001", "Veggie Wrap", "sandwiches"},
		{"SND-BLT-001", "BLT Sandwich", "sandwiches"},
		{"SND-TUN-001", "Tuna Melt", "sandwiches"},
		{"SAL-CES-001", "Caesar Salad", "salads"},
		{"SAL-GRK-001", "Greek Salad", "salads"},
		{"BTE-SAM-001", "Samosa (3pc)", "light-bites"},
		{"BTE-SPR-001", "Spring Rolls (4pc)", "light-bites"},
		{"MIN-GRL-001", "Grilled Beef Fillet", "main-courses"},
		{"MIN-GRL-002", "Grilled Chicken Breast", "main-courses"},
		{"MIN-CUR-001", "Chicken Curry", "main-courses"},
		{"MIN-CUR-002", "Beef Stew", "main-courses"},
		{"MIN-SEA-001", "Fish and Chips", "main-courses"},
		{"MIN-PAS-001", "Spaghetti Bolognese", "main-courses"},
		{"MIN-RIC-001", "Pilau Rice Bowl", "main-courses"},
		{"BRK-FUL-001", "Full English Breakfast", "breakfast"},
		{"BRK-PAN-001", "Pancake Stack", "breakfast"},
		{"BRK-AVT-001", "Avocado Toast", "breakfast"},
		{"BRK-OAT-001", "Overnight Oats", "breakfast"},
		{"PIZ-MAR-001", "Margherita Pizza", "pizza"},
		{"PIZ-PEP-001", "Pepperoni Pizza", "pizza"},
	}

	for _, it := range items {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:pos:item:%s:%s", tenantID, it.sku)))

		exists, err := client.CatalogItem.Query().Where(catalogitem.ID(id)).Exist(ctx)
		if err != nil {
			return err
		}
		if exists {
			continue
		}

		_, err = client.CatalogItem.Create().
			SetID(id).
			SetTenantID(tenantID).
			SetSku(it.sku).
			SetName(it.name).
			SetCategory(it.category).
			SetTaxStatus("taxable").
			SetStatus("active").
			Save(ctx)
		if err != nil {
			return fmt.Errorf("create catalog item %s: %w", it.name, err)
		}
	}
	log.Printf("  ✓ Catalog items seeded (%d items from inventory-api master data)", len(items))
	return nil
}
