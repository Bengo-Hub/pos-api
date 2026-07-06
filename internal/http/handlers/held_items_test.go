package handlers

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"modernc.org/sqlite"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/enttest"
	entkdsticket "github.com/bengobox/pos-service/internal/ent/kdsticket"
	entposorderline "github.com/bengobox/pos-service/internal/ent/posorderline"
)

// ── pure-Go sqlite shim ─────────────────────────────────────────────────────
// ent expects a driver registered as "sqlite3"; modernc.org/sqlite (no cgo — this repo builds with
// CGO_ENABLED=0) registers as "sqlite". Bridge it and force foreign keys on per connection.

type sqlite3Driver struct{ *sqlite.Driver }

func (d sqlite3Driver) Open(name string) (driver.Conn, error) {
	conn, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	if execer, ok := conn.(interface {
		Exec(string, []driver.Value) (driver.Result, error)
	}); ok {
		if _, err := execer.Exec("PRAGMA foreign_keys = ON;", nil); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	return conn, nil
}

func init() {
	sql.Register("sqlite3", sqlite3Driver{Driver: &sqlite.Driver{}})
}

// ── helpers ────────────────────────────────────────────────────────────────

const testTerminalSecret = "held-items-test-secret"

func newHeldItemsTestHandler(t *testing.T) (*POSOrderHandler, *ent.Client) {
	t.Helper()
	client := enttest.Open(t, "sqlite3", fmt.Sprintf("file:heldtest_%s?mode=memory&cache=shared", uuid.NewString()))
	t.Cleanup(func() { _ = client.Close() })
	h := NewPOSOrderHandler(zap.NewNop(), client, nil, nil)
	h.terminalSecret = []byte(testTerminalSecret)
	return h, client
}

func seedOrder(t *testing.T, client *ent.Client, tid, outletID uuid.UUID, status string) *ent.POSOrder {
	t.Helper()
	o, err := client.POSOrder.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetDeviceID(uuid.New()).
		SetUserID(uuid.New()).
		SetOrderNumber("ORD-" + uuid.NewString()[:8]).
		SetStatus(status).
		SetSubtotal(500).
		SetTaxTotal(0).
		SetTotalAmount(500).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	return o
}

func seedHeldItem(t *testing.T, client *ent.Client, tid, outletID, sourceOrderID uuid.UUID) *ent.HeldItem {
	t.Helper()
	hi, err := client.HeldItem.Create().
		SetTenantID(tid).
		SetOutletID(outletID).
		SetSourceOrderID(sourceOrderID).
		SetName("Sparkling Water").
		SetSku("SPW-01").
		SetQuantity(1).
		SetUnitPrice(250).
		SetHeldByUserID(uuid.New()).
		Save(context.Background())
	if err != nil {
		t.Fatalf("seed held item: %v", err)
	}
	return hi
}

// heldItemRequest builds a POST request for /held-items/{id}/(claim|void) with chi URL params and
// auth claims wired the way the router middleware would.
func heldItemRequest(t *testing.T, tid, heldID uuid.UUID, body map[string]any, roles []string) *http.Request {
	t.Helper()
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(buf))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("tenantID", tid.String())
	rctx.URLParams.Add("id", heldID.String())
	ctx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	// parseTenantUUID reads the tenant from the httpware middleware context, not the URL param.
	ctx = httpware.WithTenantID(ctx, tid.String())
	ctx = authclient.ContextWithClaims(ctx, &authclient.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: uuid.NewString()},
		TenantID:         tid.String(),
		Roles:            roles,
	})
	return req.WithContext(ctx)
}

// ── ClaimHeldItem ──────────────────────────────────────────────────────────

func TestClaimHeldItem_MergesLineIntoTargetOrder(t *testing.T) {
	h, client := newHeldItemsTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()
	source := seedOrder(t, client, tid, outletID, "open")
	target := seedOrder(t, client, tid, outletID, "open")
	held := seedHeldItem(t, client, tid, outletID, source.ID)

	req := heldItemRequest(t, tid, held.ID, map[string]any{"claimed_order_id": target.ID.String()}, nil)
	rec := httptest.NewRecorder()
	h.ClaimHeldItem(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("claim: got %d, want 200 — body %s", rec.Code, rec.Body.String())
	}

	ctx := context.Background()
	// A real line was appended to the target order at the held price.
	lines, err := client.POSOrderLine.Query().Where(entposorderline.OrderID(target.ID)).All(ctx)
	if err != nil || len(lines) != 1 {
		t.Fatalf("target order lines = %d (err %v), want 1", len(lines), err)
	}
	if lines[0].Name != "Sparkling Water" || lines[0].TotalPrice != 250 {
		t.Fatalf("claimed line = %q @ %.2f, want Sparkling Water @ 250", lines[0].Name, lines[0].TotalPrice)
	}
	// Totals grew by exactly the line price (price-only, mirroring set-aside).
	updated := client.POSOrder.GetX(ctx, target.ID)
	if updated.Subtotal != 750 || updated.TotalAmount != 750 {
		t.Fatalf("target totals = %.2f/%.2f, want 750/750", updated.Subtotal, updated.TotalAmount)
	}
	// The held item is resolved with the claiming order recorded.
	reloaded := client.HeldItem.GetX(ctx, held.ID)
	if reloaded.Status != "claimed" || reloaded.ClaimedOrderID == nil || *reloaded.ClaimedOrderID != target.ID {
		t.Fatalf("held item status=%s claimed_order_id=%v, want claimed/%s", reloaded.Status, reloaded.ClaimedOrderID, target.ID)
	}
	// The item was already prepared — NO KDS ticket may fire for the claim.
	if n := client.KDSTicket.Query().Where(entkdsticket.OrderID(target.ID)).CountX(ctx); n != 0 {
		t.Fatalf("KDS tickets for claimed line = %d, want 0", n)
	}
}

func TestClaimHeldItem_RequiresClaimedOrderID(t *testing.T) {
	h, client := newHeldItemsTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()
	source := seedOrder(t, client, tid, outletID, "open")
	held := seedHeldItem(t, client, tid, outletID, source.ID)

	rec := httptest.NewRecorder()
	h.ClaimHeldItem(rec, heldItemRequest(t, tid, held.ID, map[string]any{}, nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("claim without order id: got %d, want 400", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "claimed_order_required" {
		t.Fatalf("error code = %q, want claimed_order_required", body["code"])
	}
}

func TestClaimHeldItem_RejectsClosedOrder(t *testing.T) {
	h, client := newHeldItemsTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()
	source := seedOrder(t, client, tid, outletID, "open")
	target := seedOrder(t, client, tid, outletID, "completed")
	held := seedHeldItem(t, client, tid, outletID, source.ID)

	rec := httptest.NewRecorder()
	h.ClaimHeldItem(rec, heldItemRequest(t, tid, held.ID, map[string]any{"claimed_order_id": target.ID.String()}, nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("claim into completed order: got %d, want 400", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "order_not_open" {
		t.Fatalf("error code = %q, want order_not_open", body["code"])
	}
}

func TestClaimHeldItem_RejectsCrossOutletOrder(t *testing.T) {
	h, client := newHeldItemsTestHandler(t)
	tid := uuid.New()
	outletA, outletB := uuid.New(), uuid.New()
	source := seedOrder(t, client, tid, outletA, "open")
	target := seedOrder(t, client, tid, outletB, "open")
	held := seedHeldItem(t, client, tid, outletA, source.ID)

	rec := httptest.NewRecorder()
	h.ClaimHeldItem(rec, heldItemRequest(t, tid, held.ID, map[string]any{"claimed_order_id": target.ID.String()}, nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cross-outlet claim: got %d, want 400", rec.Code)
	}
}

func TestClaimHeldItem_RejectsAlreadyResolved(t *testing.T) {
	h, client := newHeldItemsTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()
	source := seedOrder(t, client, tid, outletID, "open")
	target := seedOrder(t, client, tid, outletID, "open")
	held := seedHeldItem(t, client, tid, outletID, source.ID)
	client.HeldItem.UpdateOneID(held.ID).SetStatus("voided").SaveX(context.Background())

	rec := httptest.NewRecorder()
	h.ClaimHeldItem(rec, heldItemRequest(t, tid, held.ID, map[string]any{"claimed_order_id": target.ID.String()}, nil))

	if rec.Code != http.StatusConflict {
		t.Fatalf("claim resolved item: got %d, want 409", rec.Code)
	}
}

// ── VoidHeldItem (manager approval) ────────────────────────────────────────

func TestVoidHeldItem_RequiresManagerApproval(t *testing.T) {
	h, client := newHeldItemsTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()
	source := seedOrder(t, client, tid, outletID, "open")
	held := seedHeldItem(t, client, tid, outletID, source.ID)

	// A plain waiter with no approval token is refused with the machine-readable code the UI
	// branches on to open the manager step-up dialog.
	rec := httptest.NewRecorder()
	h.VoidHeldItem(rec, heldItemRequest(t, tid, held.ID, map[string]any{"reason": "end of day"}, []string{"waiter"}))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("waiter void without approval: got %d, want 403", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "approval_required" {
		t.Fatalf("error code = %q, want approval_required", body["code"])
	}
	if client.HeldItem.GetX(context.Background(), held.ID).Status != "held" {
		t.Fatal("held item must stay held after a refused void")
	}
}

func TestVoidHeldItem_ApprovalTokenAllowsWaiterVoid(t *testing.T) {
	h, client := newHeldItemsTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()
	source := seedOrder(t, client, tid, outletID, "open")
	held := seedHeldItem(t, client, tid, outletID, source.ID)

	approver := uuid.New()
	token, err := issueApprovalToken("held_item.void", approver, uuid.New(), outletID, []byte(testTerminalSecret))
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	rec := httptest.NewRecorder()
	h.VoidHeldItem(rec, heldItemRequest(t, tid, held.ID,
		map[string]any{"reason": "end of day", "approval_token": token}, []string{"waiter"}))

	if rec.Code != http.StatusOK {
		t.Fatalf("approved void: got %d, want 200 — body %s", rec.Code, rec.Body.String())
	}
	if client.HeldItem.GetX(context.Background(), held.ID).Status != "voided" {
		t.Fatal("held item should be voided after approved void")
	}
}

func TestVoidHeldItem_WrongActionTokenRejected(t *testing.T) {
	h, client := newHeldItemsTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()
	source := seedOrder(t, client, tid, outletID, "open")
	held := seedHeldItem(t, client, tid, outletID, source.ID)

	// A token minted for a DIFFERENT action must not authorize a held-item void.
	token, _ := issueApprovalToken("order.void", uuid.New(), uuid.New(), outletID, []byte(testTerminalSecret))
	rec := httptest.NewRecorder()
	h.VoidHeldItem(rec, heldItemRequest(t, tid, held.ID,
		map[string]any{"reason": "x", "approval_token": token}, []string{"waiter"}))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong-action token: got %d, want 403", rec.Code)
	}
}

func TestVoidHeldItem_ManagerSelfApproves(t *testing.T) {
	h, client := newHeldItemsTestHandler(t)
	tid, outletID := uuid.New(), uuid.New()
	source := seedOrder(t, client, tid, outletID, "open")
	held := seedHeldItem(t, client, tid, outletID, source.ID)

	rec := httptest.NewRecorder()
	h.VoidHeldItem(rec, heldItemRequest(t, tid, held.ID, map[string]any{"reason": "eod"}, []string{"manager"}))

	if rec.Code != http.StatusOK {
		t.Fatalf("manager self-void: got %d, want 200 — body %s", rec.Code, rec.Body.String())
	}
	if client.HeldItem.GetX(context.Background(), held.ID).Status != "voided" {
		t.Fatal("held item should be voided by manager self-approval")
	}
}
