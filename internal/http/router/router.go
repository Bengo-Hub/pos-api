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

	"github.com/bengobox/pos-service/internal/ent"
	handlers "github.com/bengobox/pos-service/internal/http/handlers"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
	"github.com/bengobox/pos-service/internal/modules/identity"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/Bengo-Hub/httpware"
)

func New(
	log *zap.Logger,
	health *handlers.HealthHandler,
	authMiddleware *authclient.AuthMiddleware,
	entClient *ent.Client,
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
	hotel *handlers.HotelHandler,
	kds *handlers.KDSHandler,
	devices *handlers.DeviceHandler,
	pinAuth *handlers.PINAuthHandler,
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
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "Origin", "X-Request-ID", "X-Tenant-ID", "X-Tenant-Slug", "X-Outlet-ID"},
		ExposedHeaders:   []string{"Link", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

	r.Get("/healthz", health.Liveness)
	r.Get("/readyz", health.Readiness)
	r.Get("/metrics", health.Metrics)
	r.Get("/v1/docs/*", handlers.SwaggerUI)

	r.Route("/api/v1", func(api chi.Router) {
		// ── Public endpoints (no auth required) ───────────────────────────────
		// These routes are accessible before the staff member has authenticated.
		// TenantV2 extracts tenant UUID directly from the URL path parameter.
		api.Get("/openapi.json", handlers.OpenAPIJSON)

		if pinAuth != nil {
			api.Group(func(pub chi.Router) {
				pub.Use(httpware.TenantV2(httpware.TenantConfig{
					URLParamFunc: chi.URLParam,
					URLParamName: "tenantID",
					Required:     true,
				}))
				pub.Get("/{tenantID}/pos/staff", pinAuth.ListStaff)
				pub.Post("/{tenantID}/pos/auth/pin", pinAuth.Login)
				pub.Get("/{tenantID}/pos/auth/pin/profile", pinAuth.StaffProfiles)
			})
		}

		// ── Protected endpoints (auth required) ───────────────────────────────
		api.Group(func(prot chi.Router) {
			if authMiddleware != nil {
				prot.Use(authMiddleware.RequireAuth)
				prot.Use(subscriptions.SubscriptionGate())
			}

			if idSvc != nil {
				prot.Use(func(next http.Handler) http.Handler {
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

			prot.Route("/{tenantID}", func(tenant chi.Router) {
				tenant.Use(httpware.TenantV2(httpware.TenantConfig{
					ClaimsExtractor: func(ctx context.Context) (tenantID, tenantSlug string, isPlatformOwner bool, ok bool) {
						claims, found := authclient.ClaimsFromContext(ctx)
						if !found {
							return "", "", false, false
						}
						return claims.TenantID, claims.GetTenantSlug(), claims.IsPlatformOwner, true
					},
					URLParamFunc: chi.URLParam,
					URLParamName: "tenantID",
					Required:     true,
				}))
				tenant.Use(outletmw.OutletContextMiddleware(entClient, log))

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

					// Sections & Tables — requires table_management feature
					if tables != nil {
						pos.Group(func(tbl chi.Router) {
							tbl.Use(subscriptions.RequireFeature("table_management"))
							tbl.Get("/sections", tables.ListSections)
							tbl.Post("/sections", tables.CreateSection)
							tbl.Put("/sections/{id}", tables.UpdateSection)
							tbl.Get("/tables", tables.ListTables)
							tbl.Post("/tables", tables.CreateTable)
							tbl.Put("/tables/{id}", tables.UpdateTable)
							tbl.Patch("/tables/{id}/status", tables.UpdateTableStatus)
							tbl.Post("/tables/{id}/assign", tables.AssignTable)
							tbl.Post("/tables/{id}/release", tables.ReleaseTable)
						})
					}

					// Tenders
					if tenders != nil {
						pos.Get("/tenders", tenders.ListTenders)
						pos.Post("/tenders", tenders.CreateTender)
						pos.Put("/tenders/{id}", tenders.UpdateTender)
					}

					// Payments
					if payments != nil {
						pos.Post("/orders/{orderID}/payments/intent", payments.CreatePaymentIntent)
						pos.Post("/orders/{orderID}/payments", payments.RecordPayment)
						pos.Get("/orders/{orderID}/payments", payments.ListOrderPayments)
						pos.Post("/payments/initiate", payments.ProxyInitiate)
					}

					// Cash Drawers — shift report history requires shift_reports feature
					if drawers != nil {
						pos.Post("/drawers/open", drawers.OpenDrawer)
						pos.Get("/drawers/current", drawers.GetCurrentDrawer)
						pos.Post("/drawers/{id}/close", drawers.CloseDrawer)
						pos.Group(func(dr chi.Router) {
							dr.Use(subscriptions.RequireFeature("shift_reports"))
							dr.Get("/drawers", drawers.ListDrawerHistory)
						})
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

					// Device sessions (shift open/close)
					if devices != nil {
						pos.Get("/devices/current/sessions/current", devices.GetCurrentSession)
						pos.Post("/devices/current/sessions/open", devices.OpenSession)
						pos.Post("/devices/current/sessions/close", devices.CloseSession)
					}

					// Terminal PIN auth (auth-protected endpoints)
					// SetPIN requires a manager SSO token; AuthMe requires SSO token for Trinity Layer 3.
					// ListStaff / Login / StaffProfiles are registered in the public group above.
					if pinAuth != nil {
						pos.Group(func(ca chi.Router) {
							ca.Use(subscriptions.RequireFeature("multi_cashier"))
							ca.Post("/auth/pin/set", pinAuth.SetPIN)
						})
						pos.Get("/auth/me", pinAuth.AuthMe)
					}

					// KDS
					if kds != nil {
						pos.Get("/kds/stations", kds.ListStations)
						pos.Post("/kds/stations", kds.CreateStation)
						pos.Put("/kds/stations/{id}", kds.UpdateStation)
						pos.Get("/kds/kitchen", kds.GetKitchenQueue)
						pos.Get("/kds/bar", kds.GetBarQueue)
						pos.Get("/kds/tickets", kds.ListTickets)
						pos.Post("/kds/tickets/{id}/start", kds.StartTicket)
						pos.Post("/kds/tickets/{id}/ready", kds.ReadyTicket)
						pos.Post("/kds/tickets/{id}/serve", kds.ServeTicket)
						pos.Post("/kds/tickets/{id}/void", kds.VoidTicket)
						pos.Post("/kds/tickets/{id}/call-waiter", kds.CallWaiter)
					}
				})

				// Hotel module
				if hotel != nil {
					tenant.Route("/hotel", func(h chi.Router) {
						h.Get("/rooms", hotel.ListRooms)
						h.Post("/rooms", hotel.CreateRoom)
						h.Get("/rooms/{id}", hotel.GetRoom)
						h.Patch("/rooms/{id}/status", hotel.UpdateRoomStatus)
						h.Post("/rooms/{id}/check-in", hotel.CheckIn)
						h.Post("/rooms/{id}/check-out", hotel.CheckOut)
						h.Post("/rooms/{id}/folio", hotel.PostFolioCharge)
						h.Get("/rooms/{id}/folio", hotel.GetRoomFolio)
						h.Get("/facilities", hotel.ListFacilities)
						h.Post("/facilities", hotel.CreateFacility)
						h.Get("/facilities/{id}", hotel.GetFacility)
						h.Post("/facilities/{id}/book", hotel.BookFacility)
						h.Patch("/facilities/bookings/{bookingID}", hotel.UpdateBooking)
						h.Get("/facilities/bookings", hotel.ListFacilityBookings)
					})
				}
			})
		})
	})

	return r
}

