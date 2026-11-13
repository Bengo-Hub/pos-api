package reversals

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/ent"
	entpospayment "github.com/bengobox/pos-service/internal/ent/pospayment"
	entposreversal "github.com/bengobox/pos-service/internal/ent/posreversal"
	entschema "github.com/bengobox/pos-service/internal/ent/schema"
	"github.com/bengobox/pos-service/internal/modules/payments"
)

// wantPortion is one expected treasury refund of a reversal, with the reference id suffix it must
// go under ("" for the reversal id itself).
type wantPortion struct {
	channel, refType, idSuffix string
	amount, tax                float64
}

// seedTenderReversal seeds a completed 3 x 100 sale paid by pays (marked on account when
// onAccount) and a reversal of qty units of it, on the channel resolveRefundChannel picks as the
// reversal tool does. The reversal carries 20% of its amount as tax.
func seedTenderReversal(t *testing.T, svc *Service, client *ent.Client, scope string, qty float64, onAccount bool, pays ...pay) *ent.POSReversal {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()
	order, line := seedSaleWithLine(t, client, tid, 3, 100, pays...)
	if onAccount {
		order = order.Update().SetMetadata(map[string]any{"on_account": true}).SaveX(ctx)
	}
	amount := qty * 100
	return client.POSReversal.Create().
		SetTenantID(tid).SetOrderID(order.ID).SetOrderNumber(order.OrderNumber).
		SetReversalNumber("REV-" + uuid.NewString()[:6]).SetScope(entposreversal.Scope(scope)).SetStatus("pending").SetReason("voided").
		SetRefundChannel(svc.resolveRefundChannel(ctx, tid, order.ID, "")).
		SetLines([]entschema.ReversalLineJSON{{LineID: line.ID, SKU: line.Sku, Name: line.Name, Quantity: qty, OfQuantity: 3, Amount: amount}}).
		SetAmount(amount).SetTaxAmount(amount / 5).SetCostAmount(0).
		SetSteps([]entschema.ReversalStepJSON{}).SetRequestedBy(uuid.New()).
		SaveX(ctx)
}

// runReversalMoneySteps runs stepPOSTotals then stepTreasuryGL in their real order and returns
// whether the treasury step was skipped.
func runReversalMoneySteps(t *testing.T, svc *Service, rev *ent.POSReversal) bool {
	t.Helper()
	ctx := context.Background()
	if _, _, _, err := svc.stepPOSTotals(ctx, rev); err != nil {
		t.Fatalf("stepPOSTotals: %v", err)
	}
	_, _, skip, err := svc.stepTreasuryGL(ctx, rev, "tenant-slug")
	if err != nil {
		t.Fatalf("stepTreasuryGL: %v", err)
	}
	return skip
}

func checkPortions(t *testing.T, rev *ent.POSReversal, got []treasuryCall, want []wantPortion) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("treasury calls = %+v, want %+v", got, want)
	}
	for i, w := range want {
		id := rev.ID.String()
		if w.idSuffix != "" {
			id = uuid.NewSHA1(rev.ID, []byte(w.idSuffix)).String()
		}
		c := got[i]
		if c.RefundChannel != w.channel || c.ReferenceType != w.refType || c.Amount != w.amount || c.TaxAmount != w.tax || c.ReferenceID != id {
			t.Errorf("call %d = %v (tax %v) %q %q id %s, want %v (tax %v) %q %q id %s",
				i, c.Amount, c.TaxAmount, c.RefundChannel, c.ReferenceType, c.ReferenceID, w.amount, w.tax, w.channel, w.refType, id)
		}
	}
}

// treasuryCall is the part of a recorded refund request these tests check.
type treasuryCall struct {
	RefundChannel, ReferenceType, ReferenceID string
	Amount, TaxAmount                         float64
}

func recordedCalls(rec *recordingTreasuryServer) []treasuryCall {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]treasuryCall, 0, len(rec.calls))
	for _, c := range rec.calls {
		out = append(out, treasuryCall{RefundChannel: c.RefundChannel, ReferenceType: c.ReferenceType, ReferenceID: c.ReferenceID, Amount: c.Amount, TaxAmount: c.TaxAmount})
	}
	return out
}

// A full-scope reversal keeps its payment rows (stepPOSTotals nets nothing), so the money-backed
// part comes from what the real tenders paid. Before the fix every full void went to treasury as
// one offset_invoice AR write-off, so a voided cash sale was booked Cr 1400 instead of Cr Cash.
func TestFullVoidRefundsTheRealTendersThroughTheRefundChannel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		onAccount bool
		pays      []pay
		want      []wantPortion
	}{
		{
			name: "cash sale", pays: []pay{{"cash", 300, 0}},
			want: []wantPortion{{"cash", "pos_return", "", 300, 60}},
		},
		{
			name: "credit sale", onAccount: true, pays: []pay{{payments.TenderOnAccount, 300, 0}},
			want: []wantPortion{{"offset_invoice", "pos_return", "", 300, 60}},
		},
		{
			name: "cash 100 + on account 200", onAccount: true,
			pays: []pay{{"cash", 100, 0}, {payments.TenderOnAccount, 200, 10}},
			want: []wantPortion{{"cash", "pos_return", "cash", 100, 20}, {"offset_invoice", ReversalRestReferenceType, "ar", 200, 40}},
		},
		{
			name: "M-Pesa code sale", pays: []pay{{"mpesa_manual", 300, 0}},
			want: []wantPortion{{"mpesa_manual", "pos_return", "", 300, 60}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, trc := newRecordingTreasuryServer(t)
			svc, client := newTestReversalsService(t, trc)
			rev := seedTenderReversal(t, svc, client, "full", 3, tc.onAccount, tc.pays...)

			if skip := runReversalMoneySteps(t, svc, rev); skip {
				t.Fatal("treasury step skipped")
			}

			checkPortions(t, rev, recordedCalls(rec), tc.want)
			// The payment rows are kept for history: a full void nets none of them.
			paid := 0.0
			for _, p := range client.POSPayment.Query().Where(entpospayment.OrderID(rev.OrderID)).AllX(context.Background()) {
				paid += p.Amount
			}
			if paid != 300 {
				t.Errorf("payments add up to %v after the void, want 300 kept", paid)
			}
		})
	}
}

// Non-money tenders are never refunded as money, on a partial or a full reversal: a complimentary
// part goes back against the complimentary expense (treasury's complimentary channel), and an
// exchange or customer credit part goes back to the customer as store credit through treasury's
// store_credit channel. A loyalty part goes with the money (treasury booked it as cash).
func TestReversalNeverRefundsNonMoneyTendersAsCash(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scope     string
		qty       float64
		onAccount bool
		pays      []pay
		want      []wantPortion
		wantSkip  bool
	}{
		{
			// stepPOSTotals nets the newest row first: complimentary 100, then 100 of the cash. The
			// sale was part complimentary (booked with VAT on the whole sale), so both carry tax.
			name: "partial: 200 off a sale of cash 200 + complimentary 100", scope: "partial", qty: 2,
			pays: []pay{{"cash", 200, 10}, {payments.TenderComplimentary, 100, 0}},
			want: []wantPortion{{"cash", "pos_return", "cash", 100, 20}, {"complimentary", ReversalRestReferenceType, "complimentary", 100, 20}},
		},
		{
			name: "full: cash 100 + complimentary 200", scope: "full", qty: 3,
			pays: []pay{{"cash", 100, 0}, {payments.TenderComplimentary, 200, 10}},
			want: []wantPortion{{"cash", "pos_return", "cash", 100, 20}, {"complimentary", ReversalRestReferenceType, "complimentary", 200, 40}},
		},
		{
			// Treasury booked a wholly complimentary sale without VAT, so its reversal carries none.
			name: "full: wholly complimentary", scope: "full", qty: 3,
			pays: []pay{{payments.TenderComplimentary, 300, 0}},
			want: []wantPortion{{"complimentary", "pos_return", "", 300, 0}},
		},
		{
			// Treasury has no loyalty account and booked the loyalty part as cash at the till, so it
			// is reversed with the money: one portion under the reversal id.
			name: "full: cash 100 + loyalty 200", scope: "full", qty: 3,
			pays: []pay{{"cash", 100, 0}, {"loyalty", 200, 10}},
			want: []wantPortion{{"cash", "pos_return", "", 300, 60}},
		},
		{
			name: "full: wholly loyalty", scope: "full", qty: 3,
			pays: []pay{{"loyalty", 300, 0}},
			want: []wantPortion{{"cash", "pos_return", "", 300, 60}},
		},
		{
			name: "partial: 200 off a sale of cash 200 + exchange credit 100", scope: "partial", qty: 2,
			pays: []pay{{"cash", 200, 10}, {payments.TenderExchangeCredit, 100, 0}},
			want: []wantPortion{{"cash", "pos_return", "cash", 100, 20}, {"store_credit", ReversalRestReferenceType, "store-credit", 100, 20}},
		},
		{
			name: "full: cash 100 + exchange credit 200", scope: "full", qty: 3,
			pays: []pay{{"cash", 100, 0}, {payments.TenderExchangeCredit, 200, 10}},
			want: []wantPortion{{"cash", "pos_return", "cash", 100, 20}, {"store_credit", ReversalRestReferenceType, "store-credit", 200, 40}},
		},
		{
			name: "full: customer credit 300", scope: "full", qty: 3,
			pays: []pay{{payments.TenderCustomerCredit, 300, 0}},
			want: []wantPortion{{"store_credit", "pos_return", "", 300, 60}},
		},
		{
			name: "full: cash 100 + exchange credit 100 + on account 100", scope: "full", qty: 3, onAccount: true,
			pays: []pay{{"cash", 100, 0}, {payments.TenderExchangeCredit, 100, 10}, {payments.TenderOnAccount, 100, 20}},
			want: []wantPortion{
				{"cash", "pos_return", "cash", 100, 20},
				{"store_credit", ReversalRestReferenceType, "store-credit", 100, 20},
				{"offset_invoice", ReversalRestReferenceType, "ar", 100, 20},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, trc := newRecordingTreasuryServer(t)
			svc, client := newTestReversalsService(t, trc)
			rev := seedTenderReversal(t, svc, client, tc.scope, tc.qty, tc.onAccount, tc.pays...)

			if skip := runReversalMoneySteps(t, svc, rev); skip != tc.wantSkip {
				t.Fatalf("treasury step skipped = %v, want %v", skip, tc.wantSkip)
			}
			checkPortions(t, rev, recordedCalls(rec), tc.want)
		})
	}
}

// Retrying a reversal (the Retry endpoint re-runs the steps) sends the same portions under the
// same deterministic reference ids, so treasury books each once.
func TestReversalRetryIsIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scope     string
		qty       float64
		onAccount bool
		pays      []pay
		want      []wantPortion
	}{
		{
			name: "full void of cash 100 + on account 200", scope: "full", qty: 3, onAccount: true,
			pays: []pay{{"cash", 100, 0}, {payments.TenderOnAccount, 200, 10}},
			want: []wantPortion{{"cash", "pos_return", "cash", 100, 20}, {"offset_invoice", ReversalRestReferenceType, "ar", 200, 40}},
		},
		{
			name: "full void of cash 100 + exchange credit 200", scope: "full", qty: 3,
			pays: []pay{{"cash", 100, 0}, {payments.TenderExchangeCredit, 200, 10}},
			want: []wantPortion{{"cash", "pos_return", "cash", 100, 20}, {"store_credit", ReversalRestReferenceType, "store-credit", 200, 40}},
		},
		{
			name: "partial of cash 200 + complimentary 100", scope: "partial", qty: 2,
			pays: []pay{{"cash", 200, 10}, {payments.TenderComplimentary, 100, 0}},
			want: []wantPortion{{"cash", "pos_return", "cash", 100, 20}, {"complimentary", ReversalRestReferenceType, "complimentary", 100, 20}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, trc := newRecordingTreasuryServer(t)
			svc, client := newTestReversalsService(t, trc)
			rev := seedTenderReversal(t, svc, client, tc.scope, tc.qty, tc.onAccount, tc.pays...)

			runReversalMoneySteps(t, svc, rev)
			runReversalMoneySteps(t, svc, client.POSReversal.GetX(context.Background(), rev.ID))

			calls := recordedCalls(rec)
			if len(calls) != 2*len(tc.want) {
				t.Fatalf("treasury calls = %+v, want the same portions twice", calls)
			}
			checkPortions(t, rev, calls[:len(tc.want)], tc.want)
			checkPortions(t, rev, calls[len(tc.want):], tc.want)
		})
	}
}
