package orders

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// TestUpdateSaleInfo_RejectsCustomerPhoneChangeOnAccountOrder is the regression test for the live
// incident found 2026-09-14 (boi-enterprises order 002426): a credit sale posts a treasury AR debt
// keyed on the customer's phone/CRM contact at the moment it's recorded (recordCreditSale). This
// admin tool used to let staff silently repoint the LOCAL order's phone afterward (e.g. correcting
// a cashier's wrong customer pick), orphaning that already-posted invoice on the OLD customer's
// treasury account forever — the new (correct) customer was never billed, and the mismatch was
// invisible from either customer's own account (each nets out fine independently), only surfacing
// as a fleet-wide aggregate discrepancy. Confirmed live: MR MUNA WEBUYE billed at creation, MR
// MESHARK WEBUYE (the real customer) 46 minutes later via this exact endpoint, 11,390 KES orphaned.
// Same failure shape a prior session hit once and never root-caused (MR SAMMON MALABA / MR SOLOMON
// NG'ETHE, 2026-09-11). Moving that AR debt needs a deliberate treasury-side transfer, not a silent
// field edit — so a phone change on an on_account order must now be rejected outright.
func TestUpdateSaleInfo_RejectsCustomerPhoneChangeOnAccountOrder(t *testing.T) {
	client := newFiscalizationTestClient(t)
	ctx := context.Background()
	tid := uuid.New()
	svc := NewService(client, Config{}, zap.NewNop())

	originalPhone := "+254700000001"
	order, err := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(11390).SetTaxTotal(0).SetTotalAmount(11390).SetPaidTotal(0).
		SetCustomerName("MR MUNA WEBUYE").SetCustomerPhone(originalPhone).
		SetMetadata(map[string]any{"on_account": true}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed on-account order: %v", err)
	}

	newPhone := "+254785346413"
	_, err = svc.UpdateSaleInfo(ctx, tid, order.ID, UpdateSaleInfoInput{
		CustomerPhone: &newPhone,
		Reason:        "k",
	})
	if err == nil {
		t.Fatal("expected UpdateSaleInfo to reject a phone change on an on-account order, got nil error")
	}

	reloaded, rerr := client.POSOrder.Get(ctx, order.ID)
	if rerr != nil {
		t.Fatalf("reload order: %v", rerr)
	}
	if reloaded.CustomerPhone == nil || *reloaded.CustomerPhone != originalPhone {
		t.Errorf("order.CustomerPhone changed despite the rejection: got %v, want %q", reloaded.CustomerPhone, originalPhone)
	}
}

// TestUpdateSaleInfo_AllowsCustomerNameChangeOnAccountOrder confirms the new guard is scoped to
// the phone specifically — correcting just the display name (same phone, no treasury AR key
// impact) on an on-account order must still work exactly as before.
func TestUpdateSaleInfo_AllowsCustomerNameChangeOnAccountOrder(t *testing.T) {
	client := newFiscalizationTestClient(t)
	ctx := context.Background()
	tid := uuid.New()
	svc := NewService(client, Config{}, zap.NewNop())

	phone := "+254700000002"
	order, err := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(5000).SetTaxTotal(0).SetTotalAmount(5000).SetPaidTotal(0).
		SetCustomerName("Jhon Typo").SetCustomerPhone(phone).
		SetMetadata(map[string]any{"on_account": true}).
		Save(ctx)
	if err != nil {
		t.Fatalf("seed on-account order: %v", err)
	}

	fixedName := "John Typo"
	result, err := svc.UpdateSaleInfo(ctx, tid, order.ID, UpdateSaleInfoInput{
		CustomerName: &fixedName,
		Reason:       "fix spelling",
	})
	if err != nil {
		t.Fatalf("expected a name-only correction to succeed, got: %v", err)
	}
	if result.Order.CustomerName == nil || *result.Order.CustomerName != fixedName {
		t.Errorf("customer_name = %v, want %q", result.Order.CustomerName, fixedName)
	}
}

// TestUpdateSaleInfo_AllowsCustomerPhoneChangeOnNonAccountOrder confirms the new guard only
// applies to on-account orders — a cash/immediate-pay sale has no treasury AR debt to orphan, so
// correcting its customer phone must still work exactly as before.
func TestUpdateSaleInfo_AllowsCustomerPhoneChangeOnNonAccountOrder(t *testing.T) {
	client := newFiscalizationTestClient(t)
	ctx := context.Background()
	tid := uuid.New()
	svc := NewService(client, Config{}, zap.NewNop())

	order, err := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(5000).SetTaxTotal(0).SetTotalAmount(5000).SetPaidTotal(5000).
		SetCustomerName("Walk-in Customer").SetCustomerPhone("+254700000003").
		Save(ctx)
	if err != nil {
		t.Fatalf("seed cash order: %v", err)
	}

	correctedPhone := "+254700000004"
	result, err := svc.UpdateSaleInfo(ctx, tid, order.ID, UpdateSaleInfoInput{
		CustomerPhone: &correctedPhone,
		Reason:        "wrong number entered",
	})
	if err != nil {
		t.Fatalf("expected a phone correction on a non-account order to succeed, got: %v", err)
	}
	if result.Order.CustomerPhone == nil || *result.Order.CustomerPhone != correctedPhone {
		t.Errorf("customer_phone = %v, want %q", result.Order.CustomerPhone, correctedPhone)
	}
}
