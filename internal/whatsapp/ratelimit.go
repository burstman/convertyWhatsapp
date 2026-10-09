package whatsapp

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"whatsappconverty/internal/database"
)

// Default rate limits guard the single shared WABA: a careless merchant must
// not be able to consume the platform's whole sending capacity.
const (
	defaultPerShopLimit = 120 // messages per minute per shop
	defaultGlobalLimit  = 600 // messages per minute across all shops
	defaultWindow       = time.Minute
)

// pruneInterval is how often stale windows are cleared. One window row per scope
// per minute is already tiny, so this only stops the table growing forever.
const pruneInterval = 5 * time.Minute

// RateLimitExceeded is returned when a merchant or the platform exceeds the
// configured per-window budget.
type RateLimitExceeded struct {
	PerShop bool
}

func (e *RateLimitExceeded) Error() string {
	if e.PerShop {
		return "per-shop whatsapp rate limit exceeded"
	}
	return "platform whatsapp rate limit exceeded"
}

// RateLimiter is a fixed-window counter kept in Postgres. It used to be a Redis
// INCR/EXPIRE pair; with Redis out of the stack the send path still needs this
// guard, and the send already talks to this database, so the counter lives here
// too. A nil handle yields a no-op limiter so tests and local runs stay open.
type RateLimiter struct {
	db      database.Querier
	log     *slog.Logger
	perShop int
	global  int
	window  time.Duration

	mu        sync.Mutex
	lastPrune time.Time
}

// NewRateLimiter builds a limiter over an existing database handle. Nil yields a
// no-op limiter.
func NewRateLimiter(db database.Querier, log *slog.Logger) *RateLimiter {
	return &RateLimiter{
		db:      db,
		log:     log,
		perShop: defaultPerShopLimit,
		global:  defaultGlobalLimit,
		window:  defaultWindow,
	}
}

// Allow counts one send against both the per-shop and the platform window. It
// returns *RateLimitExceeded when either budget is already spent.
//
// One statement moves both windows, shop first. The platform row is only written
// when the shop row was: a merchant who is over their own budget must not be
// able to spend the platform's capacity on sends that never happen. Each upsert
// also re-checks its own ceiling as it updates, which is what keeps two sends in
// the same millisecond from both slipping past the limit.
func (l *RateLimiter) Allow(ctx context.Context, shopID uuid.UUID) error {
	if l.db == nil {
		return nil
	}

	shopScope := "shop:" + shopID.String()
	const globalScope = "global"

	var shopCounted, globalCounted bool
	err := l.db.QueryRow(ctx, `
		WITH counted_shop AS (
			INSERT INTO send_rate_windows (scope, window_start, count)
			VALUES ($1::text, $4::timestamptz, 1)
			ON CONFLICT (scope, window_start) DO UPDATE
				SET count = send_rate_windows.count + 1
				WHERE send_rate_windows.count < $5::int
			RETURNING 1
		), counted_global AS (
			INSERT INTO send_rate_windows (scope, window_start, count)
			SELECT $2::text, $4::timestamptz, 1
			WHERE EXISTS (SELECT 1 FROM counted_shop)
			ON CONFLICT (scope, window_start) DO UPDATE
				SET count = send_rate_windows.count + 1
				WHERE send_rate_windows.count < $3::int
			RETURNING 1
		)
		SELECT
			EXISTS (SELECT 1 FROM counted_shop),
			EXISTS (SELECT 1 FROM counted_global)`,
		shopScope, globalScope, l.global, l.windowStart(time.Now()), l.perShop).
		Scan(&shopCounted, &globalCounted)
	if err != nil {
		return err
	}

	l.prune(ctx)

	if !shopCounted {
		return &RateLimitExceeded{PerShop: true}
	}
	if !globalCounted {
		return &RateLimitExceeded{PerShop: false}
	}
	return nil
}

// windowStart truncates an instant to the current fixed window. Buckets are
// aligned to the window length, so two instances with slightly different clocks
// still agree on the boundary to within a second.
func (l *RateLimiter) windowStart(now time.Time) time.Time {
	return now.Truncate(l.window).UTC()
}

// prune clears windows older than the current one. A failure here is harmless:
// the rows are overwritten by the next window's primary keys, so the table stays
// correct and only a little larger.
func (l *RateLimiter) prune(ctx context.Context) {
	l.mu.Lock()
	if time.Since(l.lastPrune) < pruneInterval {
		l.mu.Unlock()
		return
	}
	l.lastPrune = time.Now()
	l.mu.Unlock()

	if _, err := l.db.Exec(ctx, `
		DELETE FROM send_rate_windows WHERE window_start < $1::timestamptz`,
		l.windowStart(time.Now()).Add(-l.window)); err != nil && l.log != nil {
		l.log.Warn("rate limit window cleanup failed", "error", err)
	}
}
