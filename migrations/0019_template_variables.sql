-- +goose Up
-- Templates get a semantic variable map: the author composes the body with
-- friendly {{token}} chips (customer_name, order_id, ...) that are converted
-- to Meta's positional {{1..N}} at submission time. This column records the
-- token per position in placeholder order, so sends can re-fill values by
-- name instead of guessing positionally. NULL == legacy positional body.
ALTER TABLE templates ADD COLUMN variables_map JSONB NULL;

-- +goose Down
ALTER TABLE templates DROP COLUMN IF EXISTS variables_map;