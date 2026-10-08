package kdsroute

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/kdsstation"
)

func station(id uuid.UUID, typ kdsstation.StationType, sort int, filter ...string) *ent.KDSStation {
	return &ent.KDSStation{ID: id, Name: string(typ), StationType: typ, SortOrder: sort, IsActive: true, CategoryFilter: filter}
}

func mustOwn(t *testing.T, r *Router, name, category string, pinned *uuid.UUID, want uuid.UUID) {
	t.Helper()
	got := r.OwnerOrFallback(name, category, pinned)
	if got == nil || *got != want {
		t.Fatalf("OwnerOrFallback(%q, %q) = %v, want %v", name, category, got, want)
	}
}

func TestKeyNormalisesSpellingVariants(t *testing.T) {
	same := [][2]string{
		{"Coffees", "Coffee"},
		{"Freakyshakes", "Freaky Shakes"},
		{"Burger", "Burgers"},
		{"Sandwich", "Sandwiches"},
		{"Accompaniment", "Accompaniments"},
		{"Beers & Ciders", "beers and ciders"},
		{"  Water & Soft Drinks ", "Water and Soft Drink"},
		{"Glass", "Glasses"},
	}
	for _, p := range same {
		if Key(p[0]) != Key(p[1]) {
			t.Errorf("Key(%q)=%q != Key(%q)=%q", p[0], Key(p[0]), p[1], Key(p[1]))
		}
	}
	if Key("Teas") == Key("Tequila") || Key("Gin") == Key("Wine") {
		t.Error("distinct categories must not collide")
	}
	if Key("") != "" || Key(" & ") != "and" {
		t.Errorf("edge keys: %q %q", Key(""), Key(" & "))
	}
}

// 2026-08 urban-loft regression: a cafe's Bar station that claims hot drinks wins over the
// "hot beverages go to the kitchen" guess.
func TestCategoryFilterWinsOverHotBeverageGuess(t *testing.T) {
	kitchen, bar := uuid.New(), uuid.New()
	r := New([]*ent.KDSStation{
		station(kitchen, kdsstation.StationTypeKitchen, 0, "Main Dishes", "Salad"),
		station(bar, kdsstation.StationTypeBar, 1, "Coffees", "Teas", "Milkshakes"),
	}, nil)
	mustOwn(t, r, "Mixed Tea", "Teas", nil, bar)
	mustOwn(t, r, "Hot Water Lemon", "Teas", nil, bar)
	mustOwn(t, r, "White Mocha Double", "Coffees", nil, bar)
	mustOwn(t, r, "Mocha Shake", "Milkshakes", nil, bar)
	mustOwn(t, r, "Grilled Chicken", "Main Dishes", nil, kitchen)
}

func TestHotBeverageGuessOnlyWhenUnclaimed(t *testing.T) {
	kitchen, bar := uuid.New(), uuid.New()
	r := New([]*ent.KDSStation{
		station(bar, kdsstation.StationTypeBar, 0, "Cocktails"),
		station(kitchen, kdsstation.StationTypeKitchen, 1, "Main Dishes"),
	}, nil)
	mustOwn(t, r, "Cappuccino", "", nil, kitchen)
	mustOwn(t, r, "Iced Coffee", "", nil, bar) // not hot: falls back to the first station
}

// 2026-10 urban-loft incident: after the menu rebuild renamed and nested the drinks categories,
// every drink the Bar filter no longer named exactly went to the kitchen printer.
func TestRenamedAndNestedCategoriesStillReachTheBar(t *testing.T) {
	kitchen, bar := uuid.New(), uuid.New()
	tree := NewTree([]Category{
		{ID: "coffee", Name: "Coffee"},
		{ID: "iced", Name: "Iced Coffee", ParentID: "coffee"},
		{ID: "wines", Name: "Wines"},
		{ID: "red", Name: "Red Wines", ParentID: "wines"},
		{ID: "house", Name: "House Wine", ParentID: "wines"},
		{ID: "juices", Name: "Juices"},
		{ID: "smoothies", Name: "Smoothies", ParentID: "juices"},
		{ID: "mains", Name: "Main Dishes"},
		{ID: "kids", Name: "Kids Corner", ParentID: "mains"},
		{ID: "fs", Name: "Freaky Shakes"},
	})
	r := New([]*ent.KDSStation{
		station(kitchen, kdsstation.StationTypeKitchen, 0, "Main Dishes"),
		station(bar, kdsstation.StationTypeBar, 1, "Coffees", "Wines", "Juices", "Freakyshakes"),
	}, tree)

	mustOwn(t, r, "Americano", "Coffee", nil, bar)               // plural/singular rename
	mustOwn(t, r, "Iced Latte", "Iced Coffee", nil, bar)         // inherits Coffee
	mustOwn(t, r, "Merlot Glass", "House Wine", nil, bar)        // inherits Wines
	mustOwn(t, r, "Cabernet", "Red Wines", nil, bar)             // inherits Wines
	mustOwn(t, r, "Mango Smoothie", "Smoothies", nil, bar)       // inherits Juices
	mustOwn(t, r, "Oreo Freakyshake", "Freaky Shakes", nil, bar) // spacing variant
	mustOwn(t, r, "Chicken Schnitzel", "Kids Corner", nil, kitchen)
}

// The closest claim wins: a station can own one sub-category of another station's section.
func TestClosestClaimWins(t *testing.T) {
	kitchen, bar := uuid.New(), uuid.New()
	tree := NewTree([]Category{
		{ID: "mains", Name: "Main Dishes"},
		{ID: "kids", Name: "Kids Corner", ParentID: "mains"},
		{ID: "shakes", Name: "Kids Shakes", ParentID: "kids"},
	})
	r := New([]*ent.KDSStation{
		station(kitchen, kdsstation.StationTypeKitchen, 0, "Main Dishes"),
		station(bar, kdsstation.StationTypeBar, 1, "Kids Shakes"),
	}, tree)
	mustOwn(t, r, "Kids Shake", "Kids Shakes", nil, bar)
	mustOwn(t, r, "Kiddie Fries", "Kids Corner", nil, kitchen)
}

func TestPinWinsOnlyForALiveStationOfThisOutlet(t *testing.T) {
	kitchen, bar, elsewhere := uuid.New(), uuid.New(), uuid.New()
	r := New([]*ent.KDSStation{
		station(kitchen, kdsstation.StationTypeKitchen, 0, "Teas"),
		station(bar, kdsstation.StationTypeBar, 1),
	}, nil)
	mustOwn(t, r, "Mixed Tea", "Teas", &bar, bar)
	// A tenant-wide pin naming another outlet's (or a deactivated) station is ignored.
	mustOwn(t, r, "Mixed Tea", "Teas", &elsewhere, kitchen)
}

func TestFallbackIsStableAndPrefersCopyStations(t *testing.T) {
	kitchen, bar, expo := uuid.New(), uuid.New(), uuid.New()
	// Query order must not matter: sort_order decides the fallback.
	r := New([]*ent.KDSStation{
		station(bar, kdsstation.StationTypeBar, 2, "Cocktails"),
		station(kitchen, kdsstation.StationTypeKitchen, 1, "Main Dishes"),
	}, nil)
	mustOwn(t, r, "Dog Food", "Dog Food", nil, kitchen)

	withExpo := New([]*ent.KDSStation{
		station(kitchen, kdsstation.StationTypeKitchen, 0, "Main Dishes"),
		station(expo, kdsstation.StationTypeExpo, 1, "Cocktails"), // expo never owns categories
	}, nil)
	mustOwn(t, withExpo, "Mojito", "Cocktails", nil, expo)
	if got := withExpo.Owner("Mojito", "Cocktails", nil); got != nil {
		t.Fatalf("expo must not own a category, got %v", got)
	}
}

func TestInactiveStationsAreIgnored(t *testing.T) {
	kitchen, bar := uuid.New(), uuid.New()
	off := station(bar, kdsstation.StationTypeBar, 0, "Cocktails")
	off.IsActive = false
	r := New([]*ent.KDSStation{off, station(kitchen, kdsstation.StationTypeKitchen, 1, "Main Dishes")}, nil)
	mustOwn(t, r, "Mojito", "Cocktails", nil, kitchen)
}

func TestCyclicTreeTerminates(t *testing.T) {
	bar := uuid.New()
	tree := NewTree([]Category{{ID: "a", Name: "A", ParentID: "b"}, {ID: "b", Name: "B", ParentID: "a"}})
	r := New([]*ent.KDSStation{station(bar, kdsstation.StationTypeBar, 0, "Z")}, tree)
	mustOwn(t, r, "x", "A", nil, bar) // fallback, no infinite loop
}

func TestCoverageAndStaleFilters(t *testing.T) {
	kitchen, bar := uuid.New(), uuid.New()
	tree := NewTree([]Category{
		{ID: "wines", Name: "Wines"}, {ID: "red", Name: "Red Wines", ParentID: "wines"},
		{ID: "mains", Name: "Main Dishes"}, {ID: "raw", Name: "Raw Ingredients"},
	})
	barSt := station(bar, kdsstation.StationTypeBar, 1, "Wines", "Whisky")
	r := New([]*ent.KDSStation{station(kitchen, kdsstation.StationTypeKitchen, 0, "Main Dishes"), barSt}, tree)
	owned, unclaimed := r.Coverage()
	if len(owned[bar]) != 2 || owned[bar][0] != "Wines" || owned[bar][1] != "Red Wines" {
		t.Fatalf("bar coverage = %v", owned[bar])
	}
	if len(unclaimed) != 1 || unclaimed[0] != "Raw Ingredients" {
		t.Fatalf("unclaimed = %v", unclaimed)
	}
	if stale := r.StaleFilters(barSt); len(stale) != 1 || stale[0] != "Whisky" {
		t.Fatalf("stale = %v", stale)
	}
	if New(nil, nil).StaleFilters(barSt) != nil {
		t.Fatal("no tree must flag nothing")
	}
}

func TestConflictsAndCleanFilterUseNormalisedKeys(t *testing.T) {
	others := []*ent.KDSStation{station(uuid.New(), kdsstation.StationTypeBar, 0, "Coffees")}
	if got := Conflicts(others, []string{"Coffee", "Burgers"}); len(got) != 1 || got[0] != "Coffee" {
		t.Fatalf("conflicts = %v", got)
	}
	if got := CleanFilter([]string{" Coffees ", "coffee", "", "Teas"}); len(got) != 2 || got[0] != "Coffees" || got[1] != "Teas" {
		t.Fatalf("clean = %v", got)
	}
}
