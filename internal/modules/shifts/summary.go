package shifts

import (
	"context"
	"sort"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"

	"github.com/bengobox/pos-service/internal/audit"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/posorder"
	"github.com/bengobox/pos-service/internal/ent/pospayment"
	"github.com/bengobox/pos-service/internal/ent/posrefund"
	"github.com/bengobox/pos-service/internal/ent/posreturn"
	"github.com/bengobox/pos-service/internal/ent/predicate"
)

// A shift (POSDeviceSession) belongs to the staff member who opened it. Every outlet shares one
// web_terminal device row, and several cashiers' shifts overlap on it all day, so a shift's
// figures are scoped to its user and its time window, never to its device_id. (Before
// 2026-10-23 they were scoped to device_id, which almost every order left as the zero UUID, and
// cash was matched by tender_id, which every payment leaves as the zero UUID, so expected cash
// always came out as the opening float.)
//
// Scope used by every shift figure (the close/auto-close variance, the live summary, the
// history list, the shift report and its PDF):
//   - orders: the shift user's orders rung up inside the window. A synced offline sale counts by
//     when it was rung up (offline_created_at), not when it reached the server.
//   - payments: completed payments taken inside the window and recorded by the shift user
//     (payment_data.collected_by, stamped by audit.TrailHook). Rows from before that stamp
//     existed fall back to the order's cashier.
//   - cash refunds: completed cash-channel returns the shift user completed inside the window.

// MethodCash is the only payment method that puts notes and coins in the drawer.
const MethodCash = "cash"

// Payment method groups used for the summary's flattened totals.
const (
	GroupCash        = "cash"
	GroupCard        = "card"
	GroupMobileMoney = "mobile_money"
	GroupOther       = "other"
)

// MethodGroup buckets a payment_data.method code.
func MethodGroup(method string) string {
	switch NormalizeMethod(method) {
	case "cash":
		return GroupCash
	case "card", "card_manual", "pdq", "card_terminal":
		return GroupCard
	case "mpesa", "mpesa_manual", "payhero", "mtn_momo", "airtel_money", "mobile_money":
		return GroupMobileMoney
	default:
		return GroupOther
	}
}

// NormalizeMethod lowercases a method code and folds the legacy "manual" alias.
func NormalizeMethod(method string) string {
	m := strings.ToLower(strings.TrimSpace(method))
	if m == "manual" {
		return "mpesa_manual"
	}
	if m == "" {
		return "unknown"
	}
	return m
}

// Window is the span a shift covers: opened_at to closed_at, or to now while it is open.
func Window(s *ent.POSDeviceSession) (time.Time, time.Time) {
	if s.ClosedAt != nil {
		return s.OpenedAt, *s.ClosedAt
	}
	return s.OpenedAt, time.Now()
}

// MethodTotal is the takings for one payment method.
type MethodTotal struct {
	Method string  `json:"method"`
	Group  string  `json:"group"`
	Amount float64 `json:"amount"`
	Count  int     `json:"count"`
}

// Summary is everything a shift screen or report shows.
type Summary struct {
	OrderCount    int           `json:"order_count"`
	Revenue       float64       `json:"total_revenue"`
	Tax           float64       `json:"total_tax"`
	Discount      float64       `json:"total_discounts"`
	VoidCount     int           `json:"void_count"`
	Refunds       float64       `json:"total_refunds"`
	RefundCount   int           `json:"refund_count"`
	CashRefunds   float64       `json:"cash_refunds"`
	Methods       []MethodTotal `json:"methods"`
	CashTaken     float64       `json:"cash_in_total"`
	CardTotal     float64       `json:"card_total"`
	MobileMoney   float64       `json:"mpesa_total"`
	PaymentsTotal float64       `json:"payments_total"`
	OpeningFloat  float64       `json:"opening_float"`
	// ExpectedCash = opening float + cash taken - cash refunded.
	ExpectedCash float64 `json:"expected_cash"`
	Currency     string  `json:"currency,omitempty"`
}

// Summarize computes a shift's figures with four bounded queries (order aggregates, refunds,
// payments in the window, cash returns in the window). Every query is scoped by an indexed
// time window, so the cost tracks one shift's activity, not the tenant's history.
func Summarize(ctx context.Context, client *ent.Client, session *ent.POSDeviceSession) (Summary, error) {
	from, to := Window(session)
	tid, uid := session.TenantID, session.UserID
	sum := Summary{OpeningFloat: session.FloatAmount}

	var rows []struct {
		Status   string  `json:"status"`
		Count    int     `json:"count"`
		Revenue  float64 `json:"revenue"`
		Tax      float64 `json:"tax"`
		Discount float64 `json:"discount"`
	}
	err := client.POSOrder.Query().
		Where(ordersInShift(tid, uid, from, to), posorder.StatusIn("completed", "voided")).
		GroupBy(posorder.FieldStatus).
		Aggregate(ent.Count(), sumAs(posorder.FieldTotalAmount, "revenue"),
			sumAs(posorder.FieldTaxTotal, "tax"), sumAs(posorder.FieldDiscountTotal, "discount")).
		Scan(ctx, &rows)
	if err != nil {
		return sum, err
	}
	for _, r := range rows {
		switch r.Status {
		case "completed":
			sum.OrderCount, sum.Revenue, sum.Tax, sum.Discount = r.Count, r.Revenue, r.Tax, r.Discount
		case "voided":
			sum.VoidCount = r.Count
		}
	}

	if cur, cerr := client.POSOrder.Query().
		Where(ordersInShift(tid, uid, from, to), posorder.CurrencyNEQ("")).
		Select(posorder.FieldCurrency).
		First(ctx); cerr == nil {
		sum.Currency = cur.Currency
	}

	orderIDs, err := client.POSOrder.Query().Where(ordersInShift(tid, uid, from, to)).IDs(ctx)
	if err != nil {
		return sum, err
	}
	if len(orderIDs) > 0 {
		refunds, rerr := client.POSRefund.Query().
			Where(posrefund.OrderIDIn(orderIDs...), posrefund.StatusEQ("completed")).
			All(ctx)
		if rerr != nil {
			return sum, rerr
		}
		for _, rf := range refunds {
			sum.Refunds += rf.Amount
			sum.RefundCount++
		}
	}

	payments, err := client.POSPayment.Query().
		Where(PaymentsCollectedInShift(tid, uid, from, to)).
		Select(pospayment.FieldAmount, pospayment.FieldPaymentData).
		All(ctx)
	if err != nil {
		return sum, err
	}
	byMethod := map[string]*MethodTotal{}
	for _, p := range payments {
		method, _ := p.PaymentData["method"].(string)
		method = NormalizeMethod(method)
		mt := byMethod[method]
		if mt == nil {
			mt = &MethodTotal{Method: method, Group: MethodGroup(method)}
			byMethod[method] = mt
		}
		mt.Amount += p.Amount
		mt.Count++
		sum.PaymentsTotal += p.Amount
		switch mt.Group {
		case GroupCash:
			sum.CashTaken += p.Amount
		case GroupCard:
			sum.CardTotal += p.Amount
		case GroupMobileMoney:
			sum.MobileMoney += p.Amount
		}
	}
	sum.Methods = make([]MethodTotal, 0, len(byMethod))
	for _, mt := range byMethod {
		sum.Methods = append(sum.Methods, *mt)
	}
	sort.Slice(sum.Methods, func(i, j int) bool {
		if sum.Methods[i].Amount != sum.Methods[j].Amount {
			return sum.Methods[i].Amount > sum.Methods[j].Amount
		}
		return sum.Methods[i].Method < sum.Methods[j].Method
	})

	cashRefunds, err := cashReturnsCompletedInShift(ctx, client, tid, uid, from, to)
	if err != nil {
		return sum, err
	}
	sum.CashRefunds = cashRefunds
	sum.ExpectedCash = session.FloatAmount + sum.CashTaken - cashRefunds
	return sum, nil
}

// ordersInShift matches the user's orders rung up inside [from, to]: by offline_created_at for
// synced offline sales, else created_at. Both branches have their own index.
func ordersInShift(tid, uid uuid.UUID, from, to time.Time) predicate.POSOrder {
	return posorder.And(
		posorder.TenantID(tid),
		posorder.UserID(uid),
		posorder.Or(
			posorder.And(posorder.OfflineCreatedAtNotNil(), posorder.OfflineCreatedAtGTE(from), posorder.OfflineCreatedAtLTE(to)),
			posorder.And(posorder.OfflineCreatedAtIsNil(), posorder.CreatedAtGTE(from), posorder.CreatedAtLTE(to)),
		),
	)
}

// PaymentsCollectedInShift matches completed payments taken inside [from, to] by uid: recorded
// by them (payment_data.collected_by) or, for rows written before that key existed, taken on one
// of their orders.
func PaymentsCollectedInShift(tid, uid uuid.UUID, from, to time.Time) predicate.POSPayment {
	collectedBy := func(s *entsql.Selector) {
		s.Where(sqljson.ValueEQ(s.C(pospayment.FieldPaymentData), uid.String(), sqljson.Path(audit.KeyCollectedBy)))
	}
	noCollector := func(s *entsql.Selector) {
		s.Where(entsql.ExprP(s.C(pospayment.FieldPaymentData) + " ->> '" + audit.KeyCollectedBy + "' IS NULL"))
	}
	return pospayment.And(
		pospayment.StatusEQ("completed"),
		pospayment.OccurredAtGTE(from),
		pospayment.OccurredAtLTE(to),
		pospayment.HasOrderWith(posorder.TenantID(tid)),
		pospayment.Or(
			collectedBy,
			pospayment.And(noCollector, pospayment.HasOrderWith(posorder.UserID(uid))),
		),
	)
}

// cashReturnsCompletedInShift sums the cash handed back for returns uid completed inside
// [from, to] (returns.Service.CompleteReturn stamps metadata completed_by and completed_at).
// Exchanges are left out: their cash leftover is a separate, smaller flow not tracked per user.
func cashReturnsCompletedInShift(ctx context.Context, client *ent.Client, tid, uid uuid.UUID, from, to time.Time) (float64, error) {
	rets, err := client.POSReturn.Query().
		Where(
			posreturn.TenantID(tid),
			posreturn.StatusEQ(posreturn.StatusCompleted),
			posreturn.RefundChannelEQ(posreturn.RefundChannelCash),
			posreturn.ReturnTypeNEQ(posreturn.ReturnTypeExchange),
			posreturn.UpdatedAtGTE(from),
		).
		All(ctx)
	if err != nil {
		return 0, err
	}
	var total float64
	for _, r := range rets {
		by, _ := r.Metadata["completed_by"].(string)
		if by == "" {
			if r.RequestedBy == uuid.Nil || r.RequestedBy != uid {
				continue
			}
		} else if by != uid.String() {
			continue
		}
		at := r.UpdatedAt
		if s, _ := r.Metadata["completed_at"].(string); s != "" {
			if t, perr := time.Parse(time.RFC3339, s); perr == nil {
				at = t
			}
		}
		if at.Before(from) || at.After(to) {
			continue
		}
		total += r.RefundAmount
	}
	return total, nil
}

// OrderTotals is the per-shift order count and revenue for a list view.
type OrderTotals struct {
	OrderCount int
	Revenue    float64
}

// OrderTotalsForSessions computes OrderTotals for many shifts of ONE user with a single query
// over the span they cover, bucketing in memory, instead of one query per row.
func OrderTotalsForSessions(ctx context.Context, client *ent.Client, tid, uid uuid.UUID, sessions []*ent.POSDeviceSession) (map[uuid.UUID]OrderTotals, error) {
	out := make(map[uuid.UUID]OrderTotals, len(sessions))
	if len(sessions) == 0 {
		return out, nil
	}
	spanFrom, spanTo := Window(sessions[0])
	for _, s := range sessions[1:] {
		f, t := Window(s)
		if f.Before(spanFrom) {
			spanFrom = f
		}
		if t.After(spanTo) {
			spanTo = t
		}
	}
	orders, err := client.POSOrder.Query().
		Where(ordersInShift(tid, uid, spanFrom, spanTo), posorder.StatusEQ("completed")).
		Select(posorder.FieldCreatedAt, posorder.FieldOfflineCreatedAt, posorder.FieldTotalAmount).
		All(ctx)
	if err != nil {
		return out, err
	}
	for _, s := range sessions {
		if s.UserID != uid {
			continue
		}
		f, t := Window(s)
		var tot OrderTotals
		for _, o := range orders {
			at := o.CreatedAt
			if o.OfflineCreatedAt != nil {
				at = *o.OfflineCreatedAt
			}
			if !at.Before(f) && !at.After(t) {
				tot.OrderCount++
				tot.Revenue += o.TotalAmount
			}
		}
		out[s.ID] = tot
	}
	return out, nil
}

// sumAs is an aliased SUM; ent's bare Sum() names every column "sum", so two in one query collide.
func sumAs(field, alias string) ent.AggregateFunc {
	return func(s *entsql.Selector) string {
		return entsql.As(entsql.Sum(s.C(field)), alias)
	}
}
