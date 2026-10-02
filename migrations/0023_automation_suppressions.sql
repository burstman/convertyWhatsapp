-- +goose Up
-- A semantic template whose placed variable resolves empty is deliberately not
-- sent, so the customer never receives a message with a blank line where the
-- order detail should be. That silence was total: the event matched, the send was
-- dropped with a log line, and the merchant's history said only "no sends yet".
-- One suppressed row per dropped event makes the miss explainable in the UI and
-- gives the "send now" action something to retry.
CREATE TABLE automation_suppressions (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    shop_id           uuid NOT NULL REFERENCES shops(id) ON DELETE CASCADE,
    automation_id     uuid NOT NULL REFERENCES automations(id) ON DELETE CASCADE,
    customer_id       uuid REFERENCES customers(id) ON DELETE SET NULL,
    template_id       uuid REFERENCES templates(id) ON DELETE SET NULL,
    reason            text NOT NULL,
    missing_variables jsonb NOT NULL DEFAULT '[]'::jsonb,
    trigger_label     text,
    tracking_code     text,
    order_id          text,
    idempotency_key   text,
    created_at        timestamptz NOT NULL DEFAULT now()
);

-- The history page reads one automation newest-first.
CREATE INDEX automation_suppressions_automation_created_idx
    ON automation_suppressions (automation_id, created_at DESC);

-- One suppressed row per dropped event: a retry reuses this key, so a message
-- that did go out earlier can never be sent twice.
CREATE UNIQUE INDEX automation_suppressions_event_idx
    ON automation_suppressions (automation_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS automation_suppressions;