package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent/predicate"
	"github.com/bengobox/pos-service/internal/modules/orders"
)

// Shadow mode for the SQL-aggregate totals footer (design: .claude/plans/
// pos-orders-summary-sql-aggregates-2026-11-10.md). For one week OrdersSummary keeps serving the
// Go result and, in the background, also computes orders.SummaryAggregatesSQL on the read
// replica, logging orders_summary_parity_mismatch whenever the two disagree. The SQL path can
// never fail or slow the request: it runs after the response is decided, on its own context,
// and any error is only logged.
//
// TODO(2026-11-17): cut over. Serve the SQL result from OrdersSummary, then delete the Go summing
// path (buildOrdersSummary and the capped enrichOrderList load) together with this file.

// summaryShadowEnv switches the shadow off without a code change. Unset or any value other than
// a recognised "off" value leaves it on.
const summaryShadowEnv = "POS_SUMMARY_SQL_SHADOW"

// summaryShadowTimeout bounds one background shadow query.
const summaryShadowTimeout = 15 * time.Second

// summaryShadowSlots caps concurrent shadow queries per pod. When every slot is busy the shadow
// for that request is skipped rather than queued, so a burst of footer loads cannot pile up
// extra replica work.
var summaryShadowSlots = make(chan struct{}, 2)

// SetSummaryShadowDB wires the database the SQL shadow runs on (the read replica's *sql.DB in
// production). Nil, the default, disables the shadow.
func (h *POSOrderHandler) SetSummaryShadowDB(db orders.RowQuerier) { h.summaryShadowDB = db }

// summaryShadowEnabled reads POS_SUMMARY_SQL_SHADOW on every call, so it follows the pod's
// environment without caching. Default on.
func summaryShadowEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(summaryShadowEnv))) {
	case "0", "false", "off", "no", "disabled":
		return false
	}
	return true
}

// shadowOrdersSummary starts the background SQL computation and comparison. goSum is what the
// endpoint is serving. A truncated Go result (more than allSalesExportCap orders) is not a fair
// comparison, so it is skipped.
func (h *POSOrderHandler) shadowOrdersSummary(r *http.Request, tid uuid.UUID, filters []predicate.POSOrder, paymentStatusFilter string, goSum ordersSummary) {
	if h.summaryShadowDB == nil || goSum.Truncated || !summaryShadowEnabled() {
		return
	}
	select {
	case summaryShadowSlots <- struct{}{}:
	default:
		return
	}
	preds := append([]predicate.POSOrder(nil), filters...)
	hash := summaryFilterHash(r)
	ctx := context.WithoutCancel(r.Context())
	db := h.summaryShadowDB
	log := h.log
	go func() {
		defer func() { <-summaryShadowSlots }()
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("orders_summary_sql_shadow_panic", zap.String("tenant_id", tid.String()),
					zap.String("filter_hash", hash), zap.Any("panic", rec))
			}
		}()
		qctx, cancel := context.WithTimeout(ctx, summaryShadowTimeout)
		defer cancel()
		started := time.Now()
		sqlSum, err := orders.SummaryAggregatesSQL(qctx, db, tid, preds, paymentStatusFilter, time.Now())
		if err != nil {
			log.Warn("orders_summary_sql_shadow_failed", zap.String("tenant_id", tid.String()),
				zap.String("filter_hash", hash), zap.Error(err))
			return
		}
		if diffs := diffOrdersSummary(goSum, sqlSum); len(diffs) > 0 {
			log.Warn("orders_summary_parity_mismatch",
				zap.String("tenant_id", tid.String()),
				zap.String("filter_hash", hash),
				zap.String("payment_status", paymentStatusFilter),
				zap.Int("go_count", goSum.Count),
				zap.Strings("diffs", diffs),
				zap.Duration("sql_elapsed", time.Since(started)))
		}
	}()
}

// summaryFilterHash identifies the filter set in logs without writing customer search terms
// (names, phone numbers) into them. url.Values.Encode sorts keys, so the hash is stable.
func summaryFilterHash(r *http.Request) string {
	sum := sha256.Sum256([]byte(r.URL.Query().Encode()))
	return hex.EncodeToString(sum[:])[:16]
}

// summaryAmountTolerance absorbs float summation order differences between Go and Postgres.
const summaryAmountTolerance = 0.005

// diffOrdersSummary lists every field where the SQL aggregate differs from the Go summary, as
// "field: go=X sql=Y". Empty means parity.
func diffOrdersSummary(goSum ordersSummary, sqlSum *orders.SummaryAggregates) []string {
	var diffs []string
	ints := func(name string, g, s int) {
		if g != s {
			diffs = append(diffs, fmt.Sprintf("%s: go=%d sql=%d", name, g, s))
		}
	}
	floats := func(name string, g, s float64) {
		if math.Abs(g-s) > summaryAmountTolerance {
			diffs = append(diffs, fmt.Sprintf("%s: go=%.4f sql=%.4f", name, g, s))
		}
	}
	ints("count", goSum.Count, sqlSum.Count)
	ints("total_matching", goSum.TotalMatching, sqlSum.Count)
	ints("item_count", goSum.ItemCount, sqlSum.ItemCount)
	floats("sum_total", goSum.SumTotal, sqlSum.SumTotal)
	floats("sum_paid", goSum.SumPaid, sqlSum.SumPaid)
	floats("sum_due", goSum.SumDue, sqlSum.SumDue)
	floats("sum_return", goSum.SumReturn, sqlSum.SumReturn)
	for _, k := range unionKeys(goSum.StatusCounts, sqlSum.StatusCounts) {
		ints("status_counts."+k, goSum.StatusCounts[k], sqlSum.StatusCounts[k])
	}
	for _, k := range unionKeys(goSum.StatusAmounts, sqlSum.StatusAmounts) {
		floats("status_amounts."+k, goSum.StatusAmounts[k], sqlSum.StatusAmounts[k])
	}
	for _, k := range unionKeys(goSum.MethodCounts, sqlSum.MethodCounts) {
		ints("method_counts."+k, goSum.MethodCounts[k], sqlSum.MethodCounts[k])
	}
	for _, k := range unionKeys(goSum.MethodAmounts, sqlSum.MethodAmounts) {
		floats("method_amounts."+k, goSum.MethodAmounts[k], sqlSum.MethodAmounts[k])
	}
	return diffs
}

// unionKeys returns the sorted union of two maps' keys, so a status or method present on only
// one side still shows up as a difference.
func unionKeys[V any](a, b map[string]V) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
