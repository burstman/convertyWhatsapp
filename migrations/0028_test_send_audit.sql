-- +goose Up
-- One row per authorized test-send attempt. A test send spends the platform's
-- real Meta budget and bypasses the automation eligibility checks, so every
-- attempt needs to be attributable to an actor. The recipient is stored only as
-- a keyed, shop-scoped hash: the audit proves who tested what, when, and with
-- which outcome without retaining the customer number or any message content.
CREATE TABLE IF NOT EXISTS test_send_audit (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id        UUID NOT NULL REFERENCES shops(id) ON DELETE CASCADE,
    actor_id       UUID REFERENCES users(id) ON DELETE SET NULL,
    automation_id  UUID REFERENCES automations(id) ON DELETE SET NULL,
    template_id    UUID REFERENCES templates(id) ON DELETE SET NULL,
    recipient_hash TEXT NOT NULL,
    outcome        TEXT NOT NULL CHECK (outcome IN ('accepted', 'rejected', 'failed', 'rate_limited')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The only read pattern is "recent attempts for one shop".
CREATE INDEX IF NOT EXISTS idx_test_send_audit_shop_created
    ON test_send_audit (shop_id, created_at DESC);

-- +goose Down
DROP TABLE IF EXISTS test_send_audit;
