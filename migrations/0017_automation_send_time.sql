-- +goose Up
-- Automations can fire instantly on the event, or wait for a fixed time of day.
-- send_time is the time-of-day (HH:MM) in the merchant's chosen IANA zone;
-- NULL means send immediately. Events arriving before send_time wait until it;
-- events arriving after send_time fire right away.
ALTER TABLE automations ADD COLUMN send_time   time    NULL;
ALTER TABLE automations ADD COLUMN send_timezone text  NOT NULL DEFAULT 'UTC';

-- +goose Down
ALTER TABLE automations DROP COLUMN IF EXISTS send_timezone;
ALTER TABLE automations DROP COLUMN IF EXISTS send_time;