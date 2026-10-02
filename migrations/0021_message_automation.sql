-- +goose Up
-- A message ledger answers "who did we text?" but not "which automation did
-- this?" — and the second question is the one a merchant actually asks when an
-- order status changes and nothing arrives. Linking the row to the automation
-- that fired it is what makes an automation's send history answerable.
ALTER TABLE messages ADD COLUMN IF NOT EXISTS automation_id uuid REFERENCES automations(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_messages_automation_id ON messages(automation_id, created_at DESC);

-- Backfill the rows sent before this column existed. Every automation send
-- already carried an idempotency key of conv:<shop>:<status>:<reference>, so
-- the automation can be recovered by matching the prefix. Keys that match no
-- automation (API and test sends) simply stay NULL.
UPDATE messages m
SET automation_id = a.id
FROM automations a
WHERE m.automation_id IS NULL
  AND m.idempotency_key <> ''
  AND m.shop_id = a.shop_id
  AND m.idempotency_key LIKE 'conv:'
       || a.shop_id::text || ':' || a.order_status || ':%';

-- +goose Down
DROP INDEX IF EXISTS idx_messages_automation_id;
ALTER TABLE messages DROP COLUMN IF EXISTS automation_id;