package database

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect opens a PostgreSQL connection pool and verifies connectivity.
//
// DefaultQueryExecMode is overridden to CacheDescribe: the pgx default caches
// every query as a named server-side prepared statement, which breaks behind
// poolers (Neon runs PgBouncer in transaction mode) — the pool re-parses under
// a statement name the backend already holds and the server aborts the
// connection with FATAL: prepared statement name is already in use (08P01).
// CacheDescribe still uses the extended protocol with binary results and caches
// the parameter/result type info client-side, but leaves no named prepared
// statement behind, so nothing can collide.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return pool, nil
}

// Ping reports whether the database is reachable.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return pool.Ping(ctx)
}

// LogStats logs pool statistics at the given interval until ctx is done.
func LogStats(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats := pool.Stat()
			logger.Debug("db pool stats",
				"acquired", stats.AcquiredConns(),
				"total", stats.TotalConns(),
				"idle", stats.IdleConns(),
			)
		}
	}
}
