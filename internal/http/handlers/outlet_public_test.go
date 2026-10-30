package handlers

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

// The storefront's product page shows the outlet's returns window, so the public outlet carries it
// even when no other public setting is set.
func TestPublicOutletCarriesReturnWindow(t *testing.T) {
	retail := "retail"
	o := &ent.Outlet{ID: uuid.New(), Name: "Gachie", UseCase: &retail, Status: "active"}
	o.Edges.Settings = &ent.OutletSetting{ReturnWindowDays: 7}

	item := toOutletPublicItem(o)
	if item.Settings == nil || item.Settings.ReturnWindowDays != 7 {
		t.Fatalf("settings = %+v, want return_window_days 7", item.Settings)
	}

	o.Edges.Settings = &ent.OutletSetting{}
	if item := toOutletPublicItem(o); item.Settings != nil {
		t.Fatalf("settings = %+v, want none when nothing public is set", item.Settings)
	}
}
