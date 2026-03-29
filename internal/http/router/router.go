package router

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"go.uber.org/zap"

	handlers "github.com/bengobox/pos-service/internal/http/handlers"
	"github.com/bengobox/pos-service/internal/modules/identity"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"
)

func New(
	log *zap.Logger,
	health *handlers.HealthHandler,
	authMiddleware *authclient.AuthMiddleware,
	idSvc *identity.Service,
	orders *handlers.POSOrderHandler,
	catalog *handlers.CatalogHandler,
	tables *handlers.TableHandler,
	tenders *handlers.TenderHandler,
	payments *handlers.PaymentHandler,
	drawers *handlers.DrawerHandler,
	barTabs *handlers.BarTabHandler,
	promotions *handlers.PromotionHandler,
	rbacHandler *handlers.RBACHandler,
	allowedOrigins []string,
) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(httpware.RequestID)
	r.Use(httpware.Logging(log))
	r.Use(httpware.Recover(log))
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "Origin", "X-Request-ID", "X-Tenant-ID", "X-Tenant-Slug"},
		ExposedHeaders:   []string{"Link", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Get("/healthz", health.Liveness)
	r.Get("/readyz", health.Readiness)
	r.Get("/metrics", health.Metrics)
	r.Get("/v1/docs/*", handlers.SwaggerUI)

	r.Route("/api/v1", func(api chi.Router) {
		// Apply auth middleware to all v1 routes
		if authMiddleware != nil {
			api.Use(authMiddleware.RequireAuth)
			// Layer 2: Subscription enforcement — mutations only (GET/HEAD/OPTIONS pass through)
			api.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
						next.ServeHTTP(w, r)
						return
					}
					claims, ok := authclient.ClaimsFromContext(r.Context())
					if !ok {
						next.ServeHTTP(w, r)
						return
					}
					if claims.IsSuperuser() || claims.IsPlatformOwner || claims.IsSubscriptionActive() {
						next.ServeHTTP(w, r)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusForbidden)
					_, _ = w.Write([]byte(`{"error":"Your subscription is not active. Please renew to continue.","code":"subscription_inactive","upgrade":true}`))
				})
			})
		}

		if idSvc != nil {
			api.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					claims, ok := authclient.ClaimsFromContext(r.Context())
					if ok && claims.Subject != "" {
						subject, _ := uuid.Parse(claims.Subject)
						slug := claims.GetTenantSlug()
						if slug != "" {
							_, err := idSvc.EnsureUserFromToken(r.Context(), subject, slug, map[string]any{
								"email":             claims.Email,
								"roles":             claims.Roles,
								"permissions":       claims.Permissions,
								"is_platform_owner": claims.IsPlatformOwner,
							})
							if err != nil {
								log.Warn("jit provisioning failed", zap.Error(err))
							}
						}
					}
					next.ServeHTTP(w, r)
				})
			})
		}

		// Serve OpenAPI spec (public, no auth required)
		api.Get("/openapi.json", handlers.OpenAPIJSON)

		api.Route("/{tenantID}", func(tenant chi.Router) {
			tenant.Use(httpware.TenantV2(httpware.TenantConfig{
				ClaimsExtractor: func(ctx context.Context) (tenantID, tenantSlug string, isPlatformOwner bool, ok bool) {
					claims, found := authclient.ClaimsFromContext(ctx)
					if !found {
						return "", "", false, false
					}
					return claims.TenantID, claims.GetTenantSlug(), claims.IsPlatformOwner, true
				},
				URLParamFunc: chi.URLParam,
				Required:     true,
			}))

			// RBAC routes
			if rbacHandler != nil {
				rbacHandler.RegisterRoutes(tenant)
			}

			tenant.Route("/pos", func(pos chi.Router) {
				// Orders
				if orders != nil {
					pos.Get("/orders", orders.ListOrders)
					pos.Post("/orders", orders.CreateOrder)
					pos.Get("/orders/{orderID}", orders.GetOrder)
					pos.Patch("/orders/{orderID}/status", orders.UpdateStatus)
				}

				// Catalog
				if catalog != nil {
					pos.Route("/catalog", func(cat chi.Router) {
						cat.Get("/items", catalog.ListCatalogItems)
						cat.Post("/items", catalog.CreateCatalogItem)
						cat.Get("/items/{id}", catalog.GetCatalogItem)
						cat.Put("/items/{id}", catalog.UpdateCatalogItem)
						cat.Delete("/items/{id}", catalog.DeleteCatalogItem)
					})
				}

				// Sections & Tables
				if tables != nil {
					pos.Get("/sections", tables.ListSections)
					pos.Post("/sections", tables.CreateSection)
					pos.Put("/sections/{id}", tables.UpdateSection)
					pos.Get("/tables", tables.ListTables)
					pos.Post("/tables", tables.CreateTable)
					pos.Put("/tables/{id}", tables.UpdateTable)
					pos.Patch("/tables/{id}/status", tables.UpdateTableStatus)
					pos.Post("/tables/{id}/assign", tables.AssignTable)
					pos.Post("/tables/{id}/release", tables.ReleaseTable)
				}

				// Tenders
				if tenders != nil {
					pos.Get("/tenders", tenders.ListTenders)
					pos.Post("/tenders", tenders.CreateTender)
					pos.Put("/tenders/{id}", tenders.UpdateTender)
				}

				// Payments
				if payments != nil {
					pos.Post("/orders/{orderID}/payments", payments.RecordPayment)
					pos.Get("/orders/{orderID}/payments", payments.ListOrderPayments)
				}

				// Cash Drawers
				if drawers != nil {
					pos.Post("/drawers/open", drawers.OpenDrawer)
					pos.Get("/drawers/current", drawers.GetCurrentDrawer)
					pos.Post("/drawers/{id}/close", drawers.CloseDrawer)
					pos.Get("/drawers", drawers.ListDrawerHistory)
				}

				// Bar Tabs
				if barTabs != nil {
					pos.Post("/bar-tabs", barTabs.OpenBarTab)
					pos.Get("/bar-tabs", barTabs.ListBarTabs)
					pos.Get("/bar-tabs/{id}", barTabs.GetBarTab)
					pos.Post("/bar-tabs/{id}/close", barTabs.CloseBarTab)
				}

				// Promotions
				if promotions != nil {
					pos.Get("/promotions", promotions.ListPromotions)
					pos.Post("/promotions", promotions.CreatePromotion)
					pos.Post("/promotions/apply", promotions.ApplyPromoCode)
				}
			})
		})
	})

	return r
}

