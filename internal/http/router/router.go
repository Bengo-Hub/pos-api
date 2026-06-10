package router

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"go.uber.org/zap"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/ent"
	handlers "github.com/bengobox/pos-service/internal/http/handlers"
	outletmw "github.com/bengobox/pos-service/internal/http/middleware"
	"github.com/bengobox/pos-service/internal/modules/identity"
	rbacmodule "github.com/bengobox/pos-service/internal/modules/rbac"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
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
	rbacSvc *rbacmodule.Service,
	hotel *handlers.HotelHandler,
	kds *handlers.KDSHandler,
	devices *handlers.DeviceHandler,
	pinAuth *handlers.PINAuthHandler,
	publicOutlet *handlers.PublicOutletHandler,
	closings *handlers.DailyClosingHandler,
	returns *handlers.ReturnHandler,
	receipt *handlers.ReceiptHandler,
	menu *handlers.MenuHandler,
	layaway *handlers.LayawayHandler,
	scale *handlers.ScaleHandler,
	pharmacy *handlers.PharmacyHandler,
	appointments *handlers.AppointmentHandler,
	commissions *handlers.CommissionHandler,
	staffSchedule *handlers.StaffScheduleHandler,
	shiftOverrides *handlers.StaffShiftOverrideHandler,
	leaveRequests *handlers.LeaveRequestHandler,
	shiftRotations *handlers.ShiftRotationHandler,
	loyalty *handlers.LoyaltyHandler,
	reports *handlers.ReportsHandler,
	webhooks *handlers.WebhookHandler,
	onlineOrders *handlers.OnlineOrderHandler,
	serviceConfig *handlers.ServiceConfigHandler,
	serviceSettings *handlers.ServiceSettingsHandler,
	notifications *handlers.NotificationsHandler,
	queue *handlers.QueueHandler,
	billSplits *handlers.BillSplitHandler,
	resources *handlers.ResourceHandler,
	commissionRules *handlers.CommissionRuleHandler,
	packages *handlers.PackageHandler,
	clients *handlers.ClientHandler,
	channels *handlers.ChannelHandler,
	print *handlers.PrintHandler,
	payroll *handlers.PayrollHandler,
	staffAdmin *handlers.StaffHandler,
	purchaseOrders *handlers.PurchaseOrdersHandler,
	repairs *handlers.RepairHandler,
	allowedOrigins []string,
	redisClient *redis.Client,
	internalServiceKey string,
) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(httpware.RequestID)
	r.Use(httpware.Logging(log))
	r.Use(httpware.Recover(log))
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(middleware.RequestSize(10 << 20)) // 10 MB max body size
	r.Use(outletmw.IPRateLimit(redisClient, outletmw.DefaultRateLimitConfig()))
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
		// Ã¢â€â‚¬Ã¢â€â‚¬ Platform admin endpoints (platform owner JWT required) Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬
		if serviceConfig != nil && authMiddleware != nil {
			api.Group(func(admin chi.Router) {
				admin.Use(authMiddleware.RequireAuth)
				serviceConfig.RegisterAdminRoutes(admin)
			})
		}

		// Ã¢â€â‚¬Ã¢â€â‚¬ Public endpoints (no auth required) Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬
		// These routes are accessible before the staff member has authenticated.
		// TenantV2 extracts tenant UUID directly from the URL path parameter.
		api.Get("/openapi.json", handlers.OpenAPIJSON)

		api.Group(func(pub chi.Router) {
			pub.Use(httpware.TenantV2(httpware.TenantConfig{
				URLParamFunc: chi.URLParam,
				URLParamName: "tenantID",
				Required:     true,
			}))
			if pinAuth != nil {
				pub.Get("/{tenantID}/pos/staff", pinAuth.ListStaff)
				pub.Post("/{tenantID}/pos/auth/pin", pinAuth.Login)
				pub.Post("/{tenantID}/pos/auth/pin/identify", pinAuth.IdentifyByPIN)
				pub.Get("/{tenantID}/pos/auth/pin/profile", pinAuth.StaffProfiles)
			}
			if publicOutlet != nil {
				pub.Get("/{tenantID}/pos/outlets", publicOutlet.ListPublicOutlets)
				pub.Get("/{tenantID}/pos/outlets/current", publicOutlet.GetCurrentOutlet)
			}
			// Branded printable customer menu document (tokenless so the QR code target opens
			// in any browser). Regenerated on every request → always reflects the live catalog.
			if menu != nil {
				pub.Get("/{tenantID}/pos/outlets/{outletID}/menu.html", menu.GetMenuHTML)
				// True-PDF variant (same tokenless data path) for DocPreview + sharing.
				pub.Get("/{tenantID}/pos/outlets/{outletID}/menu.pdf", menu.GetMenuPDF)
			}
			// Public reservation endpoints Ã¢â‚¬â€ used by the embeddable booking widget
			if tables != nil {
				pub.Get("/{tenantID}/pos/reservations/available", tables.GetAvailableSlots)
				pub.Post("/{tenantID}/pos/reservations", tables.CreateReservation)
			}
		})

		// Ã¢â€â‚¬Ã¢â€â‚¬ Protected endpoints (auth required) Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬
		// RequireAnyAuth accepts both SSO JWTs and HMAC terminal JWTs from PIN login.
		api.Group(func(prot chi.Router) {
			if pinAuth != nil {
				prot.Use(pinAuth.RequireAnyAuth(authMiddleware))
			} else if authMiddleware != nil {
				prot.Use(authMiddleware.RequireAuth)
			}
			if authMiddleware != nil {
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
									"outlet_id":         claims.GetOutletID(),
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

				// Outlet settings + TruLoad-inspired outlet switch
				if serviceSettings != nil {
					serviceSettings.RegisterRoutes(tenant)
				}

				tenant.Route("/pos", func(pos chi.Router) {
					// Orders
					if orders != nil {
						pos.Get("/orders", orders.ListOrders)
						pos.Post("/orders", orders.CreateOrder)
						pos.Get("/orders/by-number/{orderNumber}", orders.GetOrderByNumber)
						pos.Get("/orders/{orderID}", orders.GetOrder)
						pos.Patch("/orders/{orderID}/status", orders.UpdateStatus)
						pos.Patch("/orders/{orderID}/void", orders.VoidOrder)
						pos.Post("/orders/{orderID}/fire-course", orders.FireCourse)
						pos.Post("/orders/{orderID}/lines", orders.AddOrderLines)
						pos.Post("/orders/{orderID}/lines/{lineID}/serials", orders.CaptureSerial)
					}
					if print != nil {
						pos.Post("/orders/{orderID}/print", print.PrintReceipt)
					}

					// In-app notifications (waiter order-ready alerts + real-time WS stream)
					if notifications != nil {
						pos.Get("/notifications", notifications.List)
						pos.Get("/notifications/stream", notifications.StreamNotifications)
						pos.Post("/notifications/mark-all-read", notifications.MarkAllRead)
						pos.Patch("/notifications/{id}/read", notifications.MarkRead)
					}

					// Receipt
					if receipt != nil {
						pos.Get("/orders/{orderID}/receipt", receipt.GetReceipt)
						pos.Get("/orders/{orderID}/receipt/html", receipt.GetReceiptHTML)
						pos.Get("/orders/{orderID}/receipt/pdf", receipt.GetReceiptPDF)
					}

					// QZ Tray printing bridge — serve the platform cert + sign print requests so the
					// pos-ui can print silently to assigned printers. Stateless (env-driven key/cert).
					pos.Get("/printing/qz/cert", handlers.QZCert)
					pos.Post("/printing/qz/sign", handlers.QZSign)

					// Catalog
					if catalog != nil {
						pos.Route("/catalog", func(cat chi.Router) {
							cat.Get("/categories", catalog.GetCatalogCategories)
							cat.Get("/items", catalog.ListCatalogItems)
							cat.Get("/pricing/resolve", catalog.ResolvePrice)
							cat.Get("/pricing/tiers", catalog.GetPricingTiers)
							cat.Post("/items", catalog.CreateCatalogItem)
							// Price management endpoints (must come before /{id} routes)
							cat.Patch("/items/prices", catalog.SetCatalogItemPrice)
							cat.Post("/items/prices/bulk", catalog.BulkSetCatalogPrices)
							cat.Get("/items/{id}", catalog.GetCatalogItem)
							cat.Put("/items/{id}", catalog.UpdateCatalogItem)
							cat.Delete("/items/{id}", catalog.DeleteCatalogItem)
							cat.Get("/items/{id}/stock", catalog.GetItemStock)
							cat.Get("/barcode/{barcode}", catalog.BarcodeLookup)
						})
					}

					// Sections & Tables Ã¢â‚¬â€ hospitality only
					if tables != nil {
						pos.Group(func(tbl chi.Router) {
							tbl.Use(outletmw.RequireUseCase("hospitality"))
							tbl.Use(subscriptions.RequireFeature(subscriptions.FeatureTableManagement))
							tbl.Get("/sections", tables.ListSections)
							tbl.Post("/sections", tables.CreateSection)
							tbl.Put("/sections/{id}", tables.UpdateSection)
							tbl.Delete("/sections/{id}", tables.DeleteSection)
							tbl.Get("/tables", tables.ListTables)
							tbl.Post("/tables", tables.CreateTable)
							tbl.Put("/tables/{id}", tables.UpdateTable)
							tbl.Delete("/tables/{id}", tables.DeleteTable)
							tbl.Patch("/tables/{id}/status", tables.UpdateTableStatus)
							tbl.Post("/tables/{id}/assign", tables.AssignTable)
							tbl.Post("/tables/{id}/release", tables.ReleaseTable)
							tbl.Post("/tables/{id}/transfer", tables.TransferTable)
							tbl.Post("/tables/merge", tables.MergeTables)
							tbl.Post("/tables/unmerge", tables.UnmergeTables)
							// Order split + service charge live here (use TableHandler, need nil guard)
							tbl.Post("/orders/{orderID}/split", tables.SplitOrder)
							tbl.Patch("/orders/{orderID}/service-charge", tables.SetServiceCharge)
							// Reservations (staff-managed)
							tbl.Get("/reservations", tables.ListReservations)
							tbl.Get("/reservations/available", tables.GetAvailableSlots)
							tbl.Get("/reservations/{id}", tables.GetReservation)
							tbl.Post("/reservations", tables.CreateReservation)
							tbl.Patch("/reservations/{id}", tables.UpdateReservation)
							tbl.Post("/reservations/{id}/confirm", tables.ConfirmReservation)
							tbl.Post("/reservations/{id}/check-in", tables.CheckInReservation)
							tbl.Post("/reservations/{id}/cancel", tables.CancelReservation)
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
						pos.Get("/gateways", payments.GetGateways)
						pos.Post("/expenses", payments.RecordExpense)
						pos.Get("/c2b/payments", payments.ListC2BCandidates)
						pos.Post("/c2b/payments/{transID}/claim", payments.ClaimC2BPayment)
						// Recording a payment (cash/M-Pesa ref) or opening a payment intent is a
						// money-movement action Ã¢â‚¬â€ gate on payments.add (cashier, waiter, manager+).
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.payments.add", "pos.payments.manage")).
							Post("/orders/{orderID}/payments/intent", payments.CreatePaymentIntent)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.payments.add", "pos.payments.manage")).
							Post("/orders/{orderID}/payments", payments.RecordPayment)
						pos.Get("/orders/{orderID}/payments", payments.ListOrderPayments)
						pos.Get("/orders/{orderID}/payment-status/stream", payments.StreamPaymentStatus)
						pos.Post("/payments/initiate", payments.ProxyInitiate)
						// "Save as Quotation" forwards a pos cart to treasury (treasury owns quotations).
						pos.Post("/quotations", payments.CreateQuotationFromCart)
					}

					// Cash Drawers
					if drawers != nil {
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.cash_drawers.add", "pos.cash_drawers.manage")).
							Post("/drawers/open", drawers.OpenDrawer)
						pos.Get("/drawers/current", drawers.GetCurrentDrawer)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.cash_drawers.manage")).
							Post("/drawers/{id}/close", drawers.CloseDrawer)
						pos.Get("/drawers", drawers.ListDrawerHistory)
					}

					// Bar Tabs Ã¢â‚¬â€ hospitality only
					if barTabs != nil {
						pos.Group(func(bt chi.Router) {
							bt.Use(outletmw.RequireUseCase("hospitality"))
							bt.Post("/bar-tabs", barTabs.OpenBarTab)
							bt.Get("/bar-tabs", barTabs.ListBarTabs)
							bt.Get("/bar-tabs/{id}", barTabs.GetBarTab)
							bt.Post("/bar-tabs/{id}/close", barTabs.CloseBarTab)
						})
					}

					// Promotions
					if promotions != nil {
						pos.Get("/promotions", promotions.ListPromotions)
						// Creating a promotion/happy-hour is administrative; applying a code at
						// checkout is part of the cashier order flow and stays ungated.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.promotions.add", "pos.promotions.manage")).
							Post("/promotions", promotions.CreatePromotion)
						pos.Post("/promotions/apply", promotions.ApplyPromoCode)
						pos.Get("/promotions/happy-hour/active", promotions.GetActiveHappyHours)
					}

					// Device sessions (shift open/close)
					if devices != nil {
						pos.Get("/devices", devices.ListDevices)
						pos.Get("/devices/current/sessions/current", devices.GetCurrentSession)
						pos.Get("/devices/current/sessions/current/summary", devices.GetSessionSummary)
						pos.Get("/devices/current/sessions/history", devices.GetSessionHistory)
						pos.Post("/devices/current/sessions/open", devices.OpenSession)
						pos.Post("/devices/current/sessions/close", devices.CloseSession)
					}

					// Terminal PIN auth (auth-protected endpoints)
					// SetPIN requires a manager/admin SSO token Ã¢â‚¬â€ no subscription gate so admins
					// can always set staff PINs regardless of plan.
					// AuthMe requires SSO token for Trinity Layer 3.
					// ListStaff / Login / StaffProfiles are registered in the public group above.
					if pinAuth != nil {
						pos.Post("/auth/pin/set", pinAuth.SetPIN)
						pos.Get("/auth/me", pinAuth.AuthMe)
					}

					// Staff admin CRUD (requires STAFF_MANAGE permission Ã¢â‚¬â€ enforced client-side;
					// server-side role boundary enforced in the handler itself).
					if staffAdmin != nil {
						pos.Get("/staff/admin", staffAdmin.ListStaffForAdmin)
						pos.Post("/staff", staffAdmin.CreateStaff)
						pos.Patch("/staff/{staffID}", staffAdmin.UpdateStaff)
						pos.Post("/staff/{staffID}/deactivate", staffAdmin.DeactivateStaff)
					}

					// KDS Ã¢â‚¬â€ hospitality and quick_service only; outlet must have enable_kds=true
					if kds != nil {
						pos.Group(func(k chi.Router) {
							k.Use(outletmw.RequireUseCase("hospitality", "quick_service"))
							k.Use(outletmw.RequireKDSEnabled(entClient))
							k.Use(subscriptions.RequireFeature(subscriptions.FeatureKDS))
							k.Get("/kds/stations", kds.ListStations)
							k.Post("/kds/stations", kds.CreateStation)
							k.Put("/kds/stations/{id}", kds.UpdateStation)
							k.Delete("/kds/stations/{id}", kds.DeleteStation)
							k.Get("/kds/stream", kds.StreamKDS)
							k.Get("/kds/kitchen", kds.GetKitchenQueue)
							k.Get("/kds/bar", kds.GetBarQueue)
							k.Get("/kds/tickets", kds.ListTickets)
							k.Post("/kds/tickets/{id}/start", kds.StartTicket)
							k.Post("/kds/tickets/{id}/ready", kds.ReadyTicket)
							k.Post("/kds/tickets/{id}/serve", kds.ServeTicket)
							k.Post("/kds/tickets/{id}/void", kds.VoidTicket)
							k.Post("/kds/tickets/{id}/call-waiter", kds.CallWaiter)
						})
					}

					// Returns
					if returns != nil {
						pos.Post("/orders/{orderID}/returns", returns.CreateReturn)
						// Bill splitting
						if billSplits != nil {
							pos.Get("/orders/{orderID}/splits", billSplits.ListSplits)
							pos.Post("/orders/{orderID}/splits", billSplits.CreateSplits)
							// Settling a split records a payment Ã¢â‚¬â€ gate on payments.add like other tender flows.
							pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.payments.add", "pos.payments.manage")).
								Post("/orders/{orderID}/splits/{splitID}/settle", billSplits.SettleSplit)
						}
						pos.Get("/returns", returns.ListReturns)
						pos.Get("/returns/{returnID}", returns.GetReturn)
						pos.Patch("/returns/{returnID}/approve", returns.ApproveReturn)
					}

					// Layaway plans & payments
					if layaway != nil {
						pos.Post("/layaways", layaway.Create)
						pos.Get("/layaways", layaway.List)
						pos.Get("/layaways/{id}", layaway.Get)
						pos.Post("/layaways/{id}/payments", layaway.RecordPayment)
						pos.Post("/layaways/{id}/cancel", layaway.Cancel)
						pos.Post("/layaways/{id}/complete", layaway.Complete)
					}

					// Purchase orders proxy (inventory-api pass-through with tenant auth)
					if purchaseOrders != nil {
						pos.Get("/purchase-orders", purchaseOrders.List)
						pos.Post("/purchase-orders", purchaseOrders.Create)
						pos.Get("/purchase-orders/{id}", purchaseOrders.Get)
					}

					// Weighing scale readings
					if scale != nil {
						pos.Post("/scale/readings", scale.Create)
						pos.Get("/scale/readings", scale.List)
					}

					// Pharmacy Ã¢â‚¬â€ pharmacy use_case only
					if pharmacy != nil {
						pos.Group(func(ph chi.Router) {
							ph.Use(outletmw.RequireUseCase("pharmacy"))
							ph.Post("/pharmacy/prescriptions", pharmacy.CreatePrescription)
							ph.Get("/pharmacy/prescriptions", pharmacy.ListPrescriptions)
							ph.Get("/pharmacy/prescriptions/{prescriptionID}", pharmacy.GetPrescription)
							ph.Post("/pharmacy/prescriptions/{prescriptionID}/dispense", pharmacy.Dispense)
							ph.Post("/pharmacy/interaction-checks", pharmacy.CreateInteractionCheck)
							ph.Post("/pharmacy/age-verify", pharmacy.AgeVerify)
							ph.Get("/pharmacy/patients", pharmacy.ListPatients)
							ph.Get("/pharmacy/controlled-substances", pharmacy.ListControlledLogs)
							ph.Post("/pharmacy/controlled-substances", pharmacy.CreateControlledLog)
							ph.Get("/pharmacy/controlled-substances/{logID}", pharmacy.GetControlledLog)
						})
					}

					// Appointments & staff schedules Ã¢â‚¬â€ services use_case
					if appointments != nil {
						pos.Group(func(svc chi.Router) {
							svc.Use(outletmw.RequireUseCase("services"))
							apptChange := outletmw.RequireServicePermission(rbacSvc, "pos.appointments.change", "pos.appointments.manage")
							svc.Get("/appointments", appointments.List)
							svc.With(outletmw.RequireServicePermission(rbacSvc, "pos.appointments.add", "pos.appointments.manage")).
								Post("/appointments", appointments.Create)
							svc.Get("/appointments/availability", appointments.Availability)
							svc.Get("/appointments/{appointmentID}", appointments.Get)
							svc.With(apptChange).Put("/appointments/{appointmentID}", appointments.Update)
							svc.With(apptChange).Post("/appointments/{appointmentID}/check-in", appointments.CheckIn)
							svc.With(apptChange).Post("/appointments/{appointmentID}/start", appointments.Start)
							svc.With(apptChange).Post("/appointments/{appointmentID}/complete", appointments.Complete)
							svc.With(apptChange).Post("/appointments/{appointmentID}/cancel", appointments.Cancel)
							svc.With(apptChange).Post("/appointments/{appointmentID}/no-show", appointments.NoShow)
						})
					}

					// Walk-in queue Ã¢â‚¬â€ services use_case
					if queue != nil {
						pos.Group(func(svc chi.Router) {
							svc.Use(outletmw.RequireUseCase("services"))
							svc.Get("/queue", queue.List)
							svc.Post("/queue/entries", queue.Create)
							svc.Patch("/queue/entries/{entryID}/status", queue.UpdateStatus)
							svc.Post("/queue/entries/{entryID}/assign", queue.AssignStaff)
						})
					}

					// Resources Ã¢â‚¬â€ services use_case (chairs, rooms, equipment)
					if resources != nil {
						pos.Group(func(svc chi.Router) {
							svc.Use(outletmw.RequireUseCase("services"))
							svc.Get("/resources", resources.List)
							svc.Post("/resources", resources.Create)
							svc.Patch("/resources/{resourceID}", resources.PatchStatus)
						})
					}

					// Staff schedules + overrides + leave
					if staffSchedule != nil {
						pos.Get("/staff/{staffID}/schedule", staffSchedule.ListSchedule)
						pos.Put("/staff/{staffID}/schedule", staffSchedule.UpsertSchedule)
					}
					if shiftOverrides != nil {
						pos.Get("/staff/overrides", shiftOverrides.ListAllOverrides)
						pos.Get("/staff/{staffID}/overrides", shiftOverrides.ListStaffOverrides)
						pos.Post("/staff/{staffID}/overrides", shiftOverrides.CreateOverride)
						pos.Delete("/staff/{staffID}/overrides/{overrideID}", shiftOverrides.DeleteOverride)
					}
					if leaveRequests != nil {
						pos.Get("/leave-requests", leaveRequests.ListLeaveRequests)
						pos.Get("/staff/{staffID}/leave-requests", leaveRequests.ListStaffLeaveRequests)
						pos.Post("/staff/{staffID}/leave-requests", leaveRequests.CreateLeaveRequest)
						pos.Patch("/leave-requests/{leaveID}/status", leaveRequests.UpdateLeaveStatus)
					}
					if shiftRotations != nil {
						pos.Get("/shift-rotations", shiftRotations.ListRotations)
						pos.Post("/shift-rotations", shiftRotations.CreateRotation)
						pos.Get("/shift-rotations/{rotationID}", shiftRotations.GetRotation)
						pos.Patch("/shift-rotations/{rotationID}", shiftRotations.UpdateRotation)
						pos.Put("/shift-rotations/{rotationID}/slots", shiftRotations.UpsertSlots)
					}

					// Payroll & advances
					if payroll != nil {
						pos.Post("/staff/{staffID}/advances", payroll.CreateAdvance)
						pos.Get("/staff/{staffID}/advances", payroll.ListAdvances)
						pos.Post("/payroll/generate", payroll.GeneratePayroll)
						pos.Get("/payroll/{payrollID}", payroll.GetPayroll)
						pos.Post("/payroll/{payrollID}/approve", payroll.ApprovePayroll)
						pos.Post("/payroll/{payrollID}/disburse", payroll.DisbursePayroll)
					}

					// Commissions (records)
					if commissions != nil {
						pos.Get("/commissions", commissions.List)
						pos.Get("/commissions/{commissionID}", commissions.Get)
					}

					// Commission rules & payout
					if commissionRules != nil {
						pos.Get("/commissions/rules", commissionRules.List)
						pos.Post("/commissions/rules", commissionRules.Create)
						pos.Patch("/commissions/rules/{ruleID}", commissionRules.Update)
						pos.Post("/commissions/payout", commissionRules.Payout)
					}

					// Service packages
					if packages != nil {
						pos.Group(func(svc chi.Router) {
							svc.Use(outletmw.RequireUseCase("services"))
							svc.Get("/packages", packages.ListPackages)
							svc.Post("/packages", packages.CreatePackage)
							svc.Post("/packages/{packageID}/sell", packages.SellPackage)
							svc.Get("/packages/purchases", packages.ListPurchases)
							svc.Post("/packages/purchases/{purchaseID}/redeem", packages.RedeemSession)
						})
					}

					// Client records
					if clients != nil {
						pos.Get("/clients", clients.List)
						pos.Post("/clients", clients.CreateOrUpsert)
						pos.Get("/clients/{clientID}", clients.Get)
						pos.Patch("/clients/{clientID}", clients.Update)
						pos.Get("/clients/{phone}/orders", clients.GetOrdersByPhone)
					}

					// Loyalty programs & accounts — gated on the loyalty_program feature
					// (bundles include it from Starter; POS-device plans do not).
					if loyalty != nil {
						pos.Group(func(ly chi.Router) {
							ly.Use(subscriptions.RequireFeature(subscriptions.FeatureLoyalty))
							ly.Get("/loyalty/programs", loyalty.ListPrograms)
							ly.Post("/loyalty/programs", loyalty.CreateProgram)
							ly.Put("/loyalty/programs/{programID}", loyalty.UpdateProgram)
							ly.Get("/loyalty/accounts", loyalty.ListAccounts)
							ly.Post("/loyalty/accounts", loyalty.CreateAccount)
							ly.Get("/loyalty/accounts/{accountID}", loyalty.GetAccount)
							ly.Post("/loyalty/accounts/{accountID}/earn", loyalty.Earn)
							ly.Post("/loyalty/accounts/{accountID}/redeem", loyalty.Redeem)
							ly.Post("/loyalty/accounts/{accountID}/redeem-to-order", loyalty.RedeemToOrder)
							ly.Post("/loyalty/accounts/{accountID}/referrals", loyalty.CreateReferral)
							ly.Get("/loyalty/accounts/{accountID}/referrals", loyalty.ListReferrals)
						})
					}

					// Reports & Analytics
					if reports != nil {
						pos.Get("/reports/summary", reports.GetSummary)
						pos.Get("/reports/sales-summary", reports.SalesSummary)
						pos.Get("/reports/refund-summary", reports.RefundSummary)
						pos.Get("/reports/daily-breakdown", reports.DailyBreakdown)
						pos.Get("/reports/top-items", reports.TopItems)
						pos.Get("/reports/sales-by-staff", reports.SalesByStaff)
						pos.Get("/reports/export", reports.ExportDailyReport)
						// Sprint 11: additional report endpoints
						pos.Get("/reports/shifts", reports.ShiftReportList)
						pos.Get("/reports/shifts/{sessionID}", reports.ShiftReport)
						pos.Get("/reports/commissions", reports.CommissionReport)
						pos.Get("/reports/tax", reports.TaxReport)
						pos.Get("/reports/sales/by-hour", reports.SalesByHour)
						pos.Get("/reports/sales/by-category", reports.SalesByCategory)
						pos.Get("/reports/stock-consumption", reports.StockConsumptionReport)
						pos.Get("/reports/returns", reports.ReturnsSummary)
						pos.Get("/reports/void-summary", reports.VoidSummary)
						pos.Get("/reports/product-mix", reports.ProductMix)
						pos.Get("/reports/most-profitable", reports.MostProfitableItems)
					}

					// Webhook subscriptions & delivery log (Sprint 12)
					if webhooks != nil {
						pos.Get("/webhooks", webhooks.List)
						pos.Post("/webhooks", webhooks.Create)
						pos.Put("/webhooks/{webhookID}", webhooks.Update)
						pos.Delete("/webhooks/{webhookID}", webhooks.Delete)
						pos.Get("/webhooks/{webhookID}/deliveries", webhooks.ListDeliveries)
					}

					// Delivery channel integrations (Uber Eats, Glovo, etc.) Ã¢â‚¬â€ Sprint 12
					if channels != nil {
						pos.Get("/channels", channels.ListChannels)
						pos.Post("/channels", channels.CreateChannel)
						pos.Put("/channels/{channelID}", channels.UpdateChannel)
						pos.Delete("/channels/{channelID}", channels.DeleteChannel)
						pos.Get("/channels/{channelID}/sync-jobs", channels.ListSyncJobs)
						pos.Post("/channels/{channelID}/sync-jobs", channels.TriggerSyncJob)
					}

					// Online ordering pickup status Ã¢â‚¬â€ KDS click-and-collect (Sprint 13)
					if onlineOrders != nil {
						onlineFeat := subscriptions.RequireFeature(subscriptions.FeatureOnlineOrdering)
						pos.Get("/online-orders/pickup", onlineOrders.ListPickup)
						// Pickup hand-off + delivery rider assignment mutate order state Ã¢â‚¬â€ gate on
						// orders.change (waiter, manager+). Reads (pickup/rider lists) stay open.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.change", "pos.orders.manage"), onlineFeat).
							Post("/online-orders/{orderID}/ready", onlineOrders.MarkReady)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.change", "pos.orders.manage"), onlineFeat).
							Post("/online-orders/{orderID}/collected", onlineOrders.MarkCollected)
						// WS-D delivery rider assignment: list fleet (proxy logistics) +
						// assign rider (delegate to ordering-backend, which owns the order).
						pos.Get("/online-orders/riders", onlineOrders.ListAvailableRiders)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.change", "pos.orders.manage"), onlineFeat).
							Post("/online-orders/{orderID}/assign-rider", onlineOrders.AssignRider)
					}

					// Daily closings (ERP reconciliation)
					if closings != nil {
						pos.Post("/outlets/{outletID}/daily-close", closings.CloseDay)
						pos.Get("/outlets/{outletID}/daily-closings", closings.ListDailyClosings)
					}

					// Repair / job-card module. Reads open to authenticated staff; mutations gated
					// on pos.retail.add / pos.retail.manage (a retail/service-shop workflow).
					if repairs != nil {
						repairWrite := outletmw.RequireServicePermission(rbacSvc, "pos.retail.add", "pos.retail.manage")
						pos.Get("/repairs", repairs.List)
						pos.With(repairWrite).Post("/repairs", repairs.Create)
						pos.Get("/repairs/{id}", repairs.Get)
						pos.With(repairWrite).Patch("/repairs/{id}", repairs.Update)
						pos.With(repairWrite).Post("/repairs/{id}/parts", repairs.AddPart)
						pos.With(repairWrite).Delete("/repairs/{id}/parts/{partID}", repairs.RemovePart)
						pos.With(repairWrite).Post("/repairs/{id}/settle", repairs.Settle)
					}
				})

				// Hotel module Ã¢â‚¬â€ hospitality only
				if hotel != nil {
					tenant.Route("/hotel", func(h chi.Router) {
						h.Use(outletmw.RequireUseCase("hospitality"))
						// Entire hotel vertical requires the hotel_module feature. Conference/event
						// routes additionally require conference_events (a tenant can have hotel
						// without conferences, e.g. Complete Professional).
						h.Use(subscriptions.RequireFeature(subscriptions.FeatureHotelModule))
						conferenceFeat := subscriptions.RequireFeature(subscriptions.FeatureConference)
						// Front-desk operational actions (check-in/out, folio, bookings, room status,
						// facility booking, amenities, housekeeping) require hotel CHANGE; admin master
						// data (create/edit/delete rooms & facilities) requires hotel MANAGE.
						hotelChange := outletmw.RequireServicePermission(rbacSvc, "pos.hotel.change", "pos.hotel.manage")
						hotelManage := outletmw.RequireServicePermission(rbacSvc, "pos.hotel.manage")
						h.Get("/rooms", hotel.ListRooms)
						h.With(hotelManage).Post("/rooms", hotel.CreateRoom)
						h.Get("/rooms/{id}", hotel.GetRoom)
						h.With(hotelChange).Patch("/rooms/{id}/status", hotel.UpdateRoomStatus)
						// Inventory master pickers (link rooms/facilities/amenities to inventory SERVICE items + packages)
						h.Get("/inventory-service-items", hotel.ListInventoryServiceItems)
						h.Get("/inventory-bundles", hotel.ListInventoryBundles)
						// Multi-room / group bookings (RoomBooking header Ã¢â€ â€™ many RoomGuest)
						h.With(hotelChange).Post("/bookings", hotel.CreateRoomBooking)
						h.Get("/bookings", hotel.ListRoomBookings)
						h.Get("/bookings/{id}", hotel.GetRoomBooking)
						h.With(hotelManage).Patch("/bookings/{id}", hotel.UpdateRoomBooking)
						h.Get("/bookings/{id}/guests", hotel.ListBookingGuests)
						// Conference / events (BEO) + delegate meal cards — require conference_events.
						h.With(outletmw.RequireServicePermission(rbacSvc, "pos.conference.add", "pos.conference.manage"), conferenceFeat).
							Post("/events", hotel.CreateEventBooking)
						h.Get("/events", hotel.ListEventBookings)
						h.Get("/events/{id}", hotel.GetEventBooking)
						h.With(outletmw.RequireServicePermission(rbacSvc, "pos.conference.change", "pos.conference.manage"), conferenceFeat).
							Patch("/events/{id}", hotel.UpdateEventBooking)
						h.Get("/events/{id}/reconciliation", hotel.ReconcileEvent)
						h.With(outletmw.RequireServicePermission(rbacSvc, "pos.conference.manage"), conferenceFeat).
							Post("/events/{id}/generate-mealcards", hotel.GenerateMealCards)
						h.With(outletmw.RequireServicePermission(rbacSvc, "pos.conference.change", "pos.conference.manage"), conferenceFeat).
							Post("/mealcards/{code}/redeem", hotel.RedeemMealCard)
						h.With(hotelChange).Post("/rooms/{id}/check-in", hotel.CheckIn)
						h.With(hotelChange).Post("/rooms/{id}/check-out", hotel.CheckOut)
						h.With(hotelChange).Post("/rooms/{id}/folio", hotel.PostFolioCharge)
						h.Get("/rooms/{id}/folio", hotel.GetRoomFolio)
						// Checkout/settlement: full bill summary + record folio payments (with history).
						h.Get("/rooms/{id}/folio/summary", hotel.GetFolioSummary)
						h.With(hotelChange).Post("/rooms/{id}/settle", hotel.SettleFolio)
						h.Get("/facilities", hotel.ListFacilities)
						h.With(hotelManage).Post("/facilities", hotel.CreateFacility)
						h.Get("/facilities/{id}", hotel.GetFacility)
						h.With(hotelManage).Patch("/facilities/{id}", hotel.UpdateFacility)
						h.With(hotelManage).Delete("/facilities/{id}", hotel.DeleteFacility)
						h.With(hotelChange).Post("/facilities/{id}/book", hotel.BookFacility)
						h.With(hotelChange).Patch("/facilities/bookings/{bookingID}", hotel.UpdateBooking)
						h.With(hotelChange).Post("/facilities/bookings/{bookingID}/complete", hotel.CompleteFacilityBooking)
						h.Get("/facilities/bookings", hotel.ListFacilityBookings)
						// Amenity management
						h.Get("/amenities", hotel.ListAmenities)
						h.With(hotelManage).Post("/amenities", hotel.CreateAmenity)
						h.Get("/rooms/{id}/amenities", hotel.ListRoomAmenities)
						h.With(hotelChange).Post("/rooms/{id}/amenities", hotel.AssignAmenityToRoom)
						h.With(hotelChange).Post("/rooms/{id}/amenities/{amenityId}/charge", hotel.ChargeAmenityToGuest)
						// Late checkout and batch checkout
						h.With(hotelChange).Post("/rooms/{id}/late-checkout", hotel.LateCheckout)
						h.With(hotelChange).Post("/rooms/batch-checkout", hotel.BatchCheckout)
						// Housekeeping
						h.Get("/housekeeping", hotel.ListHousekeepingTasks)
						h.With(hotelChange).Post("/housekeeping", hotel.CreateHousekeepingTask)
						h.With(hotelChange).Patch("/housekeeping/{taskID}", hotel.UpdateHousekeepingTask)
					})
				}
			})
		})

		// ── Service-to-service (S2S) endpoints ──────────────────────────────────────
		// Internal backend-to-backend routes, authenticated with the shared
		// INTERNAL_SERVICE_KEY sent as the X-API-Key header (no user JWT). pos-api is the
		// loyalty source-of-truth (balances keyed on tenant + customer_phone), so other
		// services (e.g. ordering-backend) earn/redeem against these endpoints.
		if loyalty != nil && internalServiceKey != "" {
			api.Group(func(s2s chi.Router) {
				s2s.Use(requireInternalServiceKey(internalServiceKey))
				s2s.Route("/s2s/{tenant}", func(t chi.Router) {
					t.Post("/loyalty/earn", loyalty.S2SEarn)
					t.Post("/loyalty/redeem", loyalty.S2SRedeem)
					t.Get("/loyalty/balance", loyalty.S2SBalance)
				})
			})
		}
	})

	return r
}

// requireInternalServiceKey guards S2S routes by requiring the shared INTERNAL_SERVICE_KEY in the
// X-API-Key header. The key is compared in constant time to avoid leaking it via timing.
func requireInternalServiceKey(expected string) func(http.Handler) http.Handler {
	expectedBytes := []byte(expected)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := r.Header.Get("X-API-Key")
			if provided == "" || subtle.ConstantTimeCompare([]byte(provided), expectedBytes) != 1 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"invalid or missing service key"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
