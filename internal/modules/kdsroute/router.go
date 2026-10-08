// Package kdsroute decides which KDS station (kitchen, bar, ...) owns an order line. It is the
// single routing rule for every caller: stamping pos_order_lines.kds_station_id at order create
// and add-to-bill, the KDS ticket and print-chit fan-out, the sales-by-station reports and the
// station settings screen. pos-ui's offline print router mirrors it (lib/pos/kitchen-bar-print.ts)
// using the per-station category list ListStations returns, so the two never drift.
//
// Priority order for a line:
//  1. an explicit per-item pin (pos_catalog_overrides.kds_station_id) naming a live station;
//  2. the line's category claimed by a station's category_filter, walking UP the inventory
//     category tree so a sub-category ("Red Wines") inherits its parent's station ("Wines").
//     The closest claim wins, so a station can still claim one sub-category of another
//     station's section. Names compare in a normalised form (case, spacing, punctuation, "&"
//     and simple plurals), so "Coffee" matches a filter saved as "Coffees" and "Freaky Shakes"
//     matches "Freakyshakes";
//  3. legacy lines with no category: a filter name contained in the item name;
//  4. nothing claimed it: a hot-beverage name guess goes to the kitchen station;
//  5. OwnerOrFallback only: the first expo/all station, else the first station by sort order.
//
// The 2026-10 urban-loft incident behind this package: the menu rebuild renamed and nested the
// drinks categories ("Coffees" became "Coffee", beers moved under "Beers & Ciders", wines under
// "Red/White/House Wine"), the Bar station's filter still held the old exact names, and every
// unmatched drink fell through to the kitchen printer.
package kdsroute

import (
	"sort"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// Station types that receive a secondary copy of unresolved items instead of owning categories.
const (
	TypeExpo = "expo"
	TypeAll  = "all"
)

// maxTreeDepth bounds the ancestor walk so a corrupt (cyclic) category tree can never loop.
const maxTreeDepth = 32

// Key normalises a category or filter name for comparison: lower case, "&" read as "and",
// only letters and digits kept, and one simple English plural folded ("Coffees" and "Coffee",
// "Sandwiches" and "Sandwich", "Freaky Shakes" and "Freakyshakes" all share a key).
// pos-ui's normaliseCategoryKey must stay byte-for-byte equivalent.
func Key(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "&", " and ")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	k := b.String()
	switch {
	case len(k) > 4 && (strings.HasSuffix(k, "ches") || strings.HasSuffix(k, "shes") ||
		strings.HasSuffix(k, "sses") || strings.HasSuffix(k, "xes") || strings.HasSuffix(k, "zes")):
		k = k[:len(k)-2]
	case len(k) > 3 && strings.HasSuffix(k, "s") && !strings.HasSuffix(k, "ss"):
		k = k[:len(k)-1]
	}
	return k
}

// Category is one inventory category node (inventory-api GET /inventory/categories).
type Category struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	ParentID string `json:"parent_id,omitempty"`
	IsActive bool   `json:"is_active"`
}

// Tree is a tenant's category hierarchy keyed by normalised name. A nil *Tree is valid and
// behaves as a flat list (every category is its own root).
type Tree struct {
	parent map[string]string   // child key -> parent key
	names  map[string]string   // key -> display name (first seen)
	order  []string            // keys in inventory order, for stable listings
	kids   map[string][]string // parent key -> child keys
}

// NewTree builds the hierarchy from inventory categories. Inactive categories are kept: a
// historical line may still carry their name, and routing must not change when one is hidden.
func NewTree(cats []Category) *Tree {
	t := &Tree{
		parent: make(map[string]string, len(cats)),
		names:  make(map[string]string, len(cats)),
		kids:   make(map[string][]string, len(cats)),
	}
	keyByID := make(map[string]string, len(cats))
	for _, c := range cats {
		k := Key(c.Name)
		if k == "" {
			continue
		}
		keyByID[c.ID] = k
		if _, seen := t.names[k]; !seen {
			t.names[k] = strings.TrimSpace(c.Name)
			t.order = append(t.order, k)
		}
	}
	for _, c := range cats {
		k, pk := keyByID[c.ID], keyByID[c.ParentID]
		if k == "" || pk == "" || pk == k {
			continue
		}
		if _, set := t.parent[k]; set {
			continue
		}
		t.parent[k] = pk
		t.kids[pk] = append(t.kids[pk], k)
	}
	return t
}

// Has reports whether the tree knows a category with this (normalised) name.
func (t *Tree) Has(name string) bool {
	if t == nil {
		return false
	}
	_, ok := t.names[Key(name)]
	return ok
}

// Size is the number of distinct categories in the tree.
func (t *Tree) Size() int {
	if t == nil {
		return 0
	}
	return len(t.order)
}

// lineage returns the key itself followed by its ancestors, closest first.
func (t *Tree) lineage(k string) []string {
	out := []string{k}
	if t == nil {
		return out
	}
	for i := 0; i < maxTreeDepth; i++ {
		p, ok := t.parent[k]
		if !ok || p == "" {
			break
		}
		out = append(out, p)
		k = p
	}
	return out
}

// Router resolves the owning station for order lines against one outlet's active stations.
// Build it once per request (order create, report run) and reuse it for every line.
type Router struct {
	stations []*ent.KDSStation
	byID     map[uuid.UUID]*ent.KDSStation
	claims   map[string]uuid.UUID // normalised filter key -> owning station
	tree     *Tree
}

// New builds a router. stations may arrive in any order; they are sorted by sort_order then
// name so the fallback station and the winner of a (rare) normalised-name clash are stable.
// Inactive stations are ignored. tree may be nil (flat matching on the line's own category).
func New(stations []*ent.KDSStation, tree *Tree) *Router {
	active := make([]*ent.KDSStation, 0, len(stations))
	for _, st := range stations {
		if st != nil && st.IsActive {
			active = append(active, st)
		}
	}
	sort.SliceStable(active, func(i, j int) bool {
		if active[i].SortOrder != active[j].SortOrder {
			return active[i].SortOrder < active[j].SortOrder
		}
		return active[i].Name < active[j].Name
	})
	r := &Router{
		stations: active,
		byID:     make(map[uuid.UUID]*ent.KDSStation, len(active)),
		claims:   make(map[string]uuid.UUID),
		tree:     tree,
	}
	for _, st := range active {
		r.byID[st.ID] = st
		if isCopyStation(st) {
			continue
		}
		for _, c := range st.CategoryFilter {
			if k := Key(c); k != "" {
				if _, taken := r.claims[k]; !taken {
					r.claims[k] = st.ID
				}
			}
		}
	}
	return r
}

func isCopyStation(st *ent.KDSStation) bool {
	t := string(st.StationType)
	return t == TypeExpo || t == TypeAll
}

// Stations returns the active stations in routing order.
func (r *Router) Stations() []*ent.KDSStation { return r.stations }

// CopyStations returns the expo/all stations that receive unresolved items as a secondary copy.
func (r *Router) CopyStations() []uuid.UUID {
	var out []uuid.UUID
	for _, st := range r.stations {
		if isCopyStation(st) {
			out = append(out, st.ID)
		}
	}
	return out
}

// Owner returns the station that owns a line (priorities 1 to 4), or nil when nothing claims it.
func (r *Router) Owner(name, category string, pinned *uuid.UUID) *uuid.UUID {
	// 1. Explicit per-item pin, honoured only while it names a live station of this outlet.
	if pinned != nil && *pinned != uuid.Nil {
		if _, ok := r.byID[*pinned]; ok {
			id := *pinned
			return &id
		}
	}

	// 2. Category claim, closest ancestor first.
	if k := Key(category); k != "" {
		for _, ck := range r.tree.lineage(k) {
			if id, ok := r.claims[ck]; ok {
				return &id
			}
		}
	} else {
		// 3. Legacy line without a category: a filter name inside the item name.
		lname := strings.ToLower(name)
		for _, st := range r.stations {
			if isCopyStation(st) {
				continue
			}
			for _, c := range st.CategoryFilter {
				if needle := strings.ToLower(strings.TrimSpace(c)); needle != "" && strings.Contains(lname, needle) {
					id := st.ID
					return &id
				}
			}
		}
	}

	// 4. Unclaimed: hot drinks default to the kitchen for tenants that never configured them.
	if IsHotBeverage(name, category) {
		for _, st := range r.stations {
			if string(st.StationType) == "kitchen" {
				id := st.ID
				return &id
			}
		}
	}
	return nil
}

// OwnerOrFallback is Owner collapsed to exactly one station: the first expo/all station, else
// the first station in sort order. Used wherever a line needs one owner (persisting
// kds_station_id, revenue by station).
func (r *Router) OwnerOrFallback(name, category string, pinned *uuid.UUID) *uuid.UUID {
	if id := r.Owner(name, category, pinned); id != nil {
		return id
	}
	if copies := r.CopyStations(); len(copies) > 0 {
		id := copies[0]
		return &id
	}
	if len(r.stations) > 0 {
		id := r.stations[0].ID
		return &id
	}
	return nil
}

// Coverage lists, per station, every known category it owns through its filter, including
// inherited sub-categories, in inventory order. Categories no station claims are returned
// separately so the settings screen can show where they fall.
func (r *Router) Coverage() (owned map[uuid.UUID][]string, unclaimed []string) {
	owned = make(map[uuid.UUID][]string, len(r.stations))
	if r.tree == nil {
		return owned, nil
	}
	for _, k := range r.tree.order {
		name := r.tree.names[k]
		if id := r.claimFor(k); id != uuid.Nil {
			owned[id] = append(owned[id], name)
			continue
		}
		unclaimed = append(unclaimed, name)
	}
	return owned, unclaimed
}

// StaleFilters returns the station's filter entries that match no current inventory category
// (renamed or deleted). Empty when the tree is unknown, so nothing is flagged on a fetch failure.
func (r *Router) StaleFilters(st *ent.KDSStation) []string {
	if r.tree == nil || r.tree.Size() == 0 || st == nil {
		return nil
	}
	var out []string
	for _, c := range st.CategoryFilter {
		if strings.TrimSpace(c) != "" && !r.tree.Has(c) {
			out = append(out, c)
		}
	}
	return out
}

func (r *Router) claimFor(k string) uuid.UUID {
	for _, ck := range r.tree.lineage(k) {
		if id, ok := r.claims[ck]; ok {
			return id
		}
	}
	return uuid.Nil
}

// hotBeverageKeywords drive the last-resort guess in Owner (priority 4 only).
var hotBeverageKeywords = []string{
	"coffee", "tea", "espresso", "cappuccino", "latte", "americano", "macchiato",
	"mocha", "hot chocolate", "chai", "flat white", "cortado", "affogato",
	"hot beverage", "hot drink",
}

// IsHotBeverage reports whether an item looks like a hot drink by name or category. Iced
// coffee and iced tea are cold drinks and never match.
func IsHotBeverage(name, category string) bool {
	hay := strings.ToLower(name + " " + category)
	for _, kw := range hotBeverageKeywords {
		if strings.Contains(hay, kw) {
			if (kw == "coffee" || kw == "tea") && (strings.Contains(hay, "iced "+kw) || strings.Contains(hay, "ice "+kw)) {
				continue
			}
			return true
		}
	}
	return false
}

// Conflicts returns the entries of wanted that another station already claims, comparing
// normalised keys so "Coffee" and "Coffees" can never be split across two stations.
func Conflicts(others []*ent.KDSStation, wanted []string) []string {
	taken := make(map[string]struct{})
	for _, st := range others {
		for _, c := range st.CategoryFilter {
			if k := Key(c); k != "" {
				taken[k] = struct{}{}
			}
		}
	}
	var out []string
	seen := make(map[string]struct{})
	for _, c := range wanted {
		k := Key(c)
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		if _, ok := taken[k]; ok {
			out = append(out, strings.TrimSpace(c))
		}
	}
	return out
}

// CleanFilter trims entries, drops blanks and collapses entries that normalise to the same key,
// keeping the first spelling.
func CleanFilter(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, c := range in {
		c = strings.TrimSpace(c)
		k := Key(c)
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, c)
	}
	return out
}
