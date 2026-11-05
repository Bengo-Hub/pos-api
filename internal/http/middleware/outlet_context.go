package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
	entstaffoutlet "github.com/bengobox/pos-service/internal/ent/staffoutlet"
	"github.com/bengobox/pos-service/internal/posrole"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type outletContextKey struct{}

// OutletContext carries the resolved outlet for the current request.
type OutletContext struct {
	ID      uuid.UUID
	Code    string
	Name    string
	UseCase string
	IsHQ    bool
	Status  string
}

// OutletFromContext retrieves the resolved outlet from the request context.
func OutletFromContext(ctx context.Context) *OutletContext {
	v, _ := ctx.Value(outletContextKey{}).(*OutletContext)
	return v
}

// OutletContextMiddleware resolves the active outlet for the request and injects
// it into the context. Resolution order mirrors TruLoad's TenantContext middleware:
//
//  1. X-Outlet-ID header    — explicit override (HQ user / frontend-selected outlet)
//  2. JWT claim's outlet_id — terminal sessions (PIN login) whose token already carries
//     the outlet they logged into
//  3. Tenant's HQ outlet    — fallback when no outlet can be determined
//
// For non-HQ users: if X-Outlet-ID doesn't match the user's assigned outlet → 403.
func OutletContextMiddleware(client *ent.Client, log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			claims, hasClaims := authclient.ClaimsFromContext(ctx)

			// ── Resolve tenant ID ─────────────────────────────────────────────
			var tenantID uuid.UUID
			if hasClaims && claims.TenantID != "" {
				if id, err := uuid.Parse(claims.TenantID); err == nil {
					tenantID = id
				}
			}
			if tenantID == uuid.Nil {
				next.ServeHTTP(w, r)
				return
			}

			// ── Determine whether user is HQ/admin (can see all outlets) ─────
			// claims.CanAccessAllOutlets() covers platform owner / IsAdmin() / the session's own
			// outlet being HQ, but the vendored auth-client claims helper doesn't recognize the
			// "manager" role — so also allow via the caller's own role list.
			isHQUser := hasClaims && claimsSeeAllOutlets(claims)

			// ── Resolve requested outlet from header ──────────────────────────
			var requestedOutletID uuid.UUID
			if raw := r.Header.Get(httpware.HeaderOutletID); raw != "" {
				if id, err := uuid.Parse(raw); err == nil {
					requestedOutletID = id
				}
			}

			var resolved *OutletContext

			if requestedOutletID != uuid.Nil {
				o, err := client.Outlet.Query().
					Where(entoutlet.ID(requestedOutletID), entoutlet.TenantID(tenantID)).
					Only(ctx)
				if err != nil {
					http.Error(w, `{"error":"outlet not found or access denied"}`, http.StatusForbidden)
					return
				}
				// A limited manager holds exactly their list, HQ outlet included. An outlet off the
				// list is ignored rather than refused and the fallback below opens one they hold: a
				// browser still remembering an outlet from before the limit must not lock them out.
				if hasClaims && claims.OutletRestricted() {
					if claims.AllowsOutlet(o.ID.String()) {
						resolved = outletToCtx(o)
					}
				} else if !isHQUser && !o.IsHq {
					// Non-HQ users can only request their assigned outlet.
					if hasClaims && claims.Subject != "" {
						if userID, err := uuid.Parse(claims.Subject); err == nil {
							if !isStaffAssignedToOutlet(ctx, client, tenantID, userID, o.ID) {
								http.Error(w, `{"error":"outlet not assigned to this user"}`, http.StatusForbidden)
								return
							}
						}
					}
				}
				if !hasClaims || !claims.OutletRestricted() {
					resolved = outletToCtx(o)
				}
			}

			// ── Fallback: the caller's own assigned outlet from their JWT claim ───
			// Chiefly terminal (PIN login) sessions, whose claim already carries the outlet
			// they logged into. Trusted as-is (no isHQUser/isStaffAssignedToOutlet check) —
			// it's the outlet the token issuer assigned to this exact user at login, not a
			// caller-supplied override.
			if resolved == nil && hasClaims {
				raw := claims.GetOutletID()
				if claims.OutletRestricted() {
					raw = claims.RestrictedHomeOutlet() // never the HQ fallback below
				}
				if raw != "" {
					if id, err := uuid.Parse(raw); err == nil {
						o, err := client.Outlet.Query().
							Where(entoutlet.ID(id), entoutlet.TenantID(tenantID)).
							Only(ctx)
						if err == nil {
							resolved = outletToCtx(o)
						}
					}
				}
			}

			// ── Fallback: HQ outlet for this tenant ───────────────────────────
			if resolved == nil {
				o, err := client.Outlet.Query().
					Where(entoutlet.TenantID(tenantID), entoutlet.IsHq(true)).
					First(ctx)
				if err == nil {
					resolved = outletToCtx(o)
				}
			}

			if resolved != nil {
				ctx = context.WithValue(ctx, outletContextKey{}, resolved)
				// Also populate the lightweight httpware key so handlers can call httpware.GetOutletID().
				ctx = httpware.WithOutletID(ctx, resolved.ID.String())
			}
			// What RequestOutletScope needs to check an ?outlet_id= the caller names.
			ctx = context.WithValue(ctx, outletAccessKey{}, outletAccess{client: client, tenantID: tenantID})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

type outletAccessKey struct{}

type outletAccess struct {
	client   *ent.Client
	tenantID uuid.UUID
}

// RequestOutletScope is the one rule every read that takes ?outlet_id= uses (the All-Sales list,
// summary and export, and the analytics, register, KDS and hotel reports). It returns the outlet
// to filter by, or nil for every outlet:
//   - outlet_id=all: every outlet, for HQ-level callers only
//   - outlet_id=<id>: that outlet when the caller may see it (CanViewOutlet)
//   - absent, invalid or not allowed: the request's outlet context (X-Outlet-ID, the token's
//     outlet, else the HQ outlet)
//
// Before it, four copies of this logic took any outlet id from any caller and ignored "all", so a
// report could never span every outlet and branch staff could read another branch by editing the
// URL.
func RequestOutletScope(r *http.Request) *uuid.UUID {
	// The request's outlet: the middleware's OutletContext, else the lightweight httpware key
	// (the middleware sets both; some callers and tests set only the httpware one).
	contextOutlet := func() *uuid.UUID {
		if oc := OutletFromContext(r.Context()); oc != nil {
			id := oc.ID
			return &id
		}
		if s := httpware.GetOutletID(r.Context()); s != "" {
			if id, err := uuid.Parse(s); err == nil {
				return &id
			}
		}
		return nil
	}
	p := strings.TrimSpace(r.URL.Query().Get("outlet_id"))
	if p == "" {
		return contextOutlet()
	}
	if strings.EqualFold(p, "all") {
		if CanAccessAllOutlets(r) {
			return nil
		}
		return contextOutlet()
	}
	oid, err := uuid.Parse(p)
	if err != nil {
		return contextOutlet()
	}
	if acc, ok := r.Context().Value(outletAccessKey{}).(outletAccess); ok {
		if CanViewOutlet(r, acc.client, acc.tenantID, oid) {
			return &oid
		}
	} else if CanAccessAllOutlets(r) {
		return &oid
	}
	return contextOutlet()
}

func outletToCtx(o *ent.Outlet) *OutletContext {
	useCase := ""
	if o.UseCase != nil {
		useCase = *o.UseCase
	}
	return &OutletContext{
		ID:      o.ID,
		Code:    o.Code,
		Name:    o.Name,
		UseCase: useCase,
		IsHQ:    o.IsHq,
		Status:  o.Status,
	}
}

// claimsSeeAllOutlets is the HQ rule: platform owner, admin, HQ session or a manager-level role.
func claimsSeeAllOutlets(claims *authclient.Claims) bool {
	if claims.OutletRestricted() {
		return false // an assigned list outranks the manager level
	}
	return claims.CanAccessAllOutlets() || posrole.IsAdminLevelAny(claims.Roles)
}

// CanAccessAllOutlets reports whether the caller may read across every outlet (?outlet_id=all),
// by the same rule OutletContextMiddleware applies to the X-Outlet-ID header.
func CanAccessAllOutlets(r *http.Request) bool {
	claims, ok := authclient.ClaimsFromContext(r.Context())
	return ok && claims != nil && claimsSeeAllOutlets(claims)
}

// CanViewOutlet reports whether the caller may scope a read to outletID through a query param.
// It mirrors the header check: HQ callers may read any outlet of their tenant, everyone else their
// resolved outlet, an outlet they are assigned to, or the HQ outlet.
func CanViewOutlet(r *http.Request, client *ent.Client, tenantID, outletID uuid.UUID) bool {
	if CanAccessAllOutlets(r) {
		return true
	}
	if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil && claims.OutletRestricted() {
		return claims.AllowsOutlet(outletID.String())
	}
	if oc := OutletFromContext(r.Context()); oc != nil && oc.ID == outletID {
		return true
	}
	ctx := r.Context()
	if claims, ok := authclient.ClaimsFromContext(ctx); ok && claims != nil && claims.Subject != "" {
		if userID, err := uuid.Parse(claims.Subject); err == nil && isStaffAssignedToOutlet(ctx, client, tenantID, userID, outletID) {
			return true
		}
	}
	isHQ, _ := client.Outlet.Query().
		Where(entoutlet.ID(outletID), entoutlet.TenantID(tenantID), entoutlet.IsHq(true)).
		Exist(ctx)
	return isHQ
}

func isStaffAssignedToOutlet(ctx context.Context, client *ent.Client, tenantID, userID, outletID uuid.UUID) bool {
	ok, _ := client.StaffMember.Query().
		Where(
			entstaff.TenantID(tenantID),
			entstaff.UserID(userID),
			entstaff.HasOutletsWith(entstaffoutlet.OutletID(outletID)),
		).Exist(ctx)
	return ok
}
