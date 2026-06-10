package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/shopspring/decimal"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	sharedcache "github.com/Bengo-Hub/cache"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/pos-service/internal/config"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/migrate"
	handlers "github.com/bengobox/pos-service/internal/http/handlers"
	router "github.com/bengobox/pos-service/internal/http/router"
	catalogmodule "github.com/bengobox/pos-service/internal/modules/catalog"
	"github.com/bengobox/pos-service/internal/modules/identity"
	inventorymodule "github.com/bengobox/pos-service/internal/modules/inventory"
	kdsmodule "github.com/bengobox/pos-service/internal/modules/kds"
	ordermodule "github.com/bengobox/pos-service/internal/modules/orders"
	paymentmodule "github.com/bengobox/pos-service/internal/modules/payments"
	promommodule "github.com/bengobox/pos-service/internal/modules/promotions"
	rbacmodule "github.com/bengobox/pos-service/internal/modules/rbac"
	shiftsmodule "github.com/bengobox/pos-service/internal/modules/shifts"
	"github.com/bengobox/pos-service/internal/modules/tenant"
	treasurymodule "github.com/bengobox/pos-service/internal/modules/treasury"
	webhookmodule "github.com/bengobox/pos-service/internal/modules/webhooks"
	"github.com/bengobox/pos-service/internal/platform/cache"
	"github.com/bengobox/pos-service/internal/platform/database"
	"github.com/bengobox/pos-service/internal/platform/events"
	"github.com/bengobox/pos-service/internal/platform/marketflow"
	orderingclient "github.com/bengobox/pos-service/internal/platform/ordering"
	"github.com/bengobox/pos-service/internal/platform/scheduler"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
	webhookspkg "github.com/bengobox/pos-service/internal/platform/webhooks"
	"github.com/bengobox/pos-service/internal/shared/logger"
)

type App struct {
	cfg                      *config.Config
	log                      *zap.Logger
	httpServer               *http.Server
	db                       *pgxpool.Pool
	entClient                *ent.Client
	cache                    *redis.Client
	events                   *nats.Conn
	outboxPublisher          *eventslib.Publisher
	webhookWorker            *webhookmodule.DeliveryWorker
	shiftAutoEndWorker       *shiftsmodule.AutoEndWorker
	kdsHub                   *kdsmodule.Hub
	layawayReminderScheduler *scheduler.LayawayReminderScheduler
}

func New(ctx context.Context) (*App, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	log, err := logger.New(cfg.App.Env)
	if err != nil {
		return nil, fmt.Errorf("logger init: %w", err)
	}

	dbPool, err := database.NewPool(ctx, cfg.Postgres)
	if err != nil {
		return nil, fmt.Errorf("postgres init: %w", err)
	}

	redisClient := cache.NewClient(cfg.Redis)

	natsConn, err := events.Connect(cfg.Events)
	if err != nil {
		log.Warn("event bus connection failed", zap.Error(err))
	}

	healthHandler := handlers.NewHealthHandler(log, dbPool, redisClient, natsConn)

	// Initialize auth-service JWT validator
	var authMiddleware *authclient.AuthMiddleware
	authConfig := authclient.DefaultConfig(
		cfg.Auth.JWKSUrl,
		cfg.Auth.Issuer,
		cfg.Auth.Audience,
	)
	authConfig.CacheTTL = cfg.Auth.JWKSCacheTTL
	authConfig.RefreshInterval = cfg.Auth.JWKSRefreshInterval

	validator, err := authclient.NewValidator(authConfig)
	if err != nil {
		return nil, fmt.Errorf("auth validator init: %w", err)
	}
	if cfg.Auth.EnableAPIKeyAuth {
		apiKeyValidator := authclient.NewAPIKeyValidator(cfg.Auth.ServiceURL, nil)
		authMiddleware = authclient.NewAuthMiddlewareWithAPIKey(validator, apiKeyValidator)
	} else {
		authMiddleware = authclient.NewAuthMiddleware(validator)
	}

	sqlDB, err := sql.Open("pgx", cfg.Postgres.URL)
	if err != nil {
		return nil, fmt.Errorf("sql open for ent: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.Postgres.MaxOpenConns)
	sqlDB.SetMaxIdleConns(cfg.Postgres.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(cfg.Postgres.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(1 * time.Minute)
	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	entClient := ent.NewClient(ent.Driver(drv))

	// Run versioned migrations only when explicitly enabled.
	// In production, migrations are run by the entrypoint before the server starts.
	if cfg.Postgres.RunMigrations {
		if err := entClient.Schema.Create(ctx,
			schema.WithDir(migrate.Dir),
		); err != nil {
			return nil, fmt.Errorf("ent schema create: %w", err)
		}
		log.Info("versioned migrations completed")
	}

	subsClient := subscriptions.NewClient(subscriptions.Config{
		ServiceURL:     cfg.Subscriptions.ServiceURL,
		RequestTimeout: cfg.Subscriptions.RequestTimeout,
		APIKey:         cfg.Subscriptions.APIKey,
	})

	tenantCache := sharedcache.New(redisClient, log)
	tenantSyncer := tenant.NewSyncer(entClient, cfg.Auth.ServiceURL, tenantCache)
	identitySvc := identity.NewService(entClient, tenantSyncer)

	// Initialize business services
	orderSvc := ordermodule.NewService(entClient, ordermodule.Config{
		DefaultCurrency: cfg.App.DefaultCurrency,
		TaxRatePercent:  cfg.App.TaxRatePercent,
		OrderPrefix:     cfg.App.OrderPrefix,
	}, log)

	// Wire event publisher for POS order lifecycle events (shared-events outbox pattern)
	var outboxPub *eventslib.Publisher
	if natsConn != nil {
		eventPub := events.NewPublisher(sqlDB, log)
		orderSvc.SetPublisher(eventPub)

		// Start background outbox publisher
		js, jsErr := natsConn.JetStream()
		if jsErr != nil {
			log.Warn("jetstream init for outbox publisher", zap.Error(jsErr))
		} else {
			pubCfg := eventslib.DefaultPublisherConfig(js, eventPub.OutboxRepo(), log)
			outboxPub = eventslib.NewPublisher(pubCfg)
		}
	}

	// MarketFlow CRM S2S client (async contact upsert on loyalty account creation)
	mfClient := marketflow.NewClient(cfg.MarketFlow.ServiceURL, cfg.MarketFlow.APIKey, log)

	// Treasury S2S client (thin proxy; pos-api delegates all payment processing to treasury-api)
	treasuryClient := treasurymodule.NewClient(cfg.Treasury.ServiceURL, cfg.Treasury.InternalServiceKey, cfg.Treasury.RequestTimeout)

	// Tax resolver: fetches TaxCode definitions from treasury S2S with Redis caching (10-min TTL).
	// Used by orders.Service to compute tax per order line at creation time.
	taxResolver := ordermodule.NewTaxResolver(treasuryClient, redisClient, log)
	orderSvc.SetTaxResolver(taxResolver)

	// Inventory S2S client for stock backflush after order completion
	inventoryAPIURL := os.Getenv("INVENTORY_API_URL")
	if inventoryAPIURL == "" {
		inventoryAPIURL = "http://inventory-api.inventory.svc.cluster.local:4000"
	}
	inventoryClient := inventorymodule.NewClient(inventoryAPIURL, cfg.Treasury.InternalServiceKey, 15*time.Second)

	paymentSvc := paymentmodule.NewService(entClient, orderSvc, cfg.App.DefaultCurrency, log)
	paymentSvc.SetTreasuryClient(treasuryClient)
	paymentSvc.SetInventoryClient(inventoryClient)
	if pub := orderSvc.GetPublisher(); pub != nil {
		paymentSvc.SetPublisher(pub)
	}
	promoSvc := promommodule.NewService(entClient, log)
	// Auto-apply scope-enforced happy-hour / negotiated-meal discounts at checkout, and audit
	// the applied promo (decoupled hooks into the orders service; app.go adapts the line types).
	orderSvc.SetHappyHourEvaluator(
		func(ctx context.Context, tenantID, outletID uuid.UUID, lines []ordermodule.OrderLineInput) (uuid.UUID, decimal.Decimal) {
			dls := make([]promommodule.DiscountLine, 0, len(lines))
			for _, l := range lines {
				dls = append(dls, promommodule.DiscountLine{SKU: l.SKU, Total: decimal.NewFromFloat(l.TotalPrice)})
			}
			r := promoSvc.EvaluateAutoDiscount(ctx, tenantID, outletID, dls)
			return r.PromoID, r.Discount
		},
		promoSvc.RecordApplication,
	)

	// Create HTTP handlers
	orderHandler := handlers.NewPOSOrderHandler(log, entClient, orderSvc, subsClient)
	catalogHandler := handlers.NewCatalogHandler(log, entClient)
	catalogHandler.SetRedisClient(redisClient)
	tableHandler := handlers.NewTableHandler(log, entClient)
	if pub := orderSvc.GetPublisher(); pub != nil {
		tableHandler.SetPublisher(pub)
	}
	tenderHandler := handlers.NewTenderHandler(log, entClient)
	paymentHandler := handlers.NewPaymentHandler(log, paymentSvc, treasuryClient, cfg.Treasury.PublicBaseURL, entClient)
	var drawerPublisher *events.Publisher
	if pub := orderSvc.GetPublisher(); pub != nil {
		drawerPublisher = pub
	}
	drawerHandler := handlers.NewDrawerHandler(log, entClient, drawerPublisher)
	barTabHandler := handlers.NewBarTabHandler(log, entClient)
	promotionHandler := handlers.NewPromotionHandler(log, entClient, promoSvc)

	// Hotel, KDS and device session handlers
	var hotelEventPub *events.Publisher
	if pub := orderSvc.GetPublisher(); pub != nil {
		hotelEventPub = pub
	}
	hotelHandler := handlers.NewHotelHandler(log, entClient, hotelEventPub)
	hotelHandler.SetTreasuryClient(treasuryClient)
	hotelHandler.SetInventoryClient(inventoryClient)
	hotelHandler.SetSubscriptionsClient(subsClient)
	kdsHandler := handlers.NewKDSHandler(log, entClient)
	kdsHandler.Hub().SetRedis(redisClient)
	kdsHub := kdsHandler.Hub()
	deviceHandler := handlers.NewDeviceHandler(log, entClient)
	if pub := orderSvc.GetPublisher(); pub != nil {
		deviceHandler.SetPublisher(pub)
	}
	notificationsHandler := handlers.NewNotificationsHandler(log, entClient)
	// Wire the shared notification hub so KDS can push real-time alerts to floor staff.
	kdsHandler.SetNotifHub(notificationsHandler.Hub())
	queueHandler := handlers.NewQueueHandler(log, entClient)

	// Terminal PIN auth — TERMINAL_JWT_SECRET must be set in production.
	// Falls back to INTERNAL_SERVICE_KEY only to prevent a hard startup failure in dev/local environments.
	terminalJWTSecret := []byte(cfg.Auth.TerminalJWTSecret)
	if len(terminalJWTSecret) == 0 {
		log.Warn("TERMINAL_JWT_SECRET is not set; falling back to INTERNAL_SERVICE_KEY for terminal JWT signing — set TERMINAL_JWT_SECRET in production")
		terminalJWTSecret = []byte(cfg.Treasury.InternalServiceKey)
	}
	pinAuthHandler := handlers.NewPINAuthHandler(log, entClient, terminalJWTSecret, subsClient)
	publicOutletHandler := handlers.NewPublicOutletHandler(log, entClient)

	// Retail module: layaway plans, weighing scale, purchase orders proxy
	layawayHandler := handlers.NewLayawayHandler(log, entClient)
	scaleHandler := handlers.NewScaleHandler(log, entClient)
	purchaseOrdersHandler := handlers.NewPurchaseOrdersHandler(log, entClient)

	// Pharmacy + Service modules (Sprint 8/9)
	pharmacyHandler := handlers.NewPharmacyHandler(log, entClient)
	appointmentHandler := handlers.NewAppointmentHandler(log, entClient)
	commissionHandler := handlers.NewCommissionHandler(log, entClient)
	staffScheduleHandler := handlers.NewStaffScheduleHandler(log, entClient)
	shiftOverrideHandler := handlers.NewStaffShiftOverrideHandler(log, entClient)
	leaveRequestHandler := handlers.NewLeaveRequestHandler(log, entClient)
	shiftRotationHandler := handlers.NewShiftRotationHandler(log, entClient)
	payrollHandler := handlers.NewPayrollHandler(log, entClient, treasuryClient)

	// Loyalty programs (Sprint 10)
	loyaltyHandler := handlers.NewLoyaltyHandler(log, entClient, mfClient)
	if pub := orderSvc.GetPublisher(); pub != nil {
		loyaltyHandler.SetPublisher(pub)
	}

	// Reports & Analytics (Sprint 11)
	reportsHandler := handlers.NewReportsHandler(log, entClient)

	// Webhook subscriptions (Sprint 12)
	webhookHandler := handlers.NewWebhookHandler(log, entClient)

	// Ordering-backend S2S client — used to DELEGATE delivery rider assignment to the
	// canonical admin endpoint (ordering-backend owns the order + rider-assignment flow).
	orderingS2SClient := orderingclient.NewClient(cfg.Ordering.ServiceURL, cfg.Ordering.APIKey, cfg.Ordering.RequestTimeout, log)

	// Online ordering pickup status + WS-D rider assignment (Sprint 13)
	onlineOrderHandler := handlers.NewOnlineOrderHandler(log, entClient)
	if pub := orderSvc.GetPublisher(); pub != nil {
		onlineOrderHandler.SetPublisher(pub)
	}
	// Wire WS-D deps: ordering S2S client (assign-rider delegation) + logistics base URL/key
	// (available-riders proxy). Both use the shared INTERNAL_SERVICE_KEY.
	onlineOrderHandler.SetRiderDeps(orderingS2SClient, cfg.Logistics.ServiceURL, cfg.Logistics.APIKey)

	// Platform admin: service configuration CRUD
	serviceConfigHandler := handlers.NewServiceConfigHandler(entClient, log)

	// Tenant/outlet POS settings (receipt, printer, module toggles, outlet switch)
	serviceSettingsHandler := handlers.NewServiceSettingsHandler(log, entClient)

	// ERP: daily closings + returns
	closingHandler := handlers.NewDailyClosingHandler(log, entClient)
	var returnEventPub *events.Publisher
	if pub := orderSvc.GetPublisher(); pub != nil {
		returnEventPub = pub
	}
	returnHandler := handlers.NewReturnHandler(log, entClient, treasuryClient, returnEventPub)
	receiptHandler := handlers.NewReceiptHandler(log, entClient, tenantCache, cfg.Auth.ServiceURL)
	// Branded, printable customer menu document (public/tokenless — QR target). Reuses the
	// catalog assembly + tenant branding cache, mirroring ReceiptHandler wiring.
	menuHandler := handlers.NewMenuHandler(log, entClient, tenantCache, cfg.Auth.ServiceURL, catalogHandler)

	// Initialize RBAC
	rbacRepo := rbacmodule.NewEntRepository(entClient)
	rbacSvc := rbacmodule.NewService(rbacRepo, log)
	rbacHandler := handlers.NewRBACHandler(log, rbacSvc, rbacRepo)

	// Wire RBAC service into identity for JIT role assignment from JWT claims
	identitySvc.SetRBACService(rbacSvc)

	// Subscribe to auth-service user events for proactive user sync
	authEventHandler := identity.NewAuthEventHandler(entClient, identitySvc, log)
	if natsConn != nil {
		if err := authEventHandler.SubscribeToAuthEvents(natsConn); err != nil {
			log.Warn("app: failed to subscribe to auth user events", zap.Error(err))
		}
	}

	// Subscribe to auth.outlet.* NATS events — keeps local outlet mirror in sync
	authOutletHandler := identity.NewAuthOutletEventHandler(entClient, tenantSyncer, log)
	if natsConn != nil {
		if err := authOutletHandler.SubscribeToOutletEvents(natsConn); err != nil {
			log.Warn("app: failed to subscribe to auth outlet events", zap.Error(err))
		}
	}

	// Subscribe to ordering.order.confirmed — the SINGLE online-order ingestion path for
	// BOTH pickup (click-and-collect) and delivery. Idempotent via OrderLink; creates the
	// POSOrder + lines + OrderLink and routes KDS tickets to the correct stations.
	confirmedConsumer := ordermodule.NewConfirmedOrderConsumer(entClient, orderSvc, log)
	if natsConn != nil {
		if eventPub := orderSvc.GetPublisher(); eventPub != nil {
			confirmedConsumer.SetPublisher(eventPub)
		}
		if err := confirmedConsumer.SubscribeToConfirmedOrders(natsConn); err != nil {
			log.Warn("app: failed to subscribe to ordering.order.confirmed events", zap.Error(err))
		}
	}

	// Legacy ordering.order.for_pickup consumer — retained but now a no-op whenever an
	// OrderLink already exists (the confirmed consumer is authoritative). Kept subscribed
	// for backward compatibility with any still-publishing ordering-backend version.
	pickupConsumer := ordermodule.NewPickupConsumer(entClient, orderSvc, log)
	if natsConn != nil {
		if eventPub := orderSvc.GetPublisher(); eventPub != nil {
			pickupConsumer.SetPublisher(eventPub)
		}
		if err := pickupConsumer.SubscribeToPickupOrders(natsConn); err != nil {
			log.Warn("app: failed to subscribe to ordering click-and-collect events", zap.Error(err))
		}
	}

	// Wire KDS hub into order service and ordering subscriber so new tickets
	// broadcast immediately to connected KDS WebSocket clients.
	orderSvc.SetKDSHub(kdsHandler.Hub())

	// Subscribe to ordering.order.status.changed to create/update KDS tickets (Sprint 13)
	kdsOrderingSubscriber := ordermodule.NewKDSOrderingSubscriber(entClient, log)
	kdsOrderingSubscriber.SetKDSHub(kdsHandler.Hub())
	if natsConn != nil {
		if eventPub := orderSvc.GetPublisher(); eventPub != nil {
			kdsOrderingSubscriber.SetPublisher(eventPub)
		}
		if err := kdsOrderingSubscriber.SubscribeToOrderingEvents(natsConn); err != nil {
			log.Warn("app: failed to subscribe to ordering status events for KDS", zap.Error(err))
		}
	}

	// Subscribe to treasury events: payment.success/failed → complete/fail local payment; etims → store invoice data
	treasurySubscriber := paymentmodule.NewTreasurySubscriber(entClient, paymentSvc, log)
	if natsConn != nil {
		if err := treasurySubscriber.SubscribeToTreasuryEvents(natsConn); err != nil {
			log.Warn("app: failed to subscribe to treasury events", zap.Error(err))
		}
	}

	// Subscribe to inventory events for catalog projection sync + initial sync
	inventoryEventHandler := catalogmodule.NewInventoryEventHandler(entClient, log)
	if natsConn != nil {
		if err := inventoryEventHandler.SubscribeToInventoryEvents(natsConn); err != nil {
			log.Warn("app: failed to subscribe to inventory events for catalog sync", zap.Error(err))
		}
	}

	// Invalidate tenant branding cache when subscription changes so new plan
	// is reflected in subsequent JWT-enriched responses without a restart.
	if natsConn != nil {
		subCacheSub := subscriptions.NewCacheSubscriber(redisClient, log)
		if err := subCacheSub.Start(natsConn); err != nil {
			log.Warn("app: failed to start subscription cache subscriber", zap.Error(err))
		}
	}

	// Subscribe to pos.sale.finalized: auto-earn loyalty points + create commission records + ERP pass-through
	if natsConn != nil {
		saleFinalizedSub := subscriptions.NewSaleFinalizedSubscriber(entClient, log, orderSvc.GetPublisher())
		if err := saleFinalizedSub.Start(natsConn); err != nil {
			log.Warn("app: failed to start sale.finalized subscriber", zap.Error(err))
		}
	}

	// Subscribe to inventory.stock.low → re-publish as pos.alert.stock_low for notifications-service
	if natsConn != nil {
		if eventPub := orderSvc.GetPublisher(); eventPub != nil {
			stockSub := events.NewStockSubscriber(eventPub, entClient, log)
			if err := stockSub.Subscribe(natsConn); err != nil {
				log.Warn("app: failed to subscribe to inventory.stock.low", zap.Error(err))
			}
		}
	}

	// Webhook dispatcher: fan-out pos.> NATS events to matching webhook subscriptions with HTTP delivery + backoff
	if natsConn != nil {
		webhookDispatcher := webhookspkg.NewDispatcher(entClient, log)
		if err := webhookDispatcher.Start(natsConn); err != nil {
			log.Warn("app: failed to start webhook dispatcher", zap.Error(err))
		}
	}

	webhookWorker := webhookmodule.NewDeliveryWorker(entClient, log)
	shiftAutoEndWorker := shiftsmodule.NewAutoEndWorker(entClient, log)
	var layawayReminder *scheduler.LayawayReminderScheduler
	if eventPub := orderSvc.GetPublisher(); eventPub != nil {
		layawayReminder = scheduler.NewLayawayReminderScheduler(log, entClient, eventPub)
	}

	// Wire publisher into KDS handler for waiter notification event publishing
	if pub := orderSvc.GetPublisher(); pub != nil {
		kdsHandler.SetPublisher(pub)
	}

	billSplitHandler := handlers.NewBillSplitHandler(log.Named("bill-splits"), entClient)
	resourceHandler := handlers.NewResourceHandler(log, entClient)
	commissionRuleHandler := handlers.NewCommissionRuleHandler(log, entClient)
	packageHandler := handlers.NewPackageHandler(log, entClient)
	clientHandler := handlers.NewClientHandler(log, entClient)
	clientHandler.SetMarketFlowClient(mfClient)
	channelHandler := handlers.NewChannelHandler(log, entClient)
	printHandler := handlers.NewPrintHandler(log, entClient)
	staffAdminHandler := handlers.NewStaffHandler(log.Named("staff-admin"), entClient)
	// Repair / job-card module (device repair lifecycle: intake -> ... -> settled via POS)
	repairHandler := handlers.NewRepairHandler(log, entClient)
	chiRouter := router.New(log, healthHandler, authMiddleware, entClient, identitySvc, orderHandler, catalogHandler, tableHandler, tenderHandler, paymentHandler, drawerHandler, barTabHandler, promotionHandler, rbacHandler, rbacSvc, hotelHandler, kdsHandler, deviceHandler, pinAuthHandler, publicOutletHandler, closingHandler, returnHandler, receiptHandler, menuHandler, layawayHandler, scaleHandler, pharmacyHandler, appointmentHandler, commissionHandler, staffScheduleHandler, shiftOverrideHandler, leaveRequestHandler, shiftRotationHandler, loyaltyHandler, reportsHandler, webhookHandler, onlineOrderHandler, serviceConfigHandler, serviceSettingsHandler, notificationsHandler, queueHandler, billSplitHandler, resourceHandler, commissionRuleHandler, packageHandler, clientHandler, channelHandler, printHandler, payrollHandler, staffAdminHandler, purchaseOrdersHandler, repairHandler, cfg.HTTP.AllowedOrigins, redisClient, cfg.Treasury.InternalServiceKey)

	httpServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler:           chiRouter,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}

	return &App{
		cfg:                      cfg,
		log:                      log,
		httpServer:               httpServer,
		db:                       dbPool,
		entClient:                entClient,
		cache:                    redisClient,
		events:                   natsConn,
		outboxPublisher:          outboxPub,
		webhookWorker:            webhookWorker,
		shiftAutoEndWorker:       shiftAutoEndWorker,
		kdsHub:                   kdsHub,
		layawayReminderScheduler: layawayReminder,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
	// Start outbox background publisher for POS events
	if a.outboxPublisher != nil {
		go func() {
			if err := a.outboxPublisher.Start(ctx); err != nil {
				a.log.Error("outbox publisher stopped", zap.Error(err))
			}
		}()
		a.log.Info("outbox background publisher started")
	}

	// Start webhook delivery worker — polls pending deliveries every 10s
	if a.webhookWorker != nil {
		go a.webhookWorker.Start(ctx)
		a.log.Info("webhook delivery worker started")
	}

	// Start shift auto-end worker — closes overdue shift sessions every 15 min
	if a.shiftAutoEndWorker != nil {
		go a.shiftAutoEndWorker.Start(ctx)
	}

	// Start KDS hub Redis pub/sub relay — no-op if Redis is not configured
	go a.kdsHub.Start(ctx)

	// Start layaway payment-due reminder scheduler — fires once at startup then every 24h
	if a.layawayReminderScheduler != nil {
		go a.layawayReminderScheduler.Start(ctx)
	}

	a.log.Info("pos service starting", zap.String("addr", a.httpServer.Addr))

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.httpServer.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := a.httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}

		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("http server error: %w", err)
	}
}

func (a *App) Close() {
	if a.events != nil {
		if err := a.events.Drain(); err != nil {
			a.log.Warn("nats drain failed", zap.Error(err))
		}
		a.events.Close()
	}

	if a.cache != nil {
		if err := a.cache.Close(); err != nil {
			a.log.Warn("redis close failed", zap.Error(err))
		}
	}

	if a.entClient != nil {
		if err := a.entClient.Close(); err != nil {
			a.log.Warn("ent close failed", zap.Error(err))
		}
	}

	if a.db != nil {
		a.db.Close()
	}

	_ = a.log.Sync()
}
