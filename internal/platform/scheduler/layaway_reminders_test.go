package scheduler

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	"github.com/bengobox/pos-service/internal/ent/layawayplan"
)

// Pure-Go sqlite shim: ent needs a driver registered as "sqlite3" and modernc.org/sqlite
// registers as "sqlite". Each test binary needs its own copy.
type sqlite3Driver struct{ *sqlite.Driver }

func (d sqlite3Driver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	if execer, ok := conn.(interface {
		Exec(string, []driver.Value) (driver.Result, error)
	}); ok {
		if _, err := execer.Exec("PRAGMA foreign_keys = ON;", nil); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func init() { sql.Register("sqlite3", sqlite3Driver{Driver: &sqlite.Driver{}}) }

func testPlan(id uuid.UUID) *ent.LayawayPlan {
	due := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	return &ent.LayawayPlan{
		ID: id, TenantID: uuid.New(), CustomerName: "Jane", CustomerPhone: "+254712345678", CustomerEmail: "jane@example.com",
		RemainingAmount: decimal.NewFromInt(90), DueDate: &due,
	}
}

// A reminder for a CRM-linked customer carries the plan and contact ids, never the name, phone
// or email: notifications-api reads those from the CRM.
func TestLayawayPaymentDuePayloadLinked(t *testing.T) {
	plan := testPlan(uuid.New())
	contactID := uuid.New()
	p := layawayPaymentDuePayload(plan, &contactID)
	if p["crm_contact_id"] != contactID.String() || p["layaway_plan_id"] != plan.ID.String() || p["due_date"] != "2026-11-13" {
		t.Fatalf("got %v", p)
	}
	if _, ok := p["balance_due"]; !ok {
		t.Error("balance_due missing")
	}
	for _, k := range []string{"customer_name", "customer_phone", "customer_email"} {
		if _, ok := p[k]; ok {
			t.Errorf("linked payload must not carry %q", k)
		}
	}
}

// A customer with no CRM link keeps name, phone and email so the reminder still reaches them.
func TestLayawayPaymentDuePayloadUnlinked(t *testing.T) {
	plan := testPlan(uuid.New())
	p := layawayPaymentDuePayload(plan, nil)
	if p["customer_name"] != "Jane" || p["customer_phone"] != "+254712345678" || p["customer_email"] != "jane@example.com" {
		t.Fatalf("got %v, want the customer fields", p)
	}
	if _, ok := p["crm_contact_id"]; ok {
		t.Error("unlinked payload has no crm_contact_id")
	}
}

// The plan's customer is the loyalty account it was opened on, else the account on its phone,
// always in the plan's own tenant. A staff layaway has no CRM contact.
func TestLayawayCustomerContactID(t *testing.T) {
	db := enttest.Open(t, "sqlite3", fmt.Sprintf("file:layawaycontact_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	tid, other := uuid.New(), uuid.New()

	byAccount, byPhone := uuid.New(), uuid.New()
	acc := db.LoyaltyAccount.Create().SetTenantID(tid).SetCustomerPhone("+254711000001").SetCustomerName("A").
		SetCrmContactID(byAccount).SaveX(ctx)
	db.LoyaltyAccount.Create().SetTenantID(tid).SetCustomerPhone("+254711000002").SetCustomerName("B").
		SetCrmContactID(byPhone).SaveX(ctx)
	foreign := db.LoyaltyAccount.Create().SetTenantID(other).SetCustomerPhone("+254711000003").SetCustomerName("C").
		SetCrmContactID(uuid.New()).SaveX(ctx)

	mk := func(phone string) *ent.LayawayPlanCreate {
		return db.LayawayPlan.Create().SetTenantID(tid).SetOutletID(uuid.New()).SetCustomerName("X").SetCustomerPhone(phone).
			SetTotalAmount(decimal.NewFromInt(100)).SetDepositAmount(decimal.NewFromInt(10)).
			SetPaidAmount(decimal.NewFromInt(10)).SetRemainingAmount(decimal.NewFromInt(90))
	}

	if got := layawayCustomerContactID(ctx, db, mk("").SetLoyaltyAccountID(acc.ID).SaveX(ctx)); got == nil || *got != byAccount {
		t.Fatalf("by account: got %v, want %s", got, byAccount)
	}
	if got := layawayCustomerContactID(ctx, db, mk("0711000002").SaveX(ctx)); got == nil || *got != byPhone {
		t.Fatalf("by phone: got %v, want %s", got, byPhone)
	}
	if got := layawayCustomerContactID(ctx, db, mk("").SetLoyaltyAccountID(foreign.ID).SaveX(ctx)); got != nil {
		t.Fatalf("another tenant's account used: %v", got)
	}
	if got := layawayCustomerContactID(ctx, db, mk("0711000002").SetPartyType(layawayplan.PartyTypeStaff).SaveX(ctx)); got != nil {
		t.Fatalf("staff layaway: got %v", got)
	}
	if got := layawayCustomerContactID(ctx, db, mk("0799999999").SaveX(ctx)); got != nil {
		t.Fatalf("unknown phone: got %v", got)
	}
}
