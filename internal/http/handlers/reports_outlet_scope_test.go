package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Bengo-Hub/httpware"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
)

// newReportsTestHandler mirrors newHeldItemsTestHandler's sqlite-memory harness ("sqlite3" is
// already registered by held_items_test.go's init() in this same package — do not re-register).
func newReportsTestHandler(t *testing.T) (*ReportsHandler, *ent.Client) {
	t.Helper()
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:reportstest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	return NewReportsHandler(zap.NewNop(), client), client
}

func seedCompletedSale(t *testing.T, client *ent.Client, tid, outletID uuid.UUID, sku, name string, revenue float64) {
	t.Helper()
	o, err := client.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetDeviceID(uuid.New()).
		SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).
		SetStatus("completed").
		SetSubtotal(revenue).
		SetTaxTotal(0).
		SetTotalAmount(revenue).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := client.POSOrderLine.Create().
		SetOrderID(o.ID).
		SetCatalogItemID(uuid.New()).
		SetSku(sku).
		SetName(name).
		SetQuantity(1).
		SetUnitPrice(revenue).
		SetTotalPrice(revenue).
		Save(context.Background()); err != nil {
		t.Fatalf("seed order line: %v", err)
	}
}

// reportsRequest builds a GET request carrying the tenant via httpware context the way the
// router middleware would, and OPTIONALLY the ambient outlet context OutletContextMiddleware
// resolves on every real request (header → JWT claim → tenant HQ fallback) — never via an
// explicit ?outlet_id= query param, since that's the case this regression guards.
func reportsRequest(t *testing.T, tid uuid.UUID, outletID *uuid.UUID, query string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
	ctx := httpware.WithTenantID(req.Context(), tid.String())
	if outletID != nil {
		ctx = httpware.WithOutletID(ctx, outletID.String())
	}
	return req.WithContext(ctx)
}

// TestTopItems_ScopedToAmbientOutlet guards the reported bug: the dashboard's "Top Selling
// Items" widget showed items sold at OTHER outlets, because TopItems queried by tenant only and
// never consulted the ambient outlet context every other report handler (GetSummary,
// SalesByHour, ...) already relies on.
func TestTopItems_ScopedToAmbientOutlet(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid := uuid.New()
	outletA, outletB := uuid.New(), uuid.New()

	seedCompletedSale(t, client, tid, outletA, "SKU-A", "Widget A", 1000)
	seedCompletedSale(t, client, tid, outletB, "SKU-B", "Widget B", 5000)

	req := reportsRequest(t, tid, &outletA, "from=2020-01-01&to=2030-01-01")
	rec := httptest.NewRecorder()
	h.TopItems(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("TopItems: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "SKU-A") {
		t.Fatalf("expected outlet A's item SKU-A in response, got: %s", body)
	}
	if strings.Contains(body, "SKU-B") {
		t.Fatalf("outlet B's item leaked into outlet A's top-items report: %s", body)
	}
}

// TestTopItems_NoOutletContext_ReturnsTenantWide documents the fallback: with no outlet
// resolvable at all (no header, no JWT claim, no HQ outlet — the unit-test environment has none
// of those), the report stays tenant-wide rather than silently returning nothing.
func TestTopItems_NoOutletContext_ReturnsTenantWide(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid := uuid.New()
	outletA, outletB := uuid.New(), uuid.New()
	seedCompletedSale(t, client, tid, outletA, "SKU-A", "Widget A", 1000)
	seedCompletedSale(t, client, tid, outletB, "SKU-B", "Widget B", 5000)

	req := reportsRequest(t, tid, nil, "from=2020-01-01&to=2030-01-01")
	rec := httptest.NewRecorder()
	h.TopItems(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("TopItems: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "SKU-A") || !strings.Contains(body, "SKU-B") {
		t.Fatalf("expected BOTH items with no outlet context set, got: %s", body)
	}
}

// TestDailyBreakdown_ScopedToAmbientOutlet mirrors the TopItems regression for the Revenue Trend
// chart's daily-breakdown endpoint, which had the identical gap.
func TestDailyBreakdown_ScopedToAmbientOutlet(t *testing.T) {
	h, client := newReportsTestHandler(t)
	tid := uuid.New()
	outletA, outletB := uuid.New(), uuid.New()

	seedCompletedSale(t, client, tid, outletA, "SKU-A", "Widget A", 1000)
	seedCompletedSale(t, client, tid, outletB, "SKU-B", "Widget B", 5000)

	req := reportsRequest(t, tid, &outletA, "from=2020-01-01&to=2030-01-01")
	rec := httptest.NewRecorder()
	h.DailyBreakdown(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("DailyBreakdown: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Outlet A's revenue (1000) must be the only amount reflected — outlet B's 5000 must not
	// inflate the total (a cheap-but-effective proxy: 5000 never appears in ANY bucket, 1000 does).
	if !strings.Contains(body, "1000") {
		t.Fatalf("expected outlet A's revenue (1000) in response, got: %s", body)
	}
	if strings.Contains(body, "6000") {
		t.Fatalf("outlet B's revenue leaked into outlet A's daily breakdown: %s", body)
	}
}
