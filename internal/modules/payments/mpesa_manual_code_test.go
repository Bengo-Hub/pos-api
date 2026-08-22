package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
)

func seedOrderForMpesaCodeTest(t *testing.T, client *ent.Client, tenantID uuid.UUID) *ent.POSOrder {
	t.Helper()
	order, err := client.POSOrder.Create().
		SetTenantID(tenantID).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").
		SetSubtotal(100).SetTaxTotal(0).SetTotalAmount(100).SetPaidTotal(100).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return order
}

func seedCompletedMpesaPayment(t *testing.T, client *ent.Client, order *ent.POSOrder, code string) *ent.POSPayment {
	t.Helper()
	payment, err := client.POSPayment.Create().
		SetOrderID(order.ID).
		SetTenderID(uuid.New()).
		SetAmount(100).
		SetCurrency("KES").
		SetStatus(StatusCompleted).
		SetExternalReference(code).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed payment: %v", err)
	}
	return payment
}

// TestIsMpesaManualCodeReused is the regression test for the fraud vulnerability flagged live
// 2026-08-22: the manual M-Pesa "Code" tender is trust-based (the cashier sights the SMS; nothing
// calls Safaricom to verify it), and nothing previously tied a code to the one real payment it came
// from — so the SAME code (e.g. "UHMEQ4BFAA") could be typed against a second, unrelated sale and
// settle it too. isMpesaManualCodeReused is now consulted before creating a manual-code payment row.
func TestIsMpesaManualCodeReused(t *testing.T) {
	svc, client := newTestPaymentsService(t)
	tenantID := uuid.New()
	order1 := seedOrderForMpesaCodeTest(t, client, tenantID)
	order2 := seedOrderForMpesaCodeTest(t, client, tenantID)
	seedCompletedMpesaPayment(t, client, order1, "UHMEQ4BFAA")

	t.Run("fresh code is not reused", func(t *testing.T) {
		reused, err := svc.isMpesaManualCodeReused(context.Background(), tenantID, "BRANDNEW1")
		if err != nil {
			t.Fatalf("isMpesaManualCodeReused() error = %v", err)
		}
		if reused {
			t.Errorf("isMpesaManualCodeReused() = true, want false for a never-used code")
		}
	})

	t.Run("same code against a second order in the same tenant is rejected", func(t *testing.T) {
		reused, err := svc.isMpesaManualCodeReused(context.Background(), tenantID, "UHMEQ4BFAA")
		if err != nil {
			t.Fatalf("isMpesaManualCodeReused() error = %v", err)
		}
		if !reused {
			t.Errorf("isMpesaManualCodeReused() = false, want true — code already settled order %s", order1.ID)
		}
		_ = order2
	})

	t.Run("case/whitespace-insensitive match", func(t *testing.T) {
		reused, err := svc.isMpesaManualCodeReused(context.Background(), tenantID, "uhmeq4bfaa")
		if err != nil {
			t.Fatalf("isMpesaManualCodeReused() error = %v", err)
		}
		if !reused {
			t.Errorf("isMpesaManualCodeReused() = false, want true for a lowercase retype of an already-used code")
		}
	})

	t.Run("a different tenant using the same code is unaffected", func(t *testing.T) {
		otherTenant := uuid.New()
		reused, err := svc.isMpesaManualCodeReused(context.Background(), otherTenant, "UHMEQ4BFAA")
		if err != nil {
			t.Fatalf("isMpesaManualCodeReused() error = %v", err)
		}
		if reused {
			t.Errorf("isMpesaManualCodeReused() = true, want false — a different tenant must not be blocked by another tenant's code")
		}
	})
}
