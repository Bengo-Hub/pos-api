package orders

import (
	"context"
	stdsql "database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	entposorder "github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

// ============================================================================================
// KEEP IN SYNC: this file is the SQL twin of ComputeSettlement, DerivePaymentStatus,
// IsOnAccount, IsOrderOverdue and NonCommittedStatus (settlement.go), of returnsRollupFor and
// arReducingRefundChannel (handlers/orders.go) and of the method resolution plus dominantMethod
// in handlers/orders.go enrichOrderList. Any change to one of those MUST be made here too, in the
// same commit, and the parity test (handlers/orders_summary_parity_test.go) must stay green.
// ============================================================================================

// SummaryAggregates is the All-Sales totals footer computed in one SQL statement. It carries
// the same figures as the handlers' ordersSummary, without the 5,000 order cap.
type SummaryAggregates struct {
	Count         int
	ItemCount     int
	SumTotal      float64
	SumPaid       float64
	SumDue        float64
	SumReturn     float64
	StatusCounts  map[string]int
	StatusAmounts map[string]float64
	MethodCounts  map[string]int
	MethodAmounts map[string]float64
}

// RowQuerier is the part of *sql.DB the summary query needs (the read replica in production,
// a plain test database in the parity test).
type RowQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*stdsql.Rows, error)
}

// summarySQL is the statement body. {{orders}} is replaced with the Ent-built filtered
// pos_orders SELECT, and the other tokens with bound parameter placeholders. Nothing from the
// request is ever written into this text: every value travels as a parameter.
//
// How each piece maps to the Go path:
//   - ret: returnsRollupFor. Rejected returns are ignored. ret_total is the display ReturnTotal
//     (every non-rejected return). completed is completedReturns: COMPLETED returns only, minus
//     the offset_invoice channel, which ar_reconcile already folds into paid_total.
//     IS DISTINCT FROM keeps a NULL channel included, exactly like arReducingRefundChannel(nil).
//   - lines: len(order.Edges.Lines).
//   - pm and meth: the settled method. payment_data.method when it is a non-empty JSON string,
//     else the tender's type; payments that are not completed (case-insensitive) are skipped.
//     dominantMethod has no precedence order: one distinct method is that method, two or more
//     is "multiple", none is no method.
//   - base_ps: DerivePaymentStatus, branch for branch, with settled = paid_total + completed.
//   - ps: the overdue upgrade from ComputeSettlement. The database never casts a stored due
//     date (an impossible date such as 2026-02-30 would make a cast fail the whole query).
//     Instead the distinct due-date strings in scope are read first, Go decides which have
//     passed with DueDatePassed (the same rule IsOrderOverdue uses), and only those exact
//     strings are bound here as {{past_due}}.
//   - due: max(total - collected - completedReturns, 0); non-committed rows are filtered out of
//     every headline sum, which matches their due of 0 in ComputeSettlement.
const summarySQL = `
WITH o AS ({{orders}}),
ret AS (
	SELECT r.order_id,
	       SUM(r.refund_amount) AS ret_total,
	       COALESCE(SUM(r.refund_amount) FILTER (
	           WHERE r.status = 'completed' AND r.refund_channel IS DISTINCT FROM 'offset_invoice'
	       ), 0) AS completed
	FROM pos_returns r
	WHERE r.tenant_id = {{tenant}}
	  AND r.status <> 'rejected'
	  AND r.order_id IN (SELECT id FROM o)
	GROUP BY r.order_id
),
lines AS (
	SELECT l.order_id, COUNT(*) AS n
	FROM pos_order_lines l
	WHERE l.order_id IN (SELECT id FROM o)
	GROUP BY l.order_id
),
pm AS (
	SELECT p.order_id,
	       CASE WHEN jsonb_typeof(p.payment_data -> 'method') = 'string'
	                 AND (p.payment_data ->> 'method') <> ''
	            THEN p.payment_data ->> 'method'
	            ELSE t.type
	       END AS m
	FROM pos_payments p
	LEFT JOIN tenders t ON t.id = p.tender_id
	WHERE lower(p.status) = 'completed'
	  AND p.order_id IN (SELECT id FROM o)
),
meth AS (
	SELECT order_id,
	       CASE WHEN COUNT(DISTINCT m) = 1 THEN MIN(m) ELSE 'multiple' END AS method
	FROM pm
	WHERE m IS NOT NULL AND m <> ''
	GROUP BY order_id
),
base AS (
	SELECT o.status, o.total_amount, o.paid_total,
	       COALESCE(ret.completed, 0) AS cr,
	       COALESCE(ret.ret_total, 0) AS ret_total,
	       COALESCE(lines.n, 0) AS items,
	       meth.method,
	       COALESCE((o.metadata -> 'on_account') = 'true'::jsonb, false) AS on_account,
	       COALESCE(jsonb_typeof(o.metadata -> 'payment_due_date') = 'string'
	                AND (o.metadata ->> 'payment_due_date') = ANY({{past_due}}::text[]), false) AS overdue
	FROM o
	LEFT JOIN ret ON ret.order_id = o.id
	LEFT JOIN lines ON lines.order_id = o.id
	LEFT JOIN meth ON meth.order_id = o.id
),
st AS (
	SELECT b.*,
	       CASE
	           WHEN b.status IN ('refunded', 'voided', 'cancelled', 'draft') THEN b.status::text
	           WHEN b.total_amount > 0 AND (b.paid_total + b.cr) + 0.01::float8 >= b.total_amount THEN 'paid'
	           WHEN b.on_account AND (b.paid_total + b.cr) > 0 THEN 'partial'
	           WHEN b.on_account THEN 'due'
	           WHEN b.status = 'completed' THEN 'paid'
	           WHEN (b.paid_total + b.cr) > 0 THEN 'partial'
	           ELSE 'due'
	       END AS base_ps
	FROM base b
),
s AS (
	SELECT st.*,
	       CASE WHEN st.base_ps IN ('due', 'partial') AND st.overdue THEN 'overdue' ELSE st.base_ps END AS ps
	FROM st
)
SELECT GROUPING(ps) AS g_ps,
       GROUPING(method) AS g_m,
       ps,
       method,
       COUNT(*) AS n,
       COALESCE(SUM(total_amount), 0) AS amount,
       COALESCE(SUM(items) FILTER (WHERE ps NOT IN ('voided', 'cancelled', 'draft')), 0)::bigint AS item_count,
       COALESCE(SUM(total_amount) FILTER (WHERE ps NOT IN ('voided', 'cancelled', 'draft')), 0) AS sum_total,
       COALESCE(SUM(paid_total) FILTER (WHERE ps NOT IN ('voided', 'cancelled', 'draft')), 0) AS sum_paid,
       COALESCE(SUM(GREATEST(total_amount - paid_total - cr, 0)) FILTER (WHERE ps NOT IN ('voided', 'cancelled', 'draft')), 0) AS sum_due,
       COALESCE(SUM(ret_total) FILTER (WHERE ps NOT IN ('voided', 'cancelled', 'draft')), 0) AS sum_return
FROM s
{{ps_filter}}
GROUP BY GROUPING SETS ((ps), (method), ())`

// buildSummaryQuery renders the filtered pos_orders SELECT through Ent's sql builder, so the
// exact predicates the list uses (allSalesOrderFilters plus own-orders scoping) select the same
// rows here, then appends this statement's own parameters after the builder's.
func buildSummaryQuery(tenantID uuid.UUID, preds []predicate.POSOrder, paymentStatus string, pastDue []string) (string, []any) {
	ordersSQL, args := filteredOrdersSQL(preds)

	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	tenantPH := bind(tenantID)
	if pastDue == nil {
		pastDue = []string{}
	}
	pastDuePH := bind(pastDue)
	psFilter := ""
	if paymentStatus != "" {
		psFilter = "WHERE ps = " + bind(paymentStatus)
	}
	query := strings.NewReplacer(
		"{{orders}}", ordersSQL,
		"{{tenant}}", tenantPH,
		"{{past_due}}", pastDuePH,
		"{{ps_filter}}", psFilter,
	).Replace(summarySQL)
	return query, args
}

// dueDatesSQL lists the distinct payment_due_date strings among the filtered orders, so Go can
// decide which have passed without the database ever casting them.
const dueDatesSQL = `SELECT DISTINCT o.metadata ->> 'payment_due_date'
FROM ({{orders}}) o
WHERE jsonb_typeof(o.metadata -> 'payment_due_date') = 'string'`

// pastDueDates returns the due-date strings in scope that have passed at now (DueDatePassed).
func pastDueDates(ctx context.Context, db RowQuerier, preds []predicate.POSOrder, now time.Time) ([]string, error) {
	ordersSQL, args := filteredOrdersSQL(preds)
	rows, err := db.QueryContext(ctx, strings.Replace(dueDatesSQL, "{{orders}}", ordersSQL, 1), args...)
	if err != nil {
		return nil, fmt.Errorf("orders summary due dates: %w", err)
	}
	defer rows.Close()
	past := []string{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, fmt.Errorf("orders summary due dates scan: %w", err)
		}
		if DueDatePassed(raw, now) {
			past = append(past, raw)
		}
	}
	return past, rows.Err()
}

// filteredOrdersSQL renders the filtered pos_orders SELECT (with its own bound arguments).
func filteredOrdersSQL(preds []predicate.POSOrder) (string, []any) {
	b := entsql.Dialect(dialect.Postgres)
	t := b.Table(entposorder.Table)
	sel := b.Select(t.Columns(
		entposorder.FieldID,
		entposorder.FieldStatus,
		entposorder.FieldTotalAmount,
		entposorder.FieldPaidTotal,
		entposorder.FieldMetadata,
	)...).From(t)
	for _, p := range preds {
		p(sel)
	}
	return sel.Query()
}

// SummaryAggregatesSQL computes the All-Sales totals footer for the orders matching preds in
// ONE statement. paymentStatus, when set, keeps only orders whose derived payment status equals
// it (the exact match the Go path does after enrichment). now decides "overdue", so callers and
// tests control it. preds must already include the tenant predicate.
func SummaryAggregatesSQL(ctx context.Context, db RowQuerier, tenantID uuid.UUID, preds []predicate.POSOrder, paymentStatus string, now time.Time) (*SummaryAggregates, error) {
	pastDue, err := pastDueDates(ctx, db, preds, now)
	if err != nil {
		return nil, err
	}
	query, args := buildSummaryQuery(tenantID, preds, paymentStatus, pastDue)
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("orders summary sql: %w", err)
	}
	defer rows.Close()

	out := &SummaryAggregates{
		StatusCounts:  map[string]int{},
		StatusAmounts: map[string]float64{},
		MethodCounts:  map[string]int{},
		MethodAmounts: map[string]float64{},
	}
	for rows.Next() {
		var (
			gPS, gM                                   int
			ps, method                                stdsql.NullString
			n, items                                  int64
			amount, sumTotal, sumPaid, sumDue, sumRet float64
		)
		if err := rows.Scan(&gPS, &gM, &ps, &method, &n, &amount, &items, &sumTotal, &sumPaid, &sumDue, &sumRet); err != nil {
			return nil, fmt.Errorf("orders summary sql scan: %w", err)
		}
		switch {
		case gPS == 0 && gM == 1:
			// Grouped by payment status: every status counts here, voided and draft included.
			if ps.Valid && ps.String != "" {
				out.StatusCounts[ps.String] = int(n)
				out.StatusAmounts[ps.String] = amount
			}
		case gPS == 1 && gM == 0:
			// Grouped by method: orders with no settled method form a NULL group, skipped like
			// the Go path skips an empty PaymentMethod.
			if method.Valid && method.String != "" {
				out.MethodCounts[method.String] = int(n)
				out.MethodAmounts[method.String] = amount
			}
		case gPS == 1 && gM == 1:
			out.Count = int(n)
			out.ItemCount = int(items)
			out.SumTotal = sumTotal
			out.SumPaid = sumPaid
			out.SumDue = sumDue
			out.SumReturn = sumRet
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("orders summary sql rows: %w", err)
	}
	return out, nil
}
