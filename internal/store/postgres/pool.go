package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ryancswallace/jobman-control/internal/config"
)

// Open creates and verifies a bounded PostgreSQL connection pool.
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	return OpenConfigured(ctx, databaseURL, config.DefaultPoolSettings())
}

// OpenConfigured applies validated operational limits without putting them in
// the secret-bearing database URL.
func OpenConfigured(ctx context.Context, databaseURL string, settings config.PoolSettings) (*pgxpool.Pool, error) {
	configuration, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("parse PostgreSQL configuration")
	}
	configuration.MaxConns = settings.MaxConnections
	configuration.MinConns = settings.MinConnections
	configuration.MaxConnLifetime = settings.MaxLifetime
	configuration.MaxConnIdleTime = settings.MaxIdleTime
	configuration.HealthCheckPeriod = settings.HealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, configuration)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()

		return nil, fmt.Errorf("connect to PostgreSQL: %w", err)
	}

	return pool, nil
}
