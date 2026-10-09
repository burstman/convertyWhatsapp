// Package queue is the platform's Postgres-backed job queue: the work that
// cannot happen inside the request that triggered it, namely a scheduled
// automation send and the delayed marketing-template purge.
//
// It replaces an asynq + Redis queue. Redis was the wrong shape for this app:
// asynq's server syncs, heartbeats, checks for due delayed tasks and sweeps
// expired entries every few seconds whether or not a job exists, which burns
// more commands in a month than the plan allows while the real queue traffic is
// a few messages a day. A row, a claim query and a ticker cost nothing, and a
// queued send now survives a database restart instead of depending on a cache
// that is free to evict it.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"whatsappconverty/internal/database"
)

// Task kinds. Every queued job is one of these, and the runner dispatches on
// the kind.
const (
	TaskSendWhatsAppTemplate   = "send:whatsapp_template"
	TaskPurgeMarketingTemplate = "purge:marketing_template"
)

const (
	// DefaultInterval is how often the runner looks for due jobs. Deferred sends
	// are minutes or hours out, so a coarse tick costs nothing: a job simply
	// waits for the next one.
	DefaultInterval = 10 * time.Second

	defaultBatch       = 16
	defaultLease       = 90 * time.Second
	defaultMaxAttempts = 5

	// Retry spacing for a transient failure (a Meta outage, a database blip).
	// It doubles per attempt and is capped so an outage cannot park a customer
	// message hours in the future.
	retryBase = time.Minute
	retryCap  = 30 * time.Minute

	// Beyond this many doublings the delay is already capped, so stop shifting.
	retryShiftLimit = 12

	// handlerTimeout bounds one job run. The lease outlives it, so a job that
	// overruns is retried rather than run twice at once.
	handlerTimeout = 60 * time.Second

	// bookkeepTimeout bounds the small write that records a job's outcome. It
	// is detached from the run context so a shutdown mid-job still frees or
	// reschedules the row instead of leaving it leased until it expires.
	bookkeepTimeout = 5 * time.Second
)

// Params describes one job to enqueue. Payload is stored as JSONB and handed to
// the handler byte for byte.
type Params struct {
	Kind        string
	ShopID      uuid.UUID
	Payload     []byte
	RunAt       time.Time
	DedupeKey   string
	MaxAttempts int
}

// Enqueue stores a job for the runner. A zero RunAt means "at the next tick".
//
// DedupeKey is optional: when set, a second job of the same kind and key is
// silently dropped. Callers use it for the idempotency key of the work itself,
// so a webhook that arrives twice cannot queue the same send twice.
func Enqueue(ctx context.Context, db database.Querier, p Params) error {
	if p.Kind == "" {
		return errors.New("queue: job kind is required")
	}
	if p.ShopID == uuid.Nil {
		return errors.New("queue: job shop is required")
	}
	if len(p.Payload) == 0 {
		return errors.New("queue: job payload is required")
	}
	if db == nil {
		return errors.New("queue: no database handle")
	}

	attempts := p.MaxAttempts
	if attempts <= 0 {
		attempts = defaultMaxAttempts
	}

	// A job with no moment is due at the database's now(), not the app host's:
	// the claim query compares against the database clock, so taking it from Go
	// would make a due job invisible to its own reader for as long as the two
	// clocks disagree.
	var runAt *time.Time
	if !p.RunAt.IsZero() {
		at := p.RunAt.UTC()
		runAt = &at
	}

	// ON CONFLICT DO NOTHING covers both the id and the dedupe index. Inserting
	// a job that is already queued is not an error the caller needs to hear
	// about; the queued copy will do the work.
	_, err := db.Exec(ctx, `
		INSERT INTO job_queue (kind, shop_id, payload, run_at, max_attempts, dedupe_key)
		VALUES ($1, $2, $3, COALESCE($4::timestamptz, now()), $5, $6)
		ON CONFLICT DO NOTHING`,
		p.Kind, p.ShopID, p.Payload, runAt, attempts, p.DedupeKey)
	if err != nil {
		return fmt.Errorf("queue: enqueue %s: %w", p.Kind, err)
	}
	return nil
}

// Handler runs one job. Returning an error retries the job with a backoff until
// it has used up its attempts; wrap it in Permanent for a failure that retrying
// cannot fix, such as a message the send gate refused.
type Handler func(ctx context.Context, payload []byte) error

// Job is a claimed row handed to a handler. Attempts counts the current run, so
// a handler that fails on its first try sees Attempts == 1.
type Job struct {
	ID          uuid.UUID
	Kind        string
	ShopID      uuid.UUID
	Payload     []byte
	Attempts    int
	MaxAttempts int
}

// permanentError marks a job failure as final.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// Permanent marks an error as not worth retrying. The runner drops the job
// after logging it instead of spending attempts on it.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// IsPermanent reports whether err (or anything it wraps) was marked Permanent.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// Runner drains the queue in the background. It is safe to run several
// instances against one database: claims are leased, so two runners never take
// the same row.
type Runner struct {
	db       database.Querier
	log      *slog.Logger
	interval time.Duration
	batch    int
	lease    time.Duration

	mu       sync.RWMutex
	handlers map[string]Handler
}

// NewRunner builds a runner over db. Handlers must be registered with Handle
// before Start.
func NewRunner(db database.Querier, log *slog.Logger) *Runner {
	return &Runner{
		db:       db,
		log:      log,
		interval: DefaultInterval,
		batch:    defaultBatch,
		lease:    defaultLease,
		handlers: make(map[string]Handler),
	}
}

// Handle registers the handler for a task kind.
func (r *Runner) Handle(kind string, h Handler) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handlers[kind] = h
}

// Start drains the queue on a ticker until ctx is cancelled. It returns
// immediately; the loop runs in its own goroutine.
func (r *Runner) Start(ctx context.Context) {
	go r.loop(ctx)
}

func (r *Runner) loop(ctx context.Context) {
	// Drain once at boot: a job whose moment came while the previous instance
	// was down should go out seconds after the new one starts, not on the first
	// tick after it.
	r.drainLogged(ctx)

	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.drainLogged(ctx)
		}
	}
}

func (r *Runner) drainLogged(ctx context.Context) {
	if err := r.Drain(ctx); err != nil && ctx.Err() == nil {
		r.log.Warn("job queue drain failed", "error", err)
	}
}

// Drain claims and runs one batch of due jobs, waiting for that batch to finish.
// It is what the ticker calls, and is exported so a single pass can be run
// directly. Claims are leased, so two runners draining at once never take the
// same row.
func (r *Runner) Drain(ctx context.Context) error {
	jobs, err := r.claim(ctx, r.batch)
	if err != nil {
		return err
	}
	if len(jobs) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	for _, job := range jobs {
		wg.Add(1)
		go func(j Job) {
			defer wg.Done()
			r.runOne(ctx, j)
		}(job)
	}
	wg.Wait()
	return nil
}

func (r *Runner) runOne(ctx context.Context, job Job) {
	handler := r.handler(job.Kind)
	if handler == nil {
		r.log.Warn("job dropped: no handler registered",
			"job_id", job.ID, "kind", job.Kind, "shop_id", job.ShopID)
		r.drop(ctx, job)
		return
	}

	runCtx, cancel := context.WithTimeout(ctx, handlerTimeout)
	defer cancel()

	switch action := decide(handler(runCtx, job.Payload), job); action.kind {
	case actionComplete:
		r.complete(ctx, job)
	case actionDrop:
		// A send the gate refused, or a job that has spent every attempt: either
		// way another run would not change the answer, so the row goes away and
		// the reason stays in the log. The message row keeps the failure too.
		r.log.Error("job failed permanently",
			"job_id", job.ID, "kind", job.Kind, "shop_id", job.ShopID,
			"attempts", job.Attempts, "error", action.cause)
		r.drop(ctx, job)
	case actionRetry:
		r.log.Warn("job failed, retrying",
			"job_id", job.ID, "kind", job.Kind, "shop_id", job.ShopID,
			"attempt", job.Attempts, "retry_in", action.retryIn, "error", action.cause)
		r.retry(ctx, job, action.retryIn, action.cause)
	}
}

// action is what the runner does with a job once its handler has returned.
type action struct {
	kind    actionKind
	cause   error
	retryIn time.Duration
}

type actionKind int

const (
	actionComplete actionKind = iota
	actionDrop
	actionRetry
)

// decide turns a handler result into the job's fate, which is the whole retry
// policy in one place: a success finishes the job, a failure the handler called
// permanent (or one that has used every attempt) is dropped with a log line, and
// anything else waits out a backoff and tries again.
func decide(err error, job Job) action {
	if err == nil {
		return action{kind: actionComplete}
	}
	if IsPermanent(err) || job.Attempts >= job.MaxAttempts {
		return action{kind: actionDrop, cause: err}
	}
	return action{kind: actionRetry, cause: err, retryIn: backoff(job.Attempts)}
}

// claim leases up to limit due jobs. The lease is what makes the queue safe with
// more than one runner: a row stays invisible until it is done or its lease
// expires, so a crash mid-job releases it instead of stranding it forever.
func (r *Runner) claim(ctx context.Context, limit int) ([]Job, error) {
	rows, err := r.db.Query(ctx, `
		UPDATE job_queue SET
			lease_until = now() + make_interval(secs => $1::double precision),
			attempts    = attempts + 1,
			updated_at  = now()
		WHERE id IN (
			SELECT id FROM job_queue
			WHERE lease_until IS NULL AND run_at <= now()
			ORDER BY run_at
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, kind, shop_id, payload, attempts, max_attempts`,
		r.lease.Seconds(), limit)
	if err != nil {
		return nil, fmt.Errorf("queue: claim: %w", err)
	}
	defer rows.Close()

	var jobs []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Kind, &j.ShopID, &j.Payload, &j.Attempts, &j.MaxAttempts); err != nil {
			return nil, fmt.Errorf("queue: scan claim: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: claim rows: %w", err)
	}
	return jobs, nil
}

func (r *Runner) complete(ctx context.Context, job Job) {
	if err := r.delete(job, ctx); err != nil {
		r.log.Error("job finished but its row could not be removed; it will run again",
			"job_id", job.ID, "kind", job.Kind, "error", err)
	}
}

func (r *Runner) drop(ctx context.Context, job Job) { r.delete(job, ctx) }

func (r *Runner) delete(job Job, ctx context.Context) error {
	bookkeep, cancel := r.bookkeep(ctx)
	defer cancel()
	_, err := r.db.Exec(bookkeep, `DELETE FROM job_queue WHERE id = $1`, job.ID)
	return err
}

func (r *Runner) retry(ctx context.Context, job Job, delay time.Duration, cause error) {
	bookkeep, cancel := r.bookkeep(ctx)
	defer cancel()
	_, err := r.db.Exec(bookkeep, `
		UPDATE job_queue
		SET lease_until = NULL,
		    run_at      = now() + make_interval(secs => $3::double precision),
		    last_error  = $2,
		    updated_at  = now()
		WHERE id = $1`,
		job.ID, cause.Error(), delay.Seconds())
	if err != nil {
		// The row keeps its lease, so it will be retried once the lease expires.
		r.log.Error("job retry could not be scheduled; it will be picked up when the lease expires",
			"job_id", job.ID, "kind", job.Kind, "error", err)
	}
}

func (r *Runner) handler(kind string) Handler {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.handlers[kind]
}

// bookkeep gives the small write that records a job's outcome its own short
// context, so that fate is still recorded when the process is being asked to
// stop mid-run.
func (r *Runner) bookkeep(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), bookkeepTimeout)
}

// backoff spaces retries out so an outage does not become a burst. Attempts is
// the count including the run that just failed.
func backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	if attempts > retryShiftLimit {
		return retryCap
	}
	return min(retryBase<<(attempts-1), retryCap)
}
