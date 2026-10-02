-- +goose Up
-- A scheduled send's timezone used to default to UTC, so a merchant who typed
-- "10:00" got a rule that fired at 11:00 on the clock in front of them (Tunisia
-- is UTC+1 all year). The form now offers a list defaulting to Africa/Tunis and
-- the server defaults there too; this moves the rules that were left on the old
-- UTC default so an existing automation starts meaning local time.
UPDATE automations
SET send_timezone = 'Africa/Tunis',
    updated_at = now()
WHERE send_time IS NOT NULL
  AND (send_timezone IS NULL OR send_timezone = '' OR send_timezone = 'UTC');

-- +goose Down
UPDATE automations
SET send_timezone = 'UTC',
    updated_at = now()
WHERE send_time IS NOT NULL
  AND send_timezone = 'Africa/Tunis';