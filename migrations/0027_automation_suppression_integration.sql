-- +goose Up
-- Which connected Converty store a held-back event came from. A suppression is
-- retried later, long after the webhook that produced it, so the integration id
-- has to survive on the row: the retry re-checks that the store is still active
-- immediately before delivery, because a deactivated or deleted store must not
-- deliver. Nullable on purpose: rows written before this migration have no id,
-- and a retry of one is refused (fail closed) rather than delivered unverified.
ALTER TABLE automation_suppressions
    ADD COLUMN IF NOT EXISTS integration_id uuid REFERENCES converty_integrations(id) ON DELETE SET NULL;

-- +goose Down
ALTER TABLE automation_suppressions DROP COLUMN IF EXISTS integration_id;
