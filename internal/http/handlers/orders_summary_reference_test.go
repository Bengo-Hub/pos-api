package handlers

import (
	"fmt"
	"math"
	"sort"

	"github.com/bengobox/pos-service/internal/modules/orders"
)

// The Go reference for the All-Sales totals footer. Production serves the SQL aggregate
// (orders.SummaryAggregatesSQL) since the 2026-11-10 cut-over; this test-only copy of the former
// Go summing path, built on enrichOrderList and orders.ComputeSettlement, is what the parity
// test compares the SQL against, so any drift between the two settlement definitions fails a test.

// buildOrdersSummary sums enriched order rows the way the footer did before the SQL cut-over.
func buildOrdersSummary(items []orderListItem, totalMatching int, truncated bool, paymentStatusFilter string) ordersSummary {
	// paid/partial/due/overdue depend on the server-derived settlement (sell-returns netted in,
	// per orders.ComputeSettlement), so the status filter is an exact match on that derived status.
	if paymentStatusFilter != "" {
		filtered := make([]orderListItem, 0, len(items))
		for _, it := range items {
			if it.PaymentStatus == paymentStatusFilter {
				filtered = append(filtered, it)
			}
		}
		items = filtered
		if !truncated {
			totalMatching = len(items)
		}
	}
	sum := ordersSummary{
		Count:         len(items),
		TotalMatching: totalMatching,
		Truncated:     truncated,
		StatusCounts:  map[string]int{},
		StatusAmounts: map[string]float64{},
		MethodCounts:  map[string]int{},
		MethodAmounts: map[string]float64{},
	}
	for _, it := range items {
		if it.PaymentStatus != "" {
			sum.StatusCounts[it.PaymentStatus]++
			sum.StatusAmounts[it.PaymentStatus] += it.TotalAmount
		}
		// Only settled sales contribute a method; unpaid sales are in the status breakdown.
		if it.PaymentMethod != "" {
			sum.MethodCounts[it.PaymentMethod]++
			sum.MethodAmounts[it.PaymentMethod] += it.TotalAmount
		}
		// Voided/cancelled/draft rows count in the status breakdown but never in the headline
		// figures, which must reconcile with the Retail Overview dashboard and treasury AR.
		if orders.NonCommittedStatus(it.PaymentStatus) {
			continue
		}
		sum.ItemCount += it.ItemCount
		sum.SumTotal += it.TotalAmount
		sum.SumPaid += it.TotalPaid
		sum.SumDue += it.AmountDue
		sum.SumReturn += it.ReturnTotal
	}
	return sum
}

// summaryAmountTolerance absorbs float summation order differences between Go and Postgres.
const summaryAmountTolerance = 0.005

// diffOrdersSummary lists every field where the SQL aggregate differs from the Go reference, as
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

// unionKeys returns the sorted union of two maps' keys, so a key on only one side still shows up.
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
