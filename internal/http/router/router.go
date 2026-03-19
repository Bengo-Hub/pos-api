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

func New(log *zap.Logger, health *handlers.HealthHandler, authMiddleware *authclient.AuthMiddleware, idSvc *identity.Service, orders *handlers.POSOrderHandler) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(httpware.RequestID)
	r.Use(httpware.Logging(log))
	r.Use(httpware.Recover(log))
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Authorization", "Content-Type", "X-Tenant-ID", "X-Request-ID"},
		ExposedHeaders:   []string{"Link"},
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
			// Layer 2: Subscription enforcement — reject expired/cancelled tenants
			api.Use(authclient.RequireActiveSubscription())
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
								"email": claims.Email,
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

			tenant.Route("/pos", func(pos chi.Router) {
				if orders != nil {
					pos.Get("/orders", orders.ListOrders)
					pos.Post("/orders", orders.CreateOrder)
					pos.Get("/orders/{orderID}", orders.GetOrder)
					pos.Patch("/orders/{orderID}/status", orders.UpdateStatus)
				}
			})
		})
	})

	return r
}

