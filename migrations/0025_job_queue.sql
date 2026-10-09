-- +goose Up
-- Deferred work lives in Postgres instead of a Redis queue. The old asynq
-- server issued a syncer heartbeat, a worker heartbeat, a delayed-task check
-- and a janitor sweep every few seconds whether or not a single job existed,
-- which is roughly 73k commands a day on an idle instance — more than a
-- 500k commands/month plan allows. The actual job volume is a few messages a
-- day, so a table plus a claim query is the whole queue.
CREATE TABLE IF NOT EXISTS job_queue (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         TEXT NOT NULL,
    shop_id      UUID NOT NULL REFERENCES shops(id) ON DELETE CASCADE,
    payload      JSONB NOT NULL,
    run_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    attempts     INTEGER NOT NULL DEFAULT 0,
    max_attempts INTEGER NOT NULL DEFAULT 5,
    lease_until  TIMESTAMPTZ,
    dedupe_key   TEXT NOT NULL DEFAULT '',
    last_error   TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The claim query is "due rows, oldest first"; everything else looks a shop up.
CREATE INDEX IF NOT EXISTS idx_job_queue_due
    ON job_queue (run_at) WHERE lease_until IS NULL;
CREATE INDEX IF NOT EXISTS idx_job_queue_shop ON job_queue (shop_id);

-- A repeated webhook for an order status that is already queued must not add a
-- second row. The message-level idempotency key would refuse the duplicate send
-- anyway; this keeps the queue from filling with work whose answer is known.
-- Entries disappear with the row once the job runs, so a later re-queue of the
-- same key is allowed again.
CREATE UNIQUE INDEX IF NOT EXISTS idx_job_queue_dedupe
    ON job_queue (kind, dedupe_key) WHERE dedupe_key <> '';

-- Fixed-window send counter that replaces the Redis INCR/EXPIRE limiter. Both
-- the per-shop and the platform window must have headroom for a send to pass.
CREATE TABLE IF NOT EXISTS send_rate_windows (
    scope        TEXT NOT NULL,
    window_start TIMESTAMPTZ NOT NULL,
    count        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, window_start)
);

CREATE INDEX IF NOT EXISTS idx_send_rate_windows_start
    ON send_rate_windows (window_start);

-- +goose Down
DROP TABLE IF EXISTS send_rate_windows;
DROP TABLE IF EXISTS job_queue;