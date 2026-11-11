package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// marketflow asks, before erasing a CRM contact, whether POS still links it. A row in any POS table
// that carries crm_contact_id answers yes, whichever known tenant asks; no row answers no. {tenant} may be the UUID or the slug, and the route needs the service key.
func TestS2SCRMLinksAnswersPerTenant(t *testing.T) {
	db := openSQLiteTestClient(t, "crm_links")
	ctx := context.Background()

	mkTenant := func(slug string) uuid.UUID {
		tn, err := db.Tenant.Create().SetID(uuid.New()).SetName(slug).SetSlug(slug).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return tn.ID
	}
	slugA := "crm-links-a-" + uuid.NewString()[:6]
	tenantA := mkTenant(slugA)
	tenantB := mkTenant("crm-links-b-" + uuid.NewString()[:6])

	viaLoyalty, viaShell, viaClient, viaBalance, viaAppointment, unlinked := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()

	db.LoyaltyAccount.Create().SetTenantID(tenantA).SetCustomerPhone("0711000001").SetCustomerName("A").
		SetCrmContactID(viaLoyalty).SaveX(ctx)
	// A merged shell that still carries the id counts as referenced.
	db.LoyaltyAccount.Create().SetTenantID(tenantA).SetCustomerPhone("0711000002").SetCustomerName("B").
		SetCrmContactID(viaShell).SetMetadata(map[string]any{"merged_into": uuid.NewString()}).SaveX(ctx)
	db.ClientRecord.Create().SetTenantID(tenantA).SetPhone("0711000003").SetCrmContactID(viaClient).SaveX(ctx)
	db.CustomerBalanceCache.Create().SetTenantID(tenantA).SetCrmContactID(viaBalance).SaveX(ctx)
	db.Appointment.Create().SetTenantID(tenantA).SetOutletID(uuid.New()).SetServiceItemID(uuid.New()).
		SetServiceSku("SVC-1").SetStartTime(time.Now()).SetEndTime(time.Now().Add(time.Hour)).
		SetCrmContactID(viaAppointment).SaveX(ctx)
	viaBooking, viaGuest, viaEvent := uuid.New(), uuid.New(), uuid.New()
	now := time.Now()
	outlet := uuid.New()
	db.RoomBooking.Create().SetTenantID(tenantA).SetOutletID(outlet).SetConfirmationNo("BK-" + uuid.NewString()[:8]).
		SetLeadGuestName("Guest").SetArrivalDate(now).SetDepartureDate(now.AddDate(0, 0, 2)).SetCreatedBy(uuid.New()).
		SetCrmContactID(viaBooking).SaveX(ctx)
	room := db.Room.Create().SetTenantID(tenantA).SetOutletID(outlet).SetRoomNumber("C1").SetName("C1").
		SetRatePerNight(800).SaveX(ctx)
	db.RoomGuest.Create().SetTenantID(tenantA).SetRoomID(room.ID).SetGuestName("Guest").SetPhone("0700000000").
		SetIDNumber("ID-1").SetCheckInDate(now).SetNights(1).SetCheckOutDate(now.AddDate(0, 0, 1)).
		SetTotalRoomCharge(800).SetCheckedInBy(uuid.New()).SetCheckedInAt(now).SetCrmContactID(viaGuest).SaveX(ctx)
	db.EventBooking.Create().SetTenantID(tenantA).SetOutletID(outlet).SetFacilityID(uuid.New()).
		SetTitle("Launch").SetClientName("Client").SetStartAt(now).SetEndAt(now.Add(4 * time.Hour)).
		SetCreatedBy(uuid.New()).SetCrmContactID(viaEvent).SaveX(ctx)

	const key = "test-service-key"
	r := chi.NewRouter()
	r.With(httpware.RequireServiceKey(key)).
		Get("/api/v1/s2s/{tenant}/crm-links/{contactID}", NewLoyaltyHandler(zap.NewNop(), db, nil).S2SCRMLinks)

	ask := func(tenant, contact string, withKey bool) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/s2s/"+tenant+"/crm-links/"+contact, nil)
		if withKey {
			req.Header.Set("X-API-Key", key)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	for name, id := range map[string]uuid.UUID{
		"loyalty account": viaLoyalty, "merged loyalty shell": viaShell, "client record": viaClient,
		"balance cache": viaBalance, "appointment": viaAppointment,
		"room booking": viaBooking, "room guest": viaGuest, "event booking": viaEvent,
	} {
		if code, body := ask(tenantA.String(), id.String(), true); code != http.StatusOK || body["referenced"] != true || len(body) != 1 {
			t.Fatalf("%s by tenant id: %d %v", name, code, body)
		}
	}
	if code, body := ask(slugA, viaLoyalty.String(), true); code != http.StatusOK || body["referenced"] != true {
		t.Fatalf("by slug: %d %v", code, body)
	}
	// Asked under another known tenant, the row under tenant A still counts: contact ids are
	// unique across tenants, and a tenant mismatch must only ever keep the contact.
	if code, body := ask(tenantB.String(), viaLoyalty.String(), true); code != http.StatusOK || body["referenced"] != true {
		t.Fatalf("a link under another tenant must count: %d %v", code, body)
	}
	if code, body := ask(tenantA.String(), unlinked.String(), true); code != http.StatusOK || body["referenced"] != false {
		t.Fatalf("unlinked contact: %d %v", code, body)
	}
	if code, _ := ask(tenantA.String(), "not-a-uuid", true); code != http.StatusBadRequest {
		t.Fatalf("bad id: %d", code)
	}
	if code, _ := ask("no-such-tenant-"+uuid.NewString()[:8], viaLoyalty.String(), true); code != http.StatusNotFound {
		t.Fatalf("unknown slug: %d", code)
	}
	// A tenant UUID POS does not know (sync drift) cannot vouch for anything.
	if code, _ := ask(uuid.NewString(), viaLoyalty.String(), true); code != http.StatusNotFound {
		t.Fatalf("unknown tenant id: %d", code)
	}
	if code, _ := ask(tenantA.String(), viaLoyalty.String(), false); code != http.StatusUnauthorized {
		t.Fatalf("no key: %d", code)
	}
}
