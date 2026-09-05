package orders

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
)

func newFiscalizationTestClient(t *testing.T) *ent.Client {
	t.Helper()
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:fiscalizationtest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestResolveOrderCustomer_StaffCreditSale_FallsBackToStaffARKey is the regression test for a
// real, confirmed bug: a staff-credit sale (an employee buying on their own staff account,
// order.Metadata["staff_member_id"] set, no real customer phone by design — see
// payments.staffCreditFromOrderParty's own convention, payments/service.go's RecordCreditSale
// call and credit_settlement.go's creditSettlementKey) resolved to phone="" here, so every
// reduction/reversal path that credits back an AR debt through this function
// (reversals.stepTreasuryGL, returns, saledelete) silently skipped the whole AR side — the staff
// member's real owed balance never moved even though their sale's own line was voided/reduced.
func TestResolveOrderCustomer_StaffCreditSale_FallsBackToStaffARKey(t *testing.T) {
	client := newFiscalizationTestClient(t)
	ctx := context.Background()
	tid := uuid.New()
	staffID := uuid.New()

	order, err := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").SetSubtotal(100).SetTaxTotal(0).SetTotalAmount(100).SetPaidTotal(100).
		SetMetadata(map[string]any{"staff_member_id": staffID.String()}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}

	crmContactID, _, phone := ResolveOrderCustomer(ctx, client, tid, order.ID)
	wantPhone := "staff:" + staffID.String()
	if phone != wantPhone {
		t.Errorf("phone (AR key) = %q, want %q", phone, wantPhone)
	}
	if crmContactID != "" {
		t.Errorf("crmContactID = %q, want empty for a staff-credit sale", crmContactID)
	}
}

// TestResolveOrderCustomer_OrdinaryCustomer_Unaffected guards that the staff-credit fallback
// added above never fires for an ordinary (non-staff) sale that already has a real phone.
func TestResolveOrderCustomer_OrdinaryCustomer_Unaffected(t *testing.T) {
	client := newFiscalizationTestClient(t)
	ctx := context.Background()
	tid := uuid.New()
	phone := "+254700111222"
	name := "Jane Customer"

	order, err := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").SetSubtotal(100).SetTaxTotal(0).SetTotalAmount(100).SetPaidTotal(100).
		SetCustomerName(name).SetCustomerPhone(phone).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}

	_, gotName, gotPhone := ResolveOrderCustomer(ctx, client, tid, order.ID)
	if gotPhone != phone {
		t.Errorf("phone = %q, want %q (unaffected by the staff fallback)", gotPhone, phone)
	}
	if gotName != name {
		t.Errorf("name = %q, want %q", gotName, name)
	}
}

// TestResolveOrderCustomer_NoPhoneNoStaffMetadata_ReturnsEmpty guards the plain walk-in case
// (no phone, no staff_member_id metadata) stays exactly as before — empty everything, no crash on
// a nil/absent metadata map.
func TestResolveOrderCustomer_NoPhoneNoStaffMetadata_ReturnsEmpty(t *testing.T) {
	client := newFiscalizationTestClient(t)
	ctx := context.Background()
	tid := uuid.New()

	order, err := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").SetSubtotal(100).SetTaxTotal(0).SetTotalAmount(100).SetPaidTotal(100).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}

	crmContactID, name, phone := ResolveOrderCustomer(ctx, client, tid, order.ID)
	if crmContactID != "" || name != "" || phone != "" {
		t.Errorf("got (%q, %q, %q), want all empty for a plain walk-in sale", crmContactID, name, phone)
	}
}
