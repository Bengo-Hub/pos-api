package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/bengobox/pos-service/internal/ent"
)

// MetaKeyHideProfitMarginForCashier is the OutletSetting.metadata key of the tenant-admin
// "hide profit and margin from cashiers" quick config (CashierPolicyTab). Plain boolean, absent
// means shown, so every tenant keeps showing profit and margin until it opts in. A tenant that
// wants it hidden by default (boi-enterprises) has the key written true on its outlet settings.
// "Cashier" means "not manager-tier" (the caller lacks pos.orders.manage), the same meaning the
// Drafts Delete/Resume toggles use, so custom non-manager roles are covered too.
const MetaKeyHideProfitMarginForCashier = "hide_profit_margin_for_cashier"

type profitHiddenKey struct{}

// ProfitHidden reports whether HideProfitForCashier decided this request must not carry profit or
// margin figures. Handlers that build documents (PDF, CSV) read it to leave those columns out.
func ProfitHidden(ctx context.Context) bool {
	v, _ := ctx.Value(profitHiddenKey{}).(bool)
	return v
}

// profitFields are the JSON keys that carry profit, margin or unit cost. Report and order
// responses are generic maps and structs across many handlers, so they are stripped by key here
// instead of in each handler: a new profit column can only leak by using a name outside this set.
var profitFields = map[string]struct{}{
	"profit": {}, "margin_pct": {}, "gross_profit": {}, "gross_margin_pct": {}, "gross_profit_growth": {},
	"unit_cost": {}, "cost_price": {}, "food_cost_pct": {}, "total_missing_cogs": {}, "skus_missing_cost": {},
}

// profitOnlyPaths are the routes whose whole purpose is profit or margin (suffix match). A hidden
// caller gets 403 on them rather than an empty report.
var profitOnlyPaths = []string{
	"/reports/most-profitable",
	"/reports/most-profitable-document",
	"/reports/profitability-grouped-document",
	"/reports/sales-by-hour-document",
}

// profitHiddenFor says whether the request's caller must not see profit at its outlet.
func profitHiddenFor(r *http.Request, rbac permissionChecker, client *ent.Client) bool {
	if isSuperUserOrPlatformOwner(r.Context()) {
		return false
	}
	outlet := OutletFromContext(r.Context())
	if outlet == nil {
		return false
	}
	setting := getOutletSetting(r.Context(), client, outlet.ID)
	if setting == nil {
		return false
	}
	if hidden, _ := setting.Metadata[MetaKeyHideProfitMarginForCashier].(bool); !hidden {
		return false
	}
	return !HasServicePermission(r, rbac, "pos.orders.manage")
}

// HideProfitForCashier removes profit and margin from what a cashier receives when the outlet's
// hide_profit_margin_for_cashier setting is on. Managers, admins, superusers and platform owners
// are never affected. Attach it to the order read routes and the reports groups. It does nothing
// (no buffering, no rewrite) for callers who are not hidden, and does not apply to streams.
//
//   - JSON responses have every profitFields key removed, at any depth.
//   - Routes in profitOnlyPaths answer 403 profit_hidden.
//   - The request context carries ProfitHidden(ctx) = true for handlers that build documents.
func HideProfitForCashier(client *ent.Client, rbac permissionChecker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if client == nil || !profitHiddenFor(r, rbac, client) {
				next.ServeHTTP(w, r)
				return
			}
			for _, suffix := range profitOnlyPaths {
				if strings.HasSuffix(r.URL.Path, suffix) {
					writeJSON(w, http.StatusForbidden, map[string]string{
						"error":   "profit_hidden",
						"message": "profit and margin are hidden for your role at this outlet",
					})
					return
				}
			}
			r = r.WithContext(context.WithValue(r.Context(), profitHiddenKey{}, true))
			bw := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
			next.ServeHTTP(bw, r)

			body := bw.buf.Bytes()
			if strings.HasPrefix(bw.header.Get("Content-Type"), "application/json") && len(body) > 0 {
				body = stripProfitFields(body)
			}
			dst := w.Header()
			for k, v := range bw.header {
				dst[k] = v
			}
			dst.Del("Content-Length")
			w.WriteHeader(bw.status)
			_, _ = w.Write(body)
		})
	}
}

// bufferedResponse holds a handler's response so the body can be rewritten before it is sent.
type bufferedResponse struct {
	header http.Header
	status int
	buf    bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header         { return b.header }
func (b *bufferedResponse) WriteHeader(status int)      { b.status = status }
func (b *bufferedResponse) Write(p []byte) (int, error) { return b.buf.Write(p) }

// stripProfitFields drops every profitFields key from a JSON document. Numbers are kept as
// written (UseNumber), and a body that is not valid JSON is returned unchanged.
func stripProfitFields(body []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return body
	}
	out, err := json.Marshal(stripValue(doc))
	if err != nil {
		return body
	}
	return out
}

func stripValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k := range t {
			if _, hide := profitFields[k]; hide {
				delete(t, k)
				continue
			}
			t[k] = stripValue(t[k])
		}
		return t
	case []any:
		for i := range t {
			t[i] = stripValue(t[i])
		}
		return t
	}
	return v
}
