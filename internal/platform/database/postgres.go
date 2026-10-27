package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bengobox/pos-service/internal/config"
)

func NewPool(ctx context.Context, cfg config.PostgresConfig) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse config: %w", err)
	}

	poolConfig.MaxConns = int32(cfg.MaxOpenConns)
	poolConfig.MinConns = int32(cfg.MaxIdleConns)
	poolConfig.MaxConnLifetime = cfg.ConnMaxLifetime

	// Session timeouts are set on every new connection. A single pool.Exec reached only one of
	// the pool's connections. PgBouncer pools per database and user, so behind it the SET only
	// lands on this service's own server connections.
	poolConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return applySessionTimeouts(ctx, conn, cfg)
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("postgres: create pool: %w", err)
	}
	return pool, nil
}

func applySessionTimeouts(ctx context.Context, conn *pgx.Conn, cfg config.PostgresConfig) error {
	if cfg.StatementTimeout > 0 {
		if _, err := conn.Exec(ctx, fmt.Sprintf("SET statement_timeout = '%dms'", cfg.StatementTimeout.Milliseconds())); err != nil {
			return fmt.Errorf("postgres: set statement_timeout: %w", err)
		}
	}
	if cfg.IdleInTransactionTimeout > 0 {
		if _, err := conn.Exec(ctx, fmt.Sprintf("SET idle_in_transaction_session_timeout = '%dms'", cfg.IdleInTransactionTimeout.Milliseconds())); err != nil {
			return fmt.Errorf("postgres: set idle_in_transaction_session_timeout: %w", err)
		}
	}
	return nil
}
