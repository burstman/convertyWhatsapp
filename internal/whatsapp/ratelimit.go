package whatsapp

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"whatsappconverty/internal/database"
)

// Default rate limits guard the single shared WABA: a careless merchant must
// not be able to consume the platform's whole sending capacity.
const (
	defaultPerShopLimit = 120 // messages per minute per shop
	defaultGlobalLimit  = 600 // messages per minute across all shops
	defaultWindow       = time.Minute
)

// Default dedicated test-send budget. A test message costs the platform a real
// Meta message and bypasses the automation eligibility checks, so it is capped
// far tighter than customer traffic, per actor and per shop.
const (
	defaultTestSendLimit  = 10
	defaultTestSendWindow = time.Hour
)

// pruneInterval is how often stale windows are cleared. One window row per scope
// per minute is already tiny, so this only stops the table growing forever.
const pruneInterval = 5 * time.Minute

// RateLimitExceeded is returned when a merchant or the platform exceeds the
// configured per-window budget.
type RateLimitExceeded struct {
	PerShop bool
	// TestSend marks the dedicated merchant test-send budget rather than the
	// shared customer-send budget.
	TestSend bool
}

func (e *RateLimitExceeded) Error() string {
	switch {
	case e.TestSend:
		return "test-send rate limit exceeded"
	case e.PerShop:
		return "per-shop whatsapp rate limit exceeded"
	default:
		return "platform whatsapp rate limit exceeded"
	}
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

	// testLimit/testWindow bound the dedicated test-send budget. They are
	// deliberately separate from the customer-send windows so that a test send
	// can never be hidden inside (or consume) a shop's notification allowance.
	testLimit  int
	testWindow time.Duration

	mu        sync.Mutex
	lastPrune time.Time
}

// NewRateLimiter builds a limiter over an existing database handle. Nil yields a
// no-op limiter.
func NewRateLimiter(db database.Querier, log *slog.Logger) *RateLimiter {
	return &RateLimiter{
		db:         db,
		log:        log,
		perShop:    defaultPerShopLimit,
		global:     defaultGlobalLimit,
		window:     defaultWindow,
		testLimit:  defaultTestSendLimit,
		testWindow: defaultTestSendWindow,
	}
}

// WithTestSendLimit overrides the dedicated test-send budget. Non-positive
// values are ignored so a bad config cannot accidentally disable the guard.
func (l *RateLimiter) WithTestSendLimit(limit int, window time.Duration) *RateLimiter {
	if limit > 0 {
		l.testLimit = limit
	}
	if window > 0 {
		l.testWindow = window
	}
	return l
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

// AllowPlatform counts one platform-only send against the shared global
// window. Admin-inbox replies have no owning shop, so they consume only the
// platform budget, not a per-shop one.
func (l *RateLimiter) AllowPlatform(ctx context.Context) error {
	if l.db == nil {
		return nil
	}

	const globalScope = "global"

	var counted int
	err := l.db.QueryRow(ctx, `
		INSERT INTO send_rate_windows (scope, window_start, count)
		VALUES ($1::text, $2::timestamptz, 1)
		ON CONFLICT (scope, window_start) DO UPDATE
			SET count = send_rate_windows.count + 1
			WHERE send_rate_windows.count < $3::int
		RETURNING 1`,
		globalScope, l.windowStart(time.Now()), l.global).
		Scan(&counted)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &RateLimitExceeded{PerShop: false}
		}
		return err
	}

	l.prune(ctx)
	return nil
}

// AllowTestSend counts one test send against a budget scoped to a single actor
// within a single shop. It is intentionally independent of Allow's customer-send
// windows: a test send can neither be charged to customer traffic nor hide in the
// shared platform budget. The scope excludes the template and automation on
// purpose, so switching either cannot reset the budget.
//
// The upsert re-checks the ceiling as it increments, so concurrent test sends
// cannot both slip past. When the ceiling is reached the conflict branch matches
// no row and no statement runs, which leaves the outcome free of any side effect
// beyond the counter.
func (l *RateLimiter) AllowTestSend(ctx context.Context, shopID, actorID uuid.UUID) error {
	if l.db == nil {
		return nil
	}

	scope := "test-send:" + shopID.String() + ":" + actorID.String()

	var counted int
	err := l.db.QueryRow(ctx, `
		INSERT INTO send_rate_windows (scope, window_start, count)
		VALUES ($1::text, $3::timestamptz, 1)
		ON CONFLICT (scope, window_start) DO UPDATE
			SET count = send_rate_windows.count + 1
			WHERE send_rate_windows.count < $2::int
		RETURNING 1`,
		scope, l.testLimit, l.testWindowStart(time.Now())).
		Scan(&counted)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &RateLimitExceeded{PerShop: true, TestSend: true}
		}
		return err
	}

	l.prune(ctx)
	return nil
}

// windowStart truncates an instant to the current fixed window. Buckets are
// aligned to the window length, so two instances with slightly different clocks
// still agree on the boundary to within a second.
func (l *RateLimiter) windowStart(now time.Time) time.Time {
	return now.Truncate(l.window).UTC()
}

// testWindowStart is windowStart for the dedicated test-send budget.
func (l *RateLimiter) testWindowStart(now time.Time) time.Time {
	return now.Truncate(l.testWindow).UTC()
}

// prune clears windows older than the current one. A failure here is harmless:
// the rows are overwritten by the next window's primary keys, so the table stays
// correct and only a little larger.
//
// The cutoff must leave the widest window (test sends use an hour, customer sends
// a minute) untouched mid-window, otherwise a long window would be pruned away
// and its counter reset early.
func (l *RateLimiter) prune(ctx context.Context) {
	l.mu.Lock()
	if time.Since(l.lastPrune) < pruneInterval {
		l.mu.Unlock()
		return
	}
	l.lastPrune = time.Now()
	l.mu.Unlock()

	cutoff := l.windowStart(time.Now()).Add(-l.window)
	if older := l.testWindowStart(time.Now()).Add(-l.testWindow); older.Before(cutoff) {
		cutoff = older
	}

	if _, err := l.db.Exec(ctx, `
		DELETE FROM send_rate_windows WHERE window_start < $1::timestamptz`,
		cutoff); err != nil && l.log != nil {
		l.log.Warn("rate limit window cleanup failed", "error", err)
	}
}
