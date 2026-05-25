package router

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"

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
	publicOutlet *handlers.PublicOutletHandler,
	closings *handlers.DailyClosingHandler,
	returns *handlers.ReturnHandler,
	receipt *handlers.ReceiptHandler,
	layaway *handlers.LayawayHandler,
	scale *handlers.ScaleHandler,
	pharmacy *handlers.PharmacyHandler,
	appointments *handlers.AppointmentHandler,
	commissions *handlers.CommissionHandler,
	staffSchedule *handlers.StaffScheduleHandler,
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
	allowedOrigins []string,
	redisClient *redis.Client,
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
		// ── Platform admin endpoints (platform owner JWT required) ────────────
		if serviceConfig != nil && authMiddleware != nil {
			api.Group(func(admin chi.Router) {
				admin.Use(authMiddleware.RequireAuth)
				serviceConfig.RegisterAdminRoutes(admin)
			})
		}

		// ── Public endpoints (no auth required) ───────────────────────────────
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
				pub.Get("/{tenantID}/pos/auth/pin/profile", pinAuth.StaffProfiles)
			}
			if publicOutlet != nil {
				pub.Get("/{tenantID}/pos/outlets", publicOutlet.ListPublicOutlets)
				pub.Get("/{tenantID}/pos/outlets/current", publicOutlet.GetCurrentOutlet)
			}
		})

		// ── Protected endpoints (auth required) ───────────────────────────────
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
						pos.Get("/orders/{orderID}", orders.GetOrder)
						pos.Patch("/orders/{orderID}/status", orders.UpdateStatus)
						pos.Patch("/orders/{orderID}/void", orders.VoidOrder)
						pos.Post("/orders/{orderID}/fire-course", orders.FireCourse)
						pos.Post("/orders/{orderID}/lines/{lineID}/serials", orders.CaptureSerial)
					}
					if print != nil {
						pos.Post("/orders/{orderID}/print", print.PrintReceipt)
					}

					// In-app notifications (waiter order-ready alerts)
					if notifications != nil {
						pos.Get("/notifications", notifications.List)
						pos.Post("/notifications/mark-all-read", notifications.MarkAllRead)
						pos.Patch("/notifications/{id}/read", notifications.MarkRead)
					}

					// Receipt
					if receipt != nil {
						pos.Get("/orders/{orderID}/receipt", receipt.GetReceipt)
						pos.Get("/orders/{orderID}/receipt/html", receipt.GetReceiptHTML)
						pos.Get("/orders/{orderID}/receipt/pdf", receipt.GetReceiptPDF)
					}

					// Catalog
					if catalog != nil {
						pos.Route("/catalog", func(cat chi.Router) {
							cat.Get("/items", catalog.ListCatalogItems)
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

					// Sections & Tables — hospitality only + table_management subscription
					if tables != nil {
						pos.Group(func(tbl chi.Router) {
							tbl.Use(outletmw.RequireUseCase("hospitality"))
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
							tbl.Post("/tables/{id}/transfer", tables.TransferTable)
							tbl.Post("/tables/merge", tables.MergeTables)
							// Order split + service charge live here (use TableHandler, need nil guard)
							tbl.Post("/orders/{orderID}/split", tables.SplitOrder)
							tbl.Patch("/orders/{orderID}/service-charge", tables.SetServiceCharge)
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

					// Bar Tabs — hospitality only
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
						pos.Post("/promotions", promotions.CreatePromotion)
						pos.Post("/promotions/apply", promotions.ApplyPromoCode)
					}

					// Device sessions (shift open/close)
					if devices != nil {
						pos.Get("/devices", devices.ListDevices)
						pos.Get("/devices/current/sessions/current", devices.GetCurrentSession)
						pos.Get("/devices/current/sessions/current/summary", devices.GetSessionSummary)
						pos.Post("/devices/current/sessions/open", devices.OpenSession)
						pos.Post("/devices/current/sessions/close", devices.CloseSession)
					}

					// Terminal PIN auth (auth-protected endpoints)
					// SetPIN requires a manager/admin SSO token — no subscription gate so admins
					// can always set staff PINs regardless of plan.
					// AuthMe requires SSO token for Trinity Layer 3.
					// ListStaff / Login / StaffProfiles are registered in the public group above.
					if pinAuth != nil {
						pos.Post("/auth/pin/set", pinAuth.SetPIN)
						pos.Get("/auth/me", pinAuth.AuthMe)
					}

					// KDS — hospitality and quick_service only; outlet must have enable_kds=true
					if kds != nil {
						pos.Group(func(k chi.Router) {
							k.Use(outletmw.RequireUseCase("hospitality", "quick_service"))
							k.Use(outletmw.RequireKDSEnabled(entClient))
							k.Get("/kds/stations", kds.ListStations)
							k.Post("/kds/stations", kds.CreateStation)
							k.Put("/kds/stations/{id}", kds.UpdateStation)
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
							pos.Post("/orders/{orderID}/splits/{splitID}/settle", billSplits.SettleSplit)
						}
						pos.Get("/returns", returns.ListReturns)
						pos.Group(func(mgr chi.Router) {
							mgr.Use(subscriptions.RequireFeature("shift_reports"))
							mgr.Patch("/returns/{returnID}/approve", returns.ApproveReturn)
						})
					}

					// Layaway plans & payments
					if layaway != nil {
						pos.Post("/layaways", layaway.Create)
						pos.Get("/layaways", layaway.List)
						pos.Get("/layaways/{id}", layaway.Get)
						pos.Post("/layaways/{id}/payments", layaway.RecordPayment)
						pos.Post("/layaways/{id}/cancel", layaway.Cancel)
					}

					// Weighing scale readings
					if scale != nil {
						pos.Post("/scale/readings", scale.Create)
						pos.Get("/scale/readings", scale.List)
					}

					// Pharmacy — pharmacy use_case only
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

					// Appointments & staff schedules — services use_case
					if appointments != nil {
						pos.Group(func(svc chi.Router) {
							svc.Use(outletmw.RequireUseCase("services"))
							svc.Get("/appointments", appointments.List)
							svc.Post("/appointments", appointments.Create)
							svc.Get("/appointments/availability", appointments.Availability)
							svc.Get("/appointments/{appointmentID}", appointments.Get)
							svc.Put("/appointments/{appointmentID}", appointments.Update)
							svc.Post("/appointments/{appointmentID}/check-in", appointments.CheckIn)
							svc.Post("/appointments/{appointmentID}/start", appointments.Start)
							svc.Post("/appointments/{appointmentID}/complete", appointments.Complete)
							svc.Post("/appointments/{appointmentID}/cancel", appointments.Cancel)
							svc.Post("/appointments/{appointmentID}/no-show", appointments.NoShow)
						})
					}

					// Walk-in queue — services use_case
					if queue != nil {
						pos.Group(func(svc chi.Router) {
							svc.Use(outletmw.RequireUseCase("services"))
							svc.Get("/queue", queue.List)
							svc.Post("/queue/entries", queue.Create)
							svc.Patch("/queue/entries/{entryID}/status", queue.UpdateStatus)
							svc.Post("/queue/entries/{entryID}/assign", queue.AssignStaff)
						})
					}

					// Resources — services use_case (chairs, rooms, equipment)
					if resources != nil {
						pos.Group(func(svc chi.Router) {
							svc.Use(outletmw.RequireUseCase("services"))
							svc.Get("/resources", resources.List)
							svc.Post("/resources", resources.Create)
							svc.Patch("/resources/{resourceID}", resources.PatchStatus)
						})
					}

					// Staff schedules
					if staffSchedule != nil {
						pos.Get("/staff/{staffID}/schedule", staffSchedule.ListSchedule)
						pos.Put("/staff/{staffID}/schedule", staffSchedule.UpsertSchedule)
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
					}

					// Loyalty programs & accounts
					if loyalty != nil {
						pos.Get("/loyalty/programs", loyalty.ListPrograms)
						pos.Post("/loyalty/programs", loyalty.CreateProgram)
						pos.Put("/loyalty/programs/{programID}", loyalty.UpdateProgram)
						pos.Get("/loyalty/accounts", loyalty.ListAccounts)
						pos.Post("/loyalty/accounts", loyalty.CreateAccount)
						pos.Get("/loyalty/accounts/{accountID}", loyalty.GetAccount)
						pos.Post("/loyalty/accounts/{accountID}/earn", loyalty.Earn)
						pos.Post("/loyalty/accounts/{accountID}/redeem", loyalty.Redeem)
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
					}

					// Webhook subscriptions & delivery log (Sprint 12)
					if webhooks != nil {
						pos.Get("/webhooks", webhooks.List)
						pos.Post("/webhooks", webhooks.Create)
						pos.Put("/webhooks/{webhookID}", webhooks.Update)
						pos.Delete("/webhooks/{webhookID}", webhooks.Delete)
						pos.Get("/webhooks/{webhookID}/deliveries", webhooks.ListDeliveries)
					}

					// Delivery channel integrations (Uber Eats, Glovo, etc.) — Sprint 12
					if channels != nil {
						pos.Get("/channels", channels.ListChannels)
						pos.Post("/channels", channels.CreateChannel)
						pos.Put("/channels/{channelID}", channels.UpdateChannel)
						pos.Delete("/channels/{channelID}", channels.DeleteChannel)
						pos.Get("/channels/{channelID}/sync-jobs", channels.ListSyncJobs)
						pos.Post("/channels/{channelID}/sync-jobs", channels.TriggerSyncJob)
					}

					// Online ordering pickup status — KDS click-and-collect (Sprint 13)
					if onlineOrders != nil {
						pos.Get("/online-orders/pickup", onlineOrders.ListPickup)
						pos.Post("/online-orders/{orderID}/ready", onlineOrders.MarkReady)
						pos.Post("/online-orders/{orderID}/collected", onlineOrders.MarkCollected)
					}

					// Daily closings (ERP reconciliation)
					if closings != nil {
						pos.Group(func(mgr chi.Router) {
							mgr.Use(subscriptions.RequireFeature("shift_reports"))
							mgr.Post("/outlets/{outletID}/daily-close", closings.CloseDay)
							mgr.Get("/outlets/{outletID}/daily-closings", closings.ListDailyClosings)
						})
					}
				})

				// Hotel module — hospitality only
				if hotel != nil {
					tenant.Route("/hotel", func(h chi.Router) {
						h.Use(outletmw.RequireUseCase("hospitality"))
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
						h.Post("/facilities/bookings/{bookingID}/complete", hotel.CompleteFacilityBooking)
						h.Get("/facilities/bookings", hotel.ListFacilityBookings)
						// Amenity management
						h.Get("/amenities", hotel.ListAmenities)
						h.Post("/amenities", hotel.CreateAmenity)
						h.Get("/rooms/{id}/amenities", hotel.ListRoomAmenities)
						h.Post("/rooms/{id}/amenities", hotel.AssignAmenityToRoom)
						h.Post("/rooms/{id}/amenities/{amenityId}/charge", hotel.ChargeAmenityToGuest)
						// Late checkout and batch checkout
						h.Post("/rooms/{id}/late-checkout", hotel.LateCheckout)
						h.Post("/rooms/batch-checkout", hotel.BatchCheckout)
						// Housekeeping
						h.Get("/housekeeping", hotel.ListHousekeepingTasks)
						h.Post("/housekeeping", hotel.CreateHousekeepingTask)
						h.Patch("/housekeeping/{taskID}", hotel.UpdateHousekeepingTask)
					})
				}
			})
		})
	})

	return r
}

