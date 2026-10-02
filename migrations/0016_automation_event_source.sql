-- +goose Up
-- Automations trigger on one event source today: Converty order events (the
-- original order_status semantics). The source is stored so the same status
-- string can later be automated for other origins without colliding: the
-- trigger key stays in order_status and event_source disambiguates the origin.
-- Existing rows default to 'converty', preserving their current meaning.
ALTER TABLE automations ADD COLUMN IF NOT EXISTS event_source TEXT NOT NULL DEFAULT 'converty';
ALTER TABLE automations DROP CONSTRAINT IF EXISTS automations_shop_id_order_status_key;
ALTER TABLE automations
    ADD CONSTRAINT automations_shop_source_status_key UNIQUE (shop_id, event_source, order_status);

-- +goose Down
ALTER TABLE automations DROP CONSTRAINT IF EXISTS automations_shop_source_status_key;
ALTER TABLE automations ADD CONSTRAINT automations_shop_id_order_status_key UNIQUE (shop_id, order_status);
ALTER TABLE automations DROP COLUMN IF EXISTS event_source;