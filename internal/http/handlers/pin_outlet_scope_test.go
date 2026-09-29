package handlers

import (
	"context"
	"sort"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	"github.com/bengobox/pos-service/internal/modules/outletpolicy"
)

// TestOutletStaffScope reproduces the print shop PIN grid that listed the salon's stylists and
// every other outlet's managers: only staff assigned to the outlet (plus tenant-wide admins), in
// the roles the outlet's use case and service profile use, may appear.
func TestOutletStaffScope(t *testing.T) {
	client := openSQLiteTestClient(t, "pinscope")
	ctx := context.Background()
	h := NewPINAuthHandler(zap.NewNop(), client, []byte("secret"), nil, "", "")

	tn, err := client.Tenant.Create().SetID(uuid.New()).SetName("Demo").SetSlug("demo-" + uuid.NewString()[:6]).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mkOutlet := func(code string) uuid.UUID {
		o, err := client.Outlet.Create().SetTenantID(tn.ID).SetTenantSlug(tn.Slug).SetCode(code).SetName(code).SetUseCase("services").Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return o.ID
	}
	printShop, salon := mkOutlet("PRT"), mkOutlet("SVC")
	if _, err := client.OutletSetting.Create().SetOutletID(printShop).
		SetMetadata(map[string]any{outletpolicy.MetaKeyServiceProfile: outletpolicy.ProfilePrintingBranding}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	mkStaff := func(name, role string, outlets ...uuid.UUID) {
		s, err := client.StaffMember.Create().SetTenantID(tn.ID).SetUserID(uuid.New()).SetName(name).SetRole(role).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range outlets {
			if _, err := client.StaffOutlet.Create().SetTenantID(tn.ID).SetStaffMemberID(s.ID).SetOutletID(o).Save(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	mkStaff("Owner", "admin")                      // tenant-wide: listed everywhere
	mkStaff("Print Manager", "manager", printShop) // assigned here
	mkStaff("Salon Manager", "manager", salon)     // another outlet's manager
	mkStaff("Technician", "technician", printShop) // the print shop's trade role
	mkStaff("Receptionist", "receptionist", printShop)
	mkStaff("Stylist", "stylist", printShop, salon) // assigned, but not a printing role
	mkStaff("Salon Cashier", "cashier", salon)      // another outlet's cashier

	scope, ok := h.outletStaffScope(ctx, tn.ID, printShop)
	if !ok {
		t.Fatal("outlet not resolved")
	}
	members, err := client.StaffMember.Query().Where(entstaff.TenantID(tn.ID)).Where(scope...).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range members {
		got = append(got, m.Name)
	}
	sort.Strings(got)
	want := []string{"Owner", "Print Manager", "Receptionist", "Technician"}
	if len(got) != len(want) {
		t.Fatalf("print shop PIN grid = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("print shop PIN grid = %v, want %v", got, want)
		}
	}

	if _, ok := h.outletStaffScope(ctx, uuid.New(), printShop); ok {
		t.Error("another tenant's outlet must not resolve")
	}
}
