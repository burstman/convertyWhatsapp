-- +goose Up
-- Automations become first-class configurable objects: a merchant names them,
-- optionally describes them, and picks a delivery rule — instant, a fixed
-- time of day on chosen weekdays, or delayed by a number of minutes. Template
-- content itself stays managed in the Templates menu (approved templates only),
-- so an automation only references it.
ALTER TABLE automations ADD COLUMN name          TEXT  NOT NULL DEFAULT '';
ALTER TABLE automations ADD COLUMN description   TEXT  NOT NULL DEFAULT '';
ALTER TABLE automations ADD COLUMN delay_minutes INT   NULL;
ALTER TABLE automations ADD COLUMN send_days     INT[] NOT NULL DEFAULT '{1,2,3,4,5,6,7}';

-- Backfill existing automation rows with a readable label derived from their
-- trigger, so the new cards render something before merchants edit them.
UPDATE automations
SET name = CASE
    WHEN event_source = 'converty' THEN
        'Order ' || order_status
    ELSE
        order_status
END;
UPDATE automations
SET send_days = '{1,2,3,4,5,6,7}'
WHERE send_days IS NULL OR send_days = '{}';

-- +goose Down
ALTER TABLE automations DROP COLUMN IF EXISTS send_days;
ALTER TABLE automations DROP COLUMN IF EXISTS delay_minutes;
ALTER TABLE automations DROP COLUMN IF EXISTS description;
ALTER TABLE automations DROP COLUMN IF EXISTS name;