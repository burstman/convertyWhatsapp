package whatsapp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// countingDB is an in-memory stand-in for send_rate_windows. It reproduces the
// single property the real upsert guarantees for the test-send budget: an
// increment only happens while the window is under the limit, atomically.
type countingDB struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingDB() *countingDB {
	return &countingDB{counts: map[string]int{}}
}

func (d *countingDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	var tag pgconn.CommandTag
	return tag, nil
}

func (d *countingDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected Query")
}

func (d *countingDB) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("unexpected Begin")
}

func (d *countingDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	scope, _ := args[0].(string)
	limit, _ := args[1].(int)
	window, ok := args[2].(time.Time)
	if !ok {
		return countingRow{err: errors.New("unexpected window argument")}
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	key := scope + "|" + window.Format(time.RFC3339Nano)
	if d.counts[key] >= limit {
		return countingRow{err: pgx.ErrNoRows}
	}
	d.counts[key]++
	return countingRow{counted: 1}
}

type countingRow struct {
	counted int
	err     error
}

func (r countingRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) == 1 {
		if p, ok := dest[0].(*int); ok {
			*p = r.counted
		}
	}
	return nil
}

// The budget is per actor within a shop: exhausting one does not consume
// another's, and the same actor id in a different shop gets a fresh budget.
func TestAllowTestSendIsScopedPerActorAndShop(t *testing.T) {
	lim := NewRateLimiter(newCountingDB(), nil).WithTestSendLimit(2, time.Hour)
	ctx := context.Background()
	shop, otherShop := uuid.New(), uuid.New()
	alice, bob := uuid.New(), uuid.New()

	for i := 0; i < 2; i++ {
		if err := lim.AllowTestSend(ctx, shop, alice); err != nil {
			t.Fatalf("call %d should be allowed: %v", i+1, err)
		}
	}

	err := lim.AllowTestSend(ctx, shop, alice)
	var limited *RateLimitExceeded
	if !errors.As(err, &limited) || !limited.TestSend {
		t.Fatalf("third call for the same actor/shop: got %v, want test-send RateLimitExceeded", err)
	}

	if err := lim.AllowTestSend(ctx, shop, bob); err != nil {
		t.Fatalf("a different actor must not inherit the limit: %v", err)
	}
	if err := lim.AllowTestSend(ctx, otherShop, alice); err != nil {
		t.Fatalf("a different shop must not inherit the limit: %v", err)
	}
}

// Concurrent test sends must not all slip past the ceiling.
func TestAllowTestSendConcurrentCallsCannotExceedLimit(t *testing.T) {
	const limit = 5
	const calls = 50

	lim := NewRateLimiter(newCountingDB(), nil).WithTestSendLimit(limit, time.Hour)
	shop, actor := uuid.New(), uuid.New()

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed, limited := 0, 0
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := lim.AllowTestSend(context.Background(), shop, actor)
			mu.Lock()
			defer mu.Unlock()
			var rl *RateLimitExceeded
			switch {
			case err == nil:
				allowed++
			case errors.As(err, &rl) && rl.TestSend:
				limited++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if allowed != limit {
		t.Fatalf("allowed = %d, want %d", allowed, limit)
	}
	if limited != calls-limit {
		t.Fatalf("limited = %d, want %d", limited, calls-limit)
	}
}

// The budget is independent of the template and automation: the scope carries
// neither, so switching either cannot reset it. Two different automation ids
// therefore share one budget.
func TestAllowTestSendScopeIgnoresTemplateAndAutomation(t *testing.T) {
	db := newCountingDB()
	lim := NewRateLimiter(db, nil).WithTestSendLimit(1, time.Hour)
	ctx := context.Background()
	shop, actor := uuid.New(), uuid.New()

	if err := lim.AllowTestSend(ctx, shop, actor); err != nil {
		t.Fatalf("first send should be allowed: %v", err)
	}
	// Any other template/automation still maps to the same scope key.
	// AllowTestSend takes no template or automation, so the second call is the
	// same key by construction; assert it is refused.
	var limited *RateLimitExceeded
	if err := lim.AllowTestSend(ctx, shop, actor); !errors.As(err, &limited) || !limited.TestSend {
		t.Fatalf("second send: got %v, want test-send RateLimitExceeded", err)
	}
}

// A nil handle is a no-op limiter (local runs, unit tests).
func TestAllowTestSendNilDBIsNoop(t *testing.T) {
	lim := NewRateLimiter(nil, nil)
	for i := 0; i < 5; i++ {
		if err := lim.AllowTestSend(context.Background(), uuid.New(), uuid.New()); err != nil {
			t.Fatalf("nil db must allow: %v", err)
		}
	}
}
