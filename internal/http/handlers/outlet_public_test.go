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

// A services outlet tells the storefront its trade and the steps a job goes through.
func TestPublicOutletDescribesServiceTrade(t *testing.T) {
	services := "services"
	o := &ent.Outlet{ID: uuid.New(), Name: "Garage", UseCase: &services, Status: "active"}
	o.Edges.Settings = &ent.OutletSetting{Metadata: map[string]any{"service_profile": "auto_garage"}}

	item := toOutletPublicItem(o)
	if item.ServiceProfile != "auto_garage" || item.ServiceProfileLabel != "Garage & Auto Service" {
		t.Fatalf("profile = %q / %q", item.ServiceProfile, item.ServiceProfileLabel)
	}
	if len(item.ServiceStages) == 0 || item.ServiceStages[0] != "Inspection" {
		t.Fatalf("stages = %v, want Inspection first", item.ServiceStages)
	}
}
