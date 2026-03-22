package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/pos-service/internal/config"
	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/migrate"
	handlers "github.com/bengobox/pos-service/internal/http/handlers"
	router "github.com/bengobox/pos-service/internal/http/router"
	"github.com/bengobox/pos-service/internal/modules/identity"
	ordermodule "github.com/bengobox/pos-service/internal/modules/orders"
	paymentmodule "github.com/bengobox/pos-service/internal/modules/payments"
	promommodule "github.com/bengobox/pos-service/internal/modules/promotions"
	catalogmodule "github.com/bengobox/pos-service/internal/modules/catalog"
	rbacmodule "github.com/bengobox/pos-service/internal/modules/rbac"
	"github.com/bengobox/pos-service/internal/modules/tenant"
	"github.com/bengobox/pos-service/internal/platform/cache"
	"github.com/bengobox/pos-service/internal/platform/database"
	"github.com/bengobox/pos-service/internal/platform/events"
	"github.com/bengobox/pos-service/internal/platform/subscriptions"
	"github.com/bengobox/pos-service/internal/shared/logger"
)

type App struct {
	cfg        *config.Config
	log        *zap.Logger
	httpServer *http.Server
	db         *pgxpool.Pool
	entClient  *ent.Client
	cache      *redis.Client
	events     *nats.Conn
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
	authMiddleware = authclient.NewAuthMiddleware(validator)

	sqlDB, err := sql.Open("pgx", cfg.Postgres.URL)
	if err != nil {
		return nil, fmt.Errorf("sql open for ent: %w", err)
	}
	drv := entsql.OpenDB(dialect.Postgres, sqlDB)
	entClient := ent.NewClient(ent.Driver(drv))

	// Run versioned migrations on startup
	if err := entClient.Schema.Create(ctx, 
		schema.WithDir(migrate.Dir),
	); err != nil {
		return nil, fmt.Errorf("ent schema create: %w", err)
	}
	log.Info("versioned migrations completed")

	subsClient := subscriptions.NewClient(subscriptions.Config{
		ServiceURL:     cfg.Subscriptions.ServiceURL,
		RequestTimeout: cfg.Subscriptions.RequestTimeout,
	})

	tenantSyncer := tenant.NewSyncer(entClient, cfg.Auth.ServiceURL)
	identitySvc := identity.NewService(entClient, tenantSyncer)

	// Initialize business services
	orderSvc := ordermodule.NewService(entClient, ordermodule.Config{
		DefaultCurrency: cfg.App.DefaultCurrency,
		TaxRatePercent:  cfg.App.TaxRatePercent,
		OrderPrefix:     cfg.App.OrderPrefix,
	}, log)
	paymentSvc := paymentmodule.NewService(entClient, orderSvc, cfg.App.DefaultCurrency, log)
	promoSvc := promommodule.NewService(entClient, log)

	// Create HTTP handlers
	orderHandler := handlers.NewPOSOrderHandler(log, entClient, orderSvc, subsClient)
	catalogHandler := handlers.NewCatalogHandler(log, entClient)
	tableHandler := handlers.NewTableHandler(log, entClient)
	tenderHandler := handlers.NewTenderHandler(log, entClient)
	paymentHandler := handlers.NewPaymentHandler(log, paymentSvc)
	drawerHandler := handlers.NewDrawerHandler(log, entClient)
	barTabHandler := handlers.NewBarTabHandler(log, entClient)
	promotionHandler := handlers.NewPromotionHandler(log, entClient, promoSvc)

	// Initialize RBAC
	rbacRepo := rbacmodule.NewEntRepository(entClient)
	rbacSvc := rbacmodule.NewService(rbacRepo, log)
	rbacHandler := handlers.NewRBACHandler(log, rbacSvc, rbacRepo)

	// Subscribe to inventory events for catalog projection sync
	if natsConn != nil {
		inventoryEventHandler := catalogmodule.NewInventoryEventHandler(entClient, log)
		if err := inventoryEventHandler.SubscribeToInventoryEvents(natsConn); err != nil {
			log.Warn("app: failed to subscribe to inventory events for catalog sync", zap.Error(err))
		}
	}

	chiRouter := router.New(log, healthHandler, authMiddleware, identitySvc, orderHandler, catalogHandler, tableHandler, tenderHandler, paymentHandler, drawerHandler, barTabHandler, promotionHandler, rbacHandler, cfg.HTTP.AllowedOrigins)

	httpServer := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler:           chiRouter,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}

	return &App{
		cfg:        cfg,
		log:        log,
		httpServer: httpServer,
		db:         dbPool,
		entClient:  entClient,
		cache:      redisClient,
		events:     natsConn,
	}, nil
}

func (a *App) Run(ctx context.Context) error {
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
