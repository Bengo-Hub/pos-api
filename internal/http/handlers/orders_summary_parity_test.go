package handlers

import (
	"context"
	"database/sql"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/posorderline"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/predicate"
	_ "github.com/bengobox/pos-service/internal/ent/runtime"
	"github.com/bengobox/pos-service/internal/ent/tender"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// summaryParityDSN is the local dev Postgres used by the other Postgres-only tests (see
// catalog/inventory_events_test.go). The SQL aggregate uses jsonb operators, FILTER and
// GROUPING SETS, so SQLite cannot run it. The test skips when the database is unreachable.
const summaryParityDSN = "postgres://postgres:postgres@localhost:5432/pos?sslmode=disable"

// summaryParityNow is the fixed "now" handed to the SQL path. The Go path reads the real clock,
// so the seeded due dates sit far on either side (2020 and 2099) and both agree.
var summaryParityNow = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)

func openSummaryParityDB(t *testing.T) (*sql.DB, *ent.Client) {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	admin, err := sql.Open("pgx", summaryParityDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := admin.Ping(); err != nil {
		_ = admin.Close()
		t.Skipf("local postgres unreachable (expected at %s): %v", summaryParityDSN, err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	// A throwaway schema built from the CURRENT Ent schema, so the test never depends on how far
	// the shared local database has been migrated (it failed once on a missing column). pg_trgm
	// stays in public, which is on the search path, for the trigram indexes the schema declares.
	ctx := context.Background()
	schema := "summary_parity_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	for _, stmt := range []string{
		"CREATE EXTENSION IF NOT EXISTS pg_trgm",
		"CREATE SCHEMA " + schema,
	} {
		if _, err := admin.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("prepare test schema: %s: %v", stmt, err)
		}
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })

	db, err := sql.Open("pgx", summaryParityDSN+"&search_path="+schema+",public")
	if err != nil {
		t.Fatalf("open test schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	if err := client.Schema.Create(ctx); err != nil {
		t.Fatalf("create current schema in %s: %v", schema, err)
	}
	return db, client
}

// TestOrdersSummarySQL_ParityWithGo seeds every branch of ComputeSettlement and checks that the
// SQL aggregate equals the Go summing path (enrichOrderList plus buildOrdersSummary, exactly what
// OrdersSummary serves) figure for figure, with and without each payment-status filter.
func TestOrdersSummarySQL_ParityWithGo(t *testing.T) {
	db, client := openSummaryParityDB(t)
	ctx := context.Background()
	tid := uuid.New()
	seedSummaryParityOrders(t, ctx, client, tid)

	h := &POSOrderHandler{log: zap.NewNop(), client: client}
	tenantOnly := []predicate.POSOrder{posorder.TenantID(tid)}

	for _, ps := range []string{"", "paid", "partial", "due", "overdue", "refunded", "voided", "cancelled", "draft"} {
		t.Run("payment_status="+ps, func(t *testing.T) {
			goSum, sqlSum := summaryBothWays(t, ctx, h, db, tid, tenantOnly, ps)
			if diffs := diffOrdersSummary(goSum, sqlSum); len(diffs) > 0 {
				t.Fatalf("SQL summary differs from Go summary:\n%s", strings.Join(diffs, "\n"))
			}
		})
	}

	// Ent-built predicates with their own bound arguments (a jsonb path match inside a payments
	// subquery and a line subquery), to prove the statement's placeholders number correctly
	// after the builder's.
	t.Run("ent predicates with args", func(t *testing.T) {
		preds := []predicate.POSOrder{
			posorder.TenantID(tid),
			posorder.StatusNEQ(orders.StatusDraft),
			posorder.HasPaymentsWith(predicate.POSPayment(func(s *entsql.Selector) {
				s.Where(sqljson.ValueEQ(pospayment.FieldPaymentData, "mpesa", sqljson.Path("method")))
			})),
			posorder.HasLinesWith(posorderline.SkuHasPrefix("SKU-")),
		}
		goSum, sqlSum := summaryBothWays(t, ctx, h, db, tid, preds, "")
		if diffs := diffOrdersSummary(goSum, sqlSum); len(diffs) > 0 {
			t.Fatalf("SQL summary differs from Go summary:\n%s", strings.Join(diffs, "\n"))
		}
		if sqlSum.Count != 2 {
			t.Fatalf("mpesa filter matched %d orders, want 2 (partial mpesa + multi-method)", sqlSum.Count)
		}
	})

	// Guard against a trivially green parity run: the seed must really land in every bucket.
	_, all := summaryBothWays(t, ctx, h, db, tid, tenantOnly, "")
	wantStatus := map[string]int{
		"paid": 8, "partial": 4, "due": 5, "overdue": 2,
		"refunded": 1, "voided": 1, "cancelled": 1, "draft": 1,
	}
	for k, want := range wantStatus {
		if got := all.StatusCounts[k]; got != want {
			t.Errorf("status_counts[%s] = %d, want %d", k, got, want)
		}
	}
	wantMethod := map[string]int{"cash": 7, "multiple": 1, "bank_transfer": 1, "card": 2, "mpesa": 1, "ar_reconciled": 1}
	for k, want := range wantMethod {
		if got := all.MethodCounts[k]; got != want {
			t.Errorf("method_counts[%s] = %d, want %d", k, got, want)
		}
	}
}

// summaryBothWays computes the footer through the Go path and the SQL path over the same
// predicates.
func summaryBothWays(t *testing.T, ctx context.Context, h *POSOrderHandler, db *sql.DB, tid uuid.UUID, preds []predicate.POSOrder, ps string) (ordersSummary, *orders.SummaryAggregates) {
	t.Helper()
	list, err := h.client.POSOrder.Query().Where(preds...).WithLines().WithPayments().All(ctx)
	if err != nil {
		t.Fatalf("load orders: %v", err)
	}
	goSum := buildOrdersSummary(h.enrichOrderList(ctx, tid, list), len(list), false, ps)
	sqlSum, err := orders.SummaryAggregatesSQL(ctx, db, tid, preds, ps, summaryParityNow)
	if err != nil {
		t.Fatalf("SummaryAggregatesSQL: %v", err)
	}
	return goSum, sqlSum
}

type parityPay struct {
	status string
	method *string // nil leaves payment_data unset (legacy tender-only payment)
	tender uuid.UUID
	amount float64
}

type parityRet struct {
	status  posreturn.Status
	channel *posreturn.RefundChannel
	amount  float64
}

type parityOrder struct {
	number      string
	status      string
	total, paid float64
	meta        map[string]any
	lines       int
	pays        []parityPay
	rets        []parityRet
}

// strp is shared with kds_online_test.go in this package.

func chp(c posreturn.RefundChannel) *posreturn.RefundChannel { return &c }

// seedSummaryParityOrders writes one order per settlement branch under a fresh tenant and
// removes everything again when the test ends.
func seedSummaryParityOrders(t *testing.T, ctx context.Context, client *ent.Client, tid uuid.UUID) {
	t.Helper()
	legacy := client.Tender.Create().SetTenantID(tid).SetName("Bank").SetType("bank_transfer").SaveX(ctx)
	card := client.Tender.Create().SetTenantID(tid).SetName("Card").SetType("card").SaveX(ctx)
	generic := uuid.New() // modern payments point at a shared tender that has no row here

	onAcct := func(due any) map[string]any { return map[string]any{"on_account": true, "payment_due_date": due} }
	pay := func(method string, amount float64) parityPay {
		return parityPay{status: "completed", method: strp(method), tender: generic, amount: amount}
	}
	past, future := "2020-01-01T00:00:00Z", "2099-01-01T00:00:00Z"

	seed := []parityOrder{
		{number: "A-paid-cash", status: "completed", total: 100, paid: 100, lines: 2, pays: []parityPay{pay("cash", 100)}},
		{number: "B-partial-mpesa", status: "open", total: 200, paid: 50, lines: 1, pays: []parityPay{pay("mpesa", 50)}},
		{number: "C-due", status: "open", total: 150, lines: 3},
		{number: "D-overdue-on-account", status: "completed", total: 300, meta: onAcct(past), lines: 1},
		{number: "E-on-account-partial", status: "completed", total: 400, paid: 100, meta: onAcct(future), pays: []parityPay{pay("card", 100)}},
		{number: "F-overdue-partial-offset", status: "completed", total: 100, paid: 20, meta: onAcct("2020-06-01T10:00:00.5+03:00"), pays: []parityPay{pay("cash", 20)}},
		{number: "G-voided", status: "voided", total: 80, paid: 80, lines: 2, pays: []parityPay{pay("cash", 80)}},
		{number: "H-cancelled", status: "cancelled", total: 60, lines: 1},
		{number: "I-draft", status: "draft", total: 40, lines: 1},
		{number: "J-cash-return-settles", status: "open", total: 500, paid: 300, lines: 4, pays: []parityPay{pay("cash", 300)},
			rets: []parityRet{{posreturn.StatusCompleted, chp(posreturn.RefundChannelCash), 200}}},
		{number: "K-offset-invoice-return", status: "open", total: 116, paid: 40, lines: 1, pays: []parityPay{pay("ar_reconciled", 40)},
			rets: []parityRet{{posreturn.StatusCompleted, chp(posreturn.RefundChannelOffsetInvoice), 40}}},
		{number: "L-pending-return", status: "open", total: 90, paid: 30, lines: 1, pays: []parityPay{pay("cash", 30)},
			rets: []parityRet{{posreturn.StatusPending, chp(posreturn.RefundChannelCash), 60}, {posreturn.StatusRejected, chp(posreturn.RefundChannelCash), 10}}},
		{number: "M-multi-method", status: "completed", total: 250, paid: 250, lines: 2, pays: []parityPay{
			pay("cash", 100), pay("mpesa", 150), {status: "failed", method: strp("card"), tender: generic, amount: 50}}},
		{number: "N-legacy-tender", status: "completed", total: 70, paid: 70, lines: 1, pays: []parityPay{{status: "completed", tender: legacy.ID, amount: 70}}},
		{number: "O-empty-method-fallback", status: "completed", total: 30, paid: 30, pays: []parityPay{{status: "COMPLETED", method: strp(""), tender: card.ID, amount: 30}}},
		{number: "P-refunded", status: "refunded", total: 120, paid: 120, lines: 1, pays: []parityPay{pay("cash", 120)},
			rets: []parityRet{{posreturn.StatusCompleted, chp(posreturn.RefundChannelCash), 120}}},
		{number: "Q-completed-unpaid", status: "completed", total: 55, lines: 1},
		{number: "R-same-method-twice", status: "completed", total: 60, paid: 60, lines: 1, pays: []parityPay{pay("cash", 30), pay("cash", 30)}},
		{number: "S-null-channel-return", status: "open", total: 100, lines: 1, rets: []parityRet{{posreturn.StatusCompleted, nil, 100}}},
		{number: "T-malformed-due-date", status: "completed", total: 10, meta: onAcct("not-a-date")},
		// Looks like RFC3339 but is not a real day: a database cast would fail the whole query.
		// It must read as "due", exactly like IsOrderOverdue.
		{number: "T2-impossible-due-date", status: "completed", total: 12, meta: onAcct("2026-02-30T00:00:00Z")},
		{number: "U-numeric-due-date", status: "completed", total: 15, meta: onAcct(20200101)},
		{number: "V-zero-total", status: "open", total: 0},
	}

	var ids []uuid.UUID
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = client.POSPayment.Delete().Where(pospayment.OrderIDIn(ids...)).Exec(bg)
		_, _ = client.POSOrderLine.Delete().Where(posorderline.OrderIDIn(ids...)).Exec(bg)
		_, _ = client.POSReturn.Delete().Where(posreturn.TenantID(tid)).Exec(bg)
		_, _ = client.POSOrder.Delete().Where(posorder.TenantID(tid)).Exec(bg)
		_, _ = client.Tender.Delete().Where(tender.TenantID(tid)).Exec(bg)
	})

	outlet, device, user := uuid.New(), uuid.New(), uuid.New()
	for _, so := range seed {
		meta := so.meta
		if meta == nil {
			meta = map[string]any{}
		}
		o := client.POSOrder.Create().
			SetTenantID(tid).SetOutletID(outlet).SetDeviceID(device).SetUserID(user).
			SetOrderNumber(so.number).SetStatus(so.status).
			SetSubtotal(so.total).SetTaxTotal(0).SetTotalAmount(so.total).SetPaidTotal(so.paid).
			SetMetadata(meta).
			SaveX(ctx)
		ids = append(ids, o.ID)
		for l := 0; l < so.lines; l++ {
			client.POSOrderLine.Create().
				SetOrderID(o.ID).SetCatalogItemID(uuid.New()).SetSku("SKU-" + so.number).SetName("Item").
				SetQuantity(1).SetUnitPrice(1).SetTotalPrice(1).
				SaveX(ctx)
		}
		for _, p := range so.pays {
			pc := client.POSPayment.Create().SetOrderID(o.ID).SetTenderID(p.tender).SetAmount(p.amount).SetStatus(p.status)
			if p.method != nil {
				pc.SetPaymentData(map[string]any{"method": *p.method})
			}
			pc.SaveX(ctx)
		}
		for j, r := range so.rets {
			rc := client.POSReturn.Create().
				SetTenantID(tid).SetOutletID(outlet).SetOrderID(o.ID).
				SetReturnNumber(so.number + "-R" + strconv.Itoa(j)).
				SetRequestedBy(user).SetStatus(r.status).SetRefundAmount(r.amount)
			if r.channel != nil {
				rc.SetRefundChannel(*r.channel)
			}
			rc.SaveX(ctx)
		}
	}
}
