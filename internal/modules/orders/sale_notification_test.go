package orders

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent/enttest"
)

// Only email and WhatsApp are active channels: SMS is never chosen, and a channel without its
// contact falls back to the other.
func TestResolveReceiptChannelNeverPicksSMS(t *testing.T) {
	cases := []struct{ channel, phone, email, want string }{
		{"", "0712", "a@b.co", "email"},
		{"", "0712", "", "whatsapp"},
		{"sms", "0712", "", "whatsapp"},
		{"sms", "0712", "a@b.co", "email"},
		{"whatsapp", "0712", "a@b.co", "whatsapp"},
		{"whatsapp", "", "a@b.co", "email"},
		{"email", "0712", "", "whatsapp"},
		{"email", "", "", ""},
	}
	for _, c := range cases {
		if got := ResolveReceiptChannel(c.channel, c.phone, c.email); got != c.want {
			t.Errorf("ResolveReceiptChannel(%q, %q, %q) = %q, want %q", c.channel, c.phone, c.email, got, c.want)
		}
	}
}

// A WhatsApp receipt for a tenant with no WhatsApp plan or credit is refused before anything is
// published; an unknown answer lets it through for notifications-api to decide.
func TestRequestSaleNotificationRefusesUnpaidWhatsApp(t *testing.T) {
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:salenotify_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	tid := uuid.New()
	order := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").SetSubtotal(100).SetTaxTotal(0).
		SetTotalAmount(100).SetPaidTotal(100).SetCustomerPhone("+254700111222").SaveX(ctx)

	svc := &Service{client: client}
	svc.WithMessagingGate(func(context.Context, uuid.UUID, string) (bool, bool, string) {
		return false, true, "service_not_activated"
	})
	_, err := svc.RequestSaleNotification(ctx, tid, order.ID, "", "", "whatsapp")
	var inactive *ErrMessagingNotActive
	if !errors.As(err, &inactive) {
		t.Fatalf("unpaid WhatsApp must be refused, got %v", err)
	}
	// The same sale with an email typed in goes by email, which is never gated.
	if _, err := svc.RequestSaleNotification(ctx, tid, order.ID, "", "a@b.co", ""); err != nil {
		t.Fatalf("email receipt: %v", err)
	}
	// Billing unreachable: the send is not blocked here.
	svc.WithMessagingGate(func(context.Context, uuid.UUID, string) (bool, bool, string) { return false, false, "" })
	if _, err := svc.RequestSaleNotification(ctx, tid, order.ID, "", "", "whatsapp"); err != nil {
		t.Fatalf("unknown answer must not block: %v", err)
	}
	// No contact at all.
	bare := client.POSOrder.Create().
		SetTenantID(tid).SetOutletID(uuid.New()).SetDeviceID(uuid.New()).SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).SetStatus("completed").SetSubtotal(1).SetTaxTotal(0).
		SetTotalAmount(1).SetPaidTotal(1).SaveX(ctx)
	if _, err := svc.RequestSaleNotification(ctx, tid, bare.ID, "", "", ""); !errors.Is(err, ErrNoReceiptContact) {
		t.Fatalf("no contact: %v", err)
	}
}
