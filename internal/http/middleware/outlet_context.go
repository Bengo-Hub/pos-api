package middleware

import (
	"context"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"
	"github.com/bengobox/pos-service/internal/ent"
	entoutlet "github.com/bengobox/pos-service/internal/ent/outlet"
	entstaff "github.com/bengobox/pos-service/internal/ent/staffmember"
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
//  1. X-Outlet-ID header  — explicit override (HQ user / frontend-selected outlet)
//  2. StaffMember.outlet_id — for terminal sessions (PIN login)
//  3. Tenant's HQ outlet   — fallback when no outlet can be determined
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
			isHQUser := hasClaims && claims.CanAccessAllOutlets()

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
				// Non-HQ users can only request their assigned outlet.
				if !isHQUser && !o.IsHq {
					if hasClaims && claims.Subject != "" {
						if userID, err := uuid.Parse(claims.Subject); err == nil {
							if !isStaffAssignedToOutlet(ctx, client, tenantID, userID, o.ID) {
								http.Error(w, `{"error":"outlet not assigned to this user"}`, http.StatusForbidden)
								return
							}
						}
					}
				}
				resolved = outletToCtx(o)
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
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
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

func isStaffAssignedToOutlet(ctx context.Context, client *ent.Client, tenantID, userID, outletID uuid.UUID) bool {
	ok, _ := client.StaffMember.Query().
		Where(
			entstaff.TenantID(tenantID),
			entstaff.UserID(userID),
			entstaff.OutletID(outletID),
		).Exist(ctx)
	return ok
}
