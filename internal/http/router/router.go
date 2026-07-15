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
	reportPDF *handlers.ReportPDFHandler,
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
	printJobs *handlers.PrintJobsHandler,
	printAgentAPI *handlers.PrintAgentAPIHandler,
	payroll *handlers.PayrollHandler,
	staffAdmin *handlers.StaffHandler,
	purchaseOrders *handlers.PurchaseOrdersHandler,
	repairs *handlers.RepairHandler,
	allowedOrigins []string,
	redisClient *redis.Client,
	internalServiceKey string,
	backups *handlers.BackupHandler,
	backupDest *handlers.BackupDestinationHandler,
	screensaverMedia *handlers.ScreensaverMediaHandler,
	mediaRoot string,
) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	// CORS must run BEFORE the rate limiter (and other early-exit middleware) so that even a 429 /
	// 401 / timeout response still carries Access-Control-Allow-* headers — otherwise the browser
	// masks the real status as an opaque CORS error. RealIP stays above so the limiter keys on the
	// true client IP. go-chi/cors also short-circuits OPTIONS preflight here, before rate limiting.
	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   allowedOrigins,
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "Origin", "X-Request-ID", "X-Tenant-ID", "X-Tenant-Slug", "X-Outlet-ID", "X-API-Key", "Idempotency-Key"},
		ExposedHeaders:   []string{"Link", "X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset", "Retry-After", "Idempotent-Replayed"},
		AllowCredentials: true,
		MaxAge:           300,
	}))
	r.Use(httpware.RequestID)
	r.Use(httpware.Logging(log))
	r.Use(httpware.Recover(log))
	r.Use(middleware.Timeout(30 * time.Second))
	r.Use(middleware.RequestSize(10 << 20)) // 10 MB max body size
	r.Use(outletmw.IPRateLimit(redisClient, outletmw.DefaultRateLimitConfig()))

	r.Get("/healthz", health.Liveness)
	r.Get("/readyz", health.Readiness)
	r.Get("/metrics", health.Metrics)
	r.Get("/v1/docs/*", handlers.SwaggerUI)

	// Public read-only media (managed screensavers). Files are admin-uploaded display
	// assets rendered on the pre-auth PIN screen, so no auth; traversal-guarded.
	if mediaRoot != "" {
		r.Get("/media/*", handlers.ServeMedia(mediaRoot))
	}

	r.Route("/api/v1", func(api chi.Router) {
		// Ã¢â€â‚¬Ã¢â€â‚¬ Platform admin endpoints (platform owner JWT required) Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬
		if serviceConfig != nil && authMiddleware != nil {
			api.Group(func(admin chi.Router) {
				admin.Use(authMiddleware.RequireAuth)
				serviceConfig.RegisterAdminRoutes(admin)

				// Platform-default backup destination (OneDrive/GDrive/S3/WebDAV/
				// SFTP/SMB) — platform-owner only. Secret params encrypted at rest.
				if backupDest != nil {
					admin.Group(func(platform chi.Router) {
						platform.Use(requirePlatformOwner)
						backupDest.RegisterPlatformRoutes(platform)
					})
				}
			})
		}

		// Ã¢â€â‚¬Ã¢â€â‚¬ Public endpoints (no auth required) Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬Ã¢â€â‚¬
		// These routes are accessible before the staff member has authenticated.
		// TenantV2 extracts tenant UUID directly from the URL path parameter.
		api.Get("/openapi.json", handlers.OpenAPIJSON)

		// Public download for the Local Print Agent installer (no auth, no tenant — a generic,
		// credential-free binary). 302-redirects to the GitHub release asset.
		api.Get("/pos/print-agent/download", handlers.PrintAgentDownload)

		// Local Print Agent job polling (AccuPOS-style spooler). The agent lives on the shop LAN
		// and polls OUT; auth is its pairing key (X-Agent-Key), not a user JWT — hence outside the
		// tenant JWT group. Long-poll claim + ack.
		if printAgentAPI != nil {
			api.Get("/pos/printing/agent/jobs", printAgentAPI.NextJob)
			api.Post("/pos/printing/agent/jobs/{jobID}/ack", printAgentAPI.AckJob)
		}

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
				pub.Post("/{tenantID}/pos/auth/pin/step-up", pinAuth.StepUp)
				pub.Post("/{tenantID}/pos/auth/pin/step-up-card", pinAuth.StepUpByCard)
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
			// Payment-gateway init proxy. Called by the embedded treasury/Paystack payment UI (a
			// cross-origin "Books" iframe) which does NOT carry the POS user's JWT — so it must be
			// public. It only forwards the server-issued intent_id to treasury (treasury validates the
			// intent), so the intent_id is the capability; requiring POS auth here 401s the handoff.
			if payments != nil {
				pub.Post("/{tenantID}/pos/payments/initiate", payments.ProxyInitiate)
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

				// RBAC routes — role/permission management. Gated so only admins/managers
				// (pos.users.view for reads, pos.users.manage for mutations via the in-handler
				// canManageRBAC checks) can enumerate or edit the role model. Previously these
				// were authenticated-only, letting any tenant user grant themselves any role.
				if rbacHandler != nil {
					tenant.Group(func(rg chi.Router) {
						rg.Use(outletmw.RequireServicePermission(rbacSvc, "pos.users.view", "pos.users.manage"))
						rbacHandler.RegisterRoutes(rg)
					})
				}

				// Outlet settings + TruLoad-inspired outlet switch
				if serviceSettings != nil {
					serviceSettings.RegisterRoutes(tenant)
				}

				// Tenant-scoped backups (this tenant's data only) — config/admin-gated.
				if backups != nil {
					tenant.Group(func(bg chi.Router) {
						bg.Use(outletmw.RequireServicePermission(rbacSvc, "pos.config.change", "pos.config.manage"))
						backups.RegisterRoutes(bg)
					})
				}

				// Per-tenant backup-destination override (mirrors backups off the
				// PVC) — same config permission gate as the tenant backups routes.
				if backupDest != nil {
					tenant.Group(func(dg chi.Router) {
						dg.Use(outletmw.RequireServicePermission(rbacSvc, "pos.config.change", "pos.config.manage"))
						backupDest.RegisterRoutes(dg)
					})
				}

				tenant.Route("/pos", func(pos chi.Router) {
					// Replay-safety for the offline-sync worker: a request carrying an
					// Idempotency-Key (the offline local_id) is executed once and its response
					// stored, so reconnect retries never duplicate sales/payments/voids/returns.
					// No-op for normal online traffic, which sends no key.
					pos.Use(outletmw.Idempotency(entClient))

					// Managed screensaver media (Settings → Display) — list/upload/delete.
					// Permission enforced inside the handlers (pos.config.change/manage).
					if screensaverMedia != nil {
						screensaverMedia.RegisterRoutes(pos)
					}

					// Orders
					if orders != nil {
						// Reads require an orders permission; the handlers additionally narrow
						// view_own-only principals (cashiers) to their OWN sales (REQ-007).
						orderRead := outletmw.RequireServicePermission(rbacSvc,
							"pos.orders.view", "pos.orders.view_own", "pos.orders.change", "pos.orders.manage")
						pos.With(orderRead).Get("/orders", orders.ListOrders)
						pos.Post("/orders", orders.CreateOrder)
						pos.With(orderRead).Get("/orders/by-number/{orderNumber}", orders.GetOrderByNumber)
						pos.With(orderRead).Get("/orders/{orderID}", orders.GetOrder)
						pos.Patch("/orders/{orderID}/status", orders.UpdateStatus)
						// All-Sales "Edit Shipping": update shipping status/address/charges (metadata).
						pos.Patch("/orders/{orderID}/shipping", orders.UpdateShipping)
						// All-Sales "New Sale Notification": (re)send the customer their receipt/invoice.
						pos.Post("/orders/{orderID}/notify", orders.NotifySale)
						pos.Patch("/orders/{orderID}/void", orders.VoidOrder)
						// Manager generates a one-time code (shareable) to authorize voiding this
						// order when they're not at the terminal. Manager-only (handler re-checks role).
						pos.Post("/orders/{orderID}/void-code", orders.GenerateVoidCode)
						// Manager generates a one-time code (shareable) to authorize closing this
						// bill via the Complimentary/no-charge tender when they're not at the
						// terminal. Manager-only (handler re-checks role).
						pos.Post("/orders/{orderID}/complimentary-code", orders.GenerateComplimentaryCode)
						// Generic (non-order-scoped) manager approval codes for pre-order actions
						// (over-limit discount / price override / order adjustment / out-of-stock
						// override). Generate is manager-only (handler re-checks the override role);
						// verify consumes a code for the client-side out-of-stock gate.
						pos.Post("/approval-codes", orders.GenerateActionApprovalCode)
						pos.Post("/approval-codes/verify", orders.VerifyActionApprovalCode)
						pos.Post("/orders/{orderID}/fire-course", orders.FireCourse)
						pos.Post("/orders/{orderID}/lines", orders.AddOrderLines)
						pos.Post("/orders/{orderID}/lines/{lineID}/void", orders.VoidOrderLine)
						// Manager/admin corrective tool: directly edit a persisted line's price/qty
						// instead of requiring a raw database fix for stale-priced sales.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Patch("/orders/{orderID}/lines/{lineID}", orders.EditOrderLine)
						// Manager/admin corrective tool: set an unsettled order's order-level discount
						// in place (recomputes totals) so a resumed sale never settles at a stale
						// pre-discount total (root cause of the 2026-07-14 duplicate-settle incident).
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Patch("/orders/{orderID}/discount", orders.SetOrderDiscount)
						// Admin/platform-owner-only corrective tool: move a settled sale's reporting
						// date (e.g. a sale rung up and synced a day late) without touching amounts,
						// payments, or the immutable created_at audit timestamp. pos.orders.manage is
						// the outer gate (defense in depth); MoveOrderDate itself further restricts to
						// the tenant's admin/owner tier — a plain manager holding pos.orders.manage is
						// NOT enough (see dateMoveAdminRoles in orders_date_move.go).
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Patch("/orders/{orderID}/date", orders.MoveOrderDate)
						// Upsell / set-aside: hold a wrongly-ordered (already-made) item for resale
						// instead of voiding it. No manager approval; must be cleared before shift close.
						pos.Post("/orders/{orderID}/lines/{lineID}/set-aside", orders.SetAsideLine)
						pos.Get("/held-items", orders.ListHeldItems)
						pos.Post("/held-items/{id}/claim", orders.ClaimHeldItem)
						pos.Post("/held-items/{id}/void", orders.VoidHeldItem)
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
						pos.Post("/orders/{orderID}/receipt/reprint", receipt.ReprintReceipt)
					}

					// QZ Tray printing bridge — serve the platform cert + sign print requests so the
					// pos-ui can print silently to assigned printers. Stateless (env-driven key/cert).
					pos.Get("/printing/qz/cert", handlers.QZCert)
					pos.Post("/printing/qz/sign", handlers.QZSign)
					// Server-side LAN printer discovery (mDNS/scan/SNMP). Env-gated — only useful for
					// on-prem pos-api on the same network as the terminals; the pos-ui tries this first
					// then falls back to the local QZ Tray / WebUSB / Bluetooth bridges.
					pos.Get("/printing/discover", handlers.PrinterDiscover)
					// Build a diagnostic test ticket (ESC/POS hex) for the printer-setup "Test print"
					// button, so a network printer prints silently via the local agent/QZ rather than
					// opening the browser print dialog. Stateless — no order required.
					pos.Post("/printing/test-ticket", handlers.TestTicket)

					// Background print queue (AccuPOS model): explicit job enqueue for Print
					// Bill/Receipt/Test-print buttons + Local Print Agent pairing/status.
					if printJobs != nil {
						pos.Post("/printing/jobs", printJobs.EnqueueJob)
						pos.Get("/printing/agents", printJobs.ListAgents)
						pos.Post("/printing/agents", printJobs.PairAgent)
						pos.Delete("/printing/agents/{agentID}", printJobs.RevokeAgent)
					}

					// Catalog
					if catalog != nil {
						pos.Route("/catalog", func(cat chi.Router) {
							cat.Get("/version", catalog.GetCatalogVersion)
							cat.Get("/categories", catalog.GetCatalogCategories)
							cat.Get("/brands", catalog.GetBrands)
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
							// NOT gated on subscriptions.RequireFeature(FeatureTableManagement): every hospitality
							// plan tier already includes "table_management" (subscriptions-api cmd/seed/
							// plans_pos_lines.go), so the gate was pure redundant surface for a JWT/plan-sync
							// failure to break core checkout actions on — notably SplitOrder/SetServiceCharge
							// below, which have nothing to do with the "Table & Floor Management" add-on the
							// code represents. RequireUseCase("hospitality") above is the real gate here.
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
						// Dropdown data for the Add-Expense form (proxied from treasury).
						pos.Get("/expenses/categories", payments.ListExpenseCategories)
						pos.Get("/expenses/accounts", payments.ListExpenseAccounts)
						// Supplier/vendor search-select for the "Expense for" field (proxied from inventory-api).
						pos.Get("/expenses/suppliers", payments.ListExpenseSuppliers)
						// Live "Reference No" preview from treasury's document-sequence service.
						pos.Get("/expenses/next-number", payments.PreviewExpenseNumber)
						// Treasury-sourced tax codes/rates for the Settings → Tax tab (read-only).
						pos.Get("/tax-codes", payments.ListTaxCodes)
						pos.Get("/c2b/payments", payments.ListC2BCandidates)
						pos.Post("/c2b/payments/{transID}/claim", payments.ClaimC2BPayment)
						// Recording a payment (cash/M-Pesa ref) or opening a payment intent is a
						// money-movement action Ã¢â‚¬â€ gate on payments.add (cashier, waiter, manager+).
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.payments.add", "pos.payments.manage")).
							Post("/orders/{orderID}/payments/intent", payments.CreatePaymentIntent)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.payments.add", "pos.payments.manage")).
							Post("/orders/{orderID}/payments", payments.RecordPayment)
						pos.With(outletmw.RequireServicePermission(rbacSvc,
							"pos.payments.view", "pos.payments.view_own", "pos.payments.manage")).
							Get("/orders/{orderID}/payments", payments.ListOrderPayments)
						// View-Payments modal actions — manager-only. Edit touches descriptive
						// fields only (never the amount); delete is a soft VOID (paid_total
						// recompute + treasury reversal); notify sends the customer a
						// payment-received confirmation.
						paymentsManage := outletmw.RequireServicePermission(rbacSvc, "pos.payments.manage")
						pos.With(paymentsManage).
							Patch("/orders/{orderID}/payments/{paymentID}", payments.UpdateOrderPayment)
						pos.With(paymentsManage).
							Delete("/orders/{orderID}/payments/{paymentID}", payments.VoidOrderPayment)
						pos.With(paymentsManage).
							Post("/orders/{orderID}/payments/{paymentID}/notify", payments.NotifyOrderPayment)
						pos.Get("/orders/{orderID}/payment-status/stream", payments.StreamPaymentStatus)
						// Bank list + account verification (proxied to treasury S2S Paystack) for the
						// receipt payment-display bank settings.
						pos.Get("/banks/{country}", payments.ListBanks)
						pos.Get("/banks/resolve", payments.ResolveBankAccount)
						// Cheap one-shot status check the pos-ui polls with bounded backoff (replaces the
						// SSE stream's reconnect storm). Rate-limit-exempt; NATS subscriber owns truth.
						pos.Get("/orders/{orderID}/payment-status", payments.GetPaymentStatus)
						// NOTE: POST /payments/initiate is registered in the PUBLIC group — the embedded
						// cross-origin Paystack iframe calls it without the POS user's JWT (intent_id is
						// the capability; treasury validates it). Keeping it here would 401 the handoff.
						// "Save as Quotation" forwards a pos cart to treasury (treasury owns quotations).
						// Quotations are a manager/back-office action (same permission as approving sale
						// returns) — an ordinary cashier must not raise them.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Post("/quotations", payments.CreateQuotationFromCart)
						// Quotation transactions tab — proxies the treasury quotation list.
						pos.Get("/quotations", payments.ListQuotationsProxy)
					}

					// Cash Drawers
					if drawers != nil {
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.cash_drawers.add", "pos.cash_drawers.manage")).
							Post("/drawers/open", drawers.OpenDrawer)
						pos.Get("/drawers/current", drawers.GetCurrentDrawer)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.cash_drawers.manage")).
							Post("/drawers/{id}/close", drawers.CloseDrawer)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.cash_drawers.add", "pos.cash_drawers.manage")).
							Post("/drawers/{id}/movement", drawers.RecordMovement)
						pos.Get("/drawers/{id}/events", drawers.ListDrawerEvents)
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
						pos.Get("/promotions/{promoID}", promotions.GetPromotion)
						// Creating/editing/deleting a promotion/happy-hour is administrative;
						// applying a code at checkout is part of the cashier order flow and stays
						// ungated.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.promotions.add", "pos.promotions.manage")).
							Post("/promotions", promotions.CreatePromotion)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.promotions.manage")).
							Patch("/promotions/{promoID}", promotions.UpdatePromotion)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.promotions.manage")).
							Delete("/promotions/{promoID}", promotions.DeletePromotion)
						pos.Post("/promotions/apply", promotions.ApplyPromoCode)
						pos.Get("/promotions/happy-hour/active", promotions.GetActiveHappyHours)
					}

					// Device sessions (shift open/close = clock-in / clock-out).
					// Opening/closing a register session is a staff action gated on
					// pos.sessions.add (managers/finance also satisfy via pos.sessions.manage),
					// mirroring the cash-drawer open/close gating above. GET reads stay
					// auth-only so every signed-in staffer can see their own shift status.
					if devices != nil {
						pos.Get("/devices", devices.ListDevices)
						pos.Get("/devices/current/sessions/current", devices.GetCurrentSession)
						pos.Get("/devices/current/sessions/current/summary", devices.GetSessionSummary)
						pos.Get("/devices/current/sessions/history", devices.GetSessionHistory)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.sessions.add", "pos.sessions.manage")).
							Post("/devices/current/sessions/open", devices.OpenSession)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.sessions.add", "pos.sessions.manage")).
							Post("/devices/current/sessions/close", devices.CloseSession)
					}

					// Terminal PIN auth (auth-protected endpoints)
					// SetPIN requires a manager/admin SSO token Ã¢â‚¬â€ no subscription gate so admins
					// can always set staff PINs regardless of plan.
					// AuthMe requires SSO token for Trinity Layer 3.
					// ListStaff / Login / StaffProfiles are registered in the public group above.
					if pinAuth != nil {
						// SetPIN + card-token set/reset ANOTHER staff member's PIN / mint a
						// manager override card — manager/admin only, now enforced server-side.
						// AuthMe stays open: it only returns the CALLER's own role + permissions.
						staffPinManage := outletmw.RequireServicePermission(rbacSvc, "pos.users.manage", "pos.staff.manage")
						pos.With(staffPinManage).Post("/auth/pin/set", pinAuth.SetPIN)
						pos.With(staffPinManage).Post("/staff/{userID}/card-token", pinAuth.IssueStaffCardToken)
						pos.Get("/auth/me", pinAuth.AuthMe)
					}

					// Staff admin CRUD (requires STAFF_MANAGE permission Ã¢â‚¬â€ enforced client-side;
					// server-side role boundary enforced in the handler itself).
					if staffAdmin != nil {
						// Server-side permission gate (was "enforced client-side"): reads need
						// users/staff view, mutations need users/staff manage. The in-handler
						// role boundary additionally stops a manager creating/editing admin staff.
						staffView := outletmw.RequireServicePermission(rbacSvc,
							"pos.users.view", "pos.users.manage", "pos.staff.view", "pos.staff.manage")
						staffManage := outletmw.RequireServicePermission(rbacSvc,
							"pos.users.manage", "pos.staff.manage")
						pos.With(staffView).Get("/staff/admin", staffAdmin.ListStaffForAdmin)
						pos.With(staffManage).Post("/staff", staffAdmin.CreateStaff)
						pos.With(staffManage).Patch("/staff/{staffID}", staffAdmin.UpdateStaff)
						pos.With(staffManage).Post("/staff/{staffID}/deactivate", staffAdmin.DeactivateStaff)
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
							// Bulk-clear the board (serve all active tickets) — manager-only. Lets a
							// single terminal clear a cluttered board (stale never-bumped tickets).
							k.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
								Post("/kds/tickets/clear", kds.ClearTickets)
						})
					}

					// Returns
					if returns != nil {
						// Initiate a return — a cashier action.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.change_own", "pos.orders.change", "pos.orders.manage")).
							Post("/orders/{orderID}/returns", returns.CreateReturn)
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
						// Approval / rejection is a manager decision.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Patch("/returns/{returnID}/approve", returns.ApproveReturn)
						// Completion (money-out + inventory restock) is done at the till by a cashier/manager.
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.change_own", "pos.orders.change", "pos.orders.manage")).
							Post("/returns/{returnID}/complete", returns.CompleteReturn)
					}

					// Layaway plans & payments. Gated with the SAME order/payment permission codes
					// every seeded role already carries (no new codes → no prod seeder run):
					// create/read for till roles, money + terminal transitions for managers.
					if layaway != nil {
						layawayRead := outletmw.RequireServicePermission(rbacSvc,
							"pos.orders.view", "pos.orders.view_own", "pos.orders.manage")
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.add", "pos.orders.manage")).
							Post("/layaways", layaway.Create)
						pos.With(layawayRead).Get("/layaways", layaway.List)
						// Staff fund-from-salary links (admin/reconcile view).
						pos.With(layawayRead).Get("/staff-credit", layaway.ListStaffCredit)
						pos.With(layawayRead).Get("/layaways/{id}", layaway.Get)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.payments.add", "pos.payments.manage")).
							Post("/layaways/{id}/payments", layaway.RecordPayment)
						layawayManage := outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")
						pos.With(layawayManage).Post("/layaways/{id}/cancel", layaway.Cancel)
						pos.With(layawayManage).Post("/layaways/{id}/forfeit", layaway.Forfeit)
						pos.With(layawayManage).Post("/layaways/{id}/complete", layaway.Complete)
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

					// Commissions (records + rules & payout). Commissions are a retail/services
					// concept (salespeople, therapists) — NOT hospitality/QSR/pharmacy. Gate the
					// whole surface by use case so cross-use-case data never mixes (matches the
					// pos-ui module map which only lists commissions for retail/services).
					if commissions != nil || commissionRules != nil {
						pos.Group(func(cm chi.Router) {
							cm.Use(outletmw.RequireUseCase("retail", "services"))
							if commissions != nil {
								cm.Get("/commissions", commissions.List)
								cm.Get("/commissions/{commissionID}", commissions.Get)
							}
							if commissionRules != nil {
								cm.Get("/commissions/rules", commissionRules.List)
								cm.Post("/commissions/rules", commissionRules.Create)
								cm.Patch("/commissions/rules/{ruleID}", commissionRules.Update)
								cm.Post("/commissions/payout", commissionRules.Payout)
							}
						})
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
						// Customer DIRECTORY (CRM-backed, loyalty-enriched) — the Clients page
						// default listing + search. Distinct from /clients (POS-local records).
						pos.Get("/customers", clients.ListCustomers)
						pos.Post("/clients", clients.CreateOrUpsert)
						pos.Post("/clients/bulk-import", clients.BulkImport)
						pos.Get("/clients/{clientID}", clients.Get)
						pos.Patch("/clients/{clientID}", clients.Update)
						pos.Get("/clients/{phone}/orders", clients.GetOrdersByPhone)
						// Credit terms (treasury AR proxy): balance + limit + payment period.
						// Setting terms = manager action (same permission that approves credit sales).
						pos.Get("/clients/{accountID}/credit", clients.GetCredit)
						pos.With(outletmw.RequireServicePermission(rbacSvc, "pos.orders.manage")).
							Put("/clients/{accountID}/credit", clients.SetCredit)
					}

					// Loyalty programs & accounts — gated on the loyalty_program feature
					// (bundles include it from Starter; POS-device plans do not).
					if loyalty != nil {
						pos.Group(func(ly chi.Router) {
							// Loyalty is a retail/services concept — not hospitality/QSR/pharmacy.
							// Gate by use case (matches the pos-ui module map) in addition to the plan feature.
							ly.Use(outletmw.RequireUseCase("retail", "services"))
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
						pos.Get("/reports/audit-logs", reports.ListAuditLogs)
						pos.Get("/reports/exceptions", reports.Exceptions)
						pos.Get("/reports/sales-summary", reports.SalesSummary)
						pos.Get("/reports/refund-summary", reports.RefundSummary)
						pos.Get("/reports/daily-breakdown", reports.DailyBreakdown)
						pos.Get("/reports/top-items", reports.TopItems)
						pos.Get("/reports/register-details", reports.RegisterDetails)
						pos.Get("/reports/sales-by-staff", reports.SalesByStaff)
						pos.Get("/reports/export", reports.ExportDailyReport)
						// Sprint 11: additional report endpoints
						pos.Get("/reports/shifts", reports.ShiftReportList)
						pos.Get("/reports/shifts/{sessionID}", reports.ShiftReport)
						pos.Get("/reports/commissions", reports.CommissionReport)
						pos.Get("/reports/tax", reports.TaxReport)
						// Hyphenated (matching every other report route + the pos-ui hooks, which
						// have always requested these two exact paths) — NOT the nested
						// "/sales/by-hour" form these two used to register under, which the
						// frontend never called and 404'd unconditionally.
						pos.Get("/reports/sales-by-hour", reports.SalesByHour)
						pos.Get("/reports/sales-by-category", reports.SalesByCategory)
						pos.Get("/reports/sales/by-kds-station", reports.SalesByKDSStation)
						pos.Get("/reports/stock-consumption", reports.StockConsumptionReport)
						pos.Get("/reports/returns", reports.ReturnsSummary)
						pos.Get("/reports/void-summary", reports.VoidSummary)
						pos.Get("/reports/product-mix", reports.ProductMix)
						pos.Get("/reports/most-profitable", reports.MostProfitableItems)
					}

					// Branded report documents (PDF/CSV via ?format=) — reset summary, item-type,
					// daily sales, shift X, staff, tax and profitability reports.
					if reportPDF != nil {
						pos.Get("/reports/reset-summary", reportPDF.ResetSummary)
						pos.Get("/reports/sales-by-item-type", reportPDF.SalesByItemType)
						pos.Get("/reports/sales-by-kds-station-document", reportPDF.SalesByKDSStationDoc)
						pos.Get("/reports/daily-sales", reportPDF.DailySales)
						pos.Get("/reports/shift/{sessionID}", reportPDF.ShiftReportPDF)
						pos.Get("/reports/staff", reportPDF.SalesByStaffPDF)
						pos.Get("/reports/tax-document", reportPDF.TaxReportPDF)
						pos.Get("/reports/most-profitable-document", reportPDF.MostProfitablePDF)
						// Analytics-page reports (cards + table + bar chart; ?format=pdf|csv).
						pos.Get("/reports/sales-by-hour-document", reportPDF.SalesByHourDoc)
						pos.Get("/reports/sales-by-category-document", reportPDF.SalesByCategoryDoc)
						pos.Get("/reports/product-mix-document", reportPDF.ProductMixDoc)
						pos.Get("/reports/void-summary-document", reportPDF.VoidSummaryDoc)
						// All-Sales page export — same filters + per-cashier scoping as GET /orders.
						pos.Get("/reports/all-sales-document", reportPDF.AllSalesDocument)
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
						// Collection history (collected + uncollected) for pickup/takeaway/delivery.
						pos.Get("/online-orders/history", onlineOrders.ListPickupHistory)
						// POS-native delivery dispatch queue (order_subtype=delivery) — read-only list.
						pos.Get("/online-orders/dispatch", onlineOrders.ListDeliveryDispatch)
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

				// Hotel module — hospitality only
				if hotel != nil {
					tenant.Route("/hotel", func(h chi.Router) {
						h.Use(outletmw.RequireUseCase("hospitality"))
						conferenceFeat := subscriptions.RequireFeature(subscriptions.FeatureConference)
						// Front-desk operational actions (check-in/out, folio, bookings, room status,
						// facility booking, amenities, housekeeping) require hotel CHANGE; admin master
						// data (create/edit/delete rooms & facilities) requires hotel MANAGE.
						hotelChange := outletmw.RequireServicePermission(rbacSvc, "pos.hotel.change", "pos.hotel.manage")
						hotelManage := outletmw.RequireServicePermission(rbacSvc, "pos.hotel.manage")

						// Inventory master pickers (link rooms/facilities/amenities to inventory SERVICE
						// items + packages) — shared by both the hotel PMS and facilities forms below,
						// so they sit outside either feature gate rather than requiring hotel_module.
						h.Get("/inventory-service-items", hotel.ListInventoryServiceItems)
						h.Get("/inventory-bundles", hotel.ListInventoryBundles)

						// ── Full hotel PMS: rooms, group bookings, conference/events, folio,
						// amenities, housekeeping. Requires hotel_module (Enterprise+).
						h.Group(func(g chi.Router) {
							g.Use(subscriptions.RequireFeature(subscriptions.FeatureHotelModule))
							g.Get("/rooms", hotel.ListRooms)
							g.With(hotelManage).Post("/rooms", hotel.CreateRoom)
							g.Get("/rooms/{id}", hotel.GetRoom)
							g.With(hotelChange).Patch("/rooms/{id}/status", hotel.UpdateRoomStatus)
							// Multi-room / group bookings (RoomBooking header -> many RoomGuest)
							g.With(hotelChange).Post("/bookings", hotel.CreateRoomBooking)
							g.Get("/bookings", hotel.ListRoomBookings)
							g.Get("/bookings/{id}", hotel.GetRoomBooking)
							g.With(hotelManage).Patch("/bookings/{id}", hotel.UpdateRoomBooking)
							g.Get("/bookings/{id}/guests", hotel.ListBookingGuests)
							// Conference / events (BEO) + delegate meal cards — require conference_events.
							g.With(outletmw.RequireServicePermission(rbacSvc, "pos.conference.add", "pos.conference.manage"), conferenceFeat).
								Post("/events", hotel.CreateEventBooking)
							g.Get("/events", hotel.ListEventBookings)
							g.Get("/events/{id}", hotel.GetEventBooking)
							g.With(outletmw.RequireServicePermission(rbacSvc, "pos.conference.change", "pos.conference.manage"), conferenceFeat).
								Patch("/events/{id}", hotel.UpdateEventBooking)
							g.Get("/events/{id}/reconciliation", hotel.ReconcileEvent)
							g.With(outletmw.RequireServicePermission(rbacSvc, "pos.conference.manage"), conferenceFeat).
								Post("/events/{id}/generate-mealcards", hotel.GenerateMealCards)
							g.With(outletmw.RequireServicePermission(rbacSvc, "pos.conference.change", "pos.conference.manage"), conferenceFeat).
								Post("/mealcards/{code}/redeem", hotel.RedeemMealCard)
							g.With(hotelChange).Post("/rooms/{id}/check-in", hotel.CheckIn)
							g.With(hotelChange).Post("/rooms/{id}/check-out", hotel.CheckOut)
							g.With(hotelChange).Post("/rooms/{id}/folio", hotel.PostFolioCharge)
							g.Get("/rooms/{id}/folio", hotel.GetRoomFolio)
							// Checkout/settlement: full bill summary + record folio payments (with history).
							g.Get("/rooms/{id}/folio/summary", hotel.GetFolioSummary)
							g.With(hotelChange).Post("/rooms/{id}/settle", hotel.SettleFolio)
							// Amenity management
							g.Get("/amenities", hotel.ListAmenities)
							g.With(hotelManage).Post("/amenities", hotel.CreateAmenity)
							g.Get("/rooms/{id}/amenities", hotel.ListRoomAmenities)
							g.With(hotelChange).Post("/rooms/{id}/amenities", hotel.AssignAmenityToRoom)
							g.With(hotelChange).Post("/rooms/{id}/amenities/{amenityId}/charge", hotel.ChargeAmenityToGuest)
							// Late checkout and batch checkout
							g.With(hotelChange).Post("/rooms/{id}/late-checkout", hotel.LateCheckout)
							g.With(hotelChange).Post("/rooms/batch-checkout", hotel.BatchCheckout)
							// Housekeeping
							g.Get("/housekeeping", hotel.ListHousekeepingTasks)
							g.With(hotelChange).Post("/housekeeping", hotel.CreateHousekeepingTask)
							g.With(hotelChange).Patch("/housekeeping/{taskID}", hotel.UpdateHousekeepingTask)
						})

						// ── Bookable spaces: co-working desks, conference/meeting rooms — sell +
						// capacity-manage a Facility from the till. Requires facility_booking
						// (POS_HOSP_PRO "Growth" and up), independent of the full hotel PMS above —
						// a cafe with spare floor space shouldn't need rooms/check-in/folio just to
						// sell co-working.
						h.Group(func(g chi.Router) {
							g.Use(subscriptions.RequireFeature(subscriptions.FeatureFacilityBooking))
							g.Get("/facilities", hotel.ListFacilities)
							g.With(hotelManage).Post("/facilities", hotel.CreateFacility)
							g.Get("/facilities/{id}", hotel.GetFacility)
							g.With(hotelManage).Patch("/facilities/{id}", hotel.UpdateFacility)
							g.With(hotelManage).Delete("/facilities/{id}", hotel.DeleteFacility)
							g.Get("/facilities/{id}/availability", hotel.GetFacilityAvailability)
							g.With(hotelChange).Post("/facilities/{id}/book", hotel.BookFacility)
							g.With(hotelChange).Patch("/facilities/bookings/{bookingID}", hotel.UpdateBooking)
							g.With(hotelChange).Post("/facilities/bookings/{bookingID}/complete", hotel.CompleteFacilityBooking)
							g.Get("/facilities/bookings", hotel.ListFacilityBookings)
						})
					})
				}
			})
		})

		// ── Service-to-service (S2S) endpoints ──────────────────────────────────────
		// Internal backend-to-backend routes, authenticated with the shared
		// INTERNAL_SERVICE_KEY sent as the X-API-Key header (no user JWT). pos-api is the
		// loyalty source-of-truth (balances keyed on tenant + customer_phone), so other
		// services (e.g. ordering-backend) earn/redeem against these endpoints.
		if internalServiceKey != "" && (loyalty != nil || reports != nil || payments != nil) {
			api.Group(func(s2s chi.Router) {
				s2s.Use(requireInternalServiceKey(internalServiceKey))
				s2s.Route("/s2s/{tenant}", func(t chi.Router) {
					if loyalty != nil {
						t.Post("/loyalty/earn", loyalty.S2SEarn)
						t.Post("/loyalty/redeem", loyalty.S2SRedeem)
						t.Get("/loyalty/balance", loyalty.S2SBalance)
					}
					if reports != nil {
						// POS units sold per SKU — consumed by inventory-api menu-engineering/variance
						// so POS sales are counted, not only ordering-service orders.
						t.Get("/pos/sales/by-sku", reports.S2SSalesBySKU)
					}
					if payments != nil {
						// Manual ops recovery tool — see S2SRecheckOrderCompletion's doc comment.
						t.Post("/orders/{orderNumber}/recheck-completion", payments.S2SRecheckOrderCompletion)
					}
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

// requirePlatformOwner gates a route to platform-owner principals only. It is used
// for the platform-default (tenant_id NULL) backup-destination management routes,
// which configure the off-PVC mirror for ALL tenants and so must never be reachable
// by an ordinary tenant user. Returns 401 when unauthenticated, 403 otherwise.
func requirePlatformOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := authclient.ClaimsFromContext(r.Context())
		if !ok || claims == nil || claims.Subject == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		// SEC-3 (auth-client v0.10.0): a tenant superuser is NOT a platform owner and must
		// never reach platform-default (tenant_id NULL) management, which configures the
		// off-PVC mirror for ALL tenants. Mirror the shared RequirePlatformOwner contract.
		if !claims.IsPlatformOwner {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"platform owner required"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
