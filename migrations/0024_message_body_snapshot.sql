-- +goose Up
-- The send history could say a message was sent and to whom, but not what it
-- said. The variable values are stored, so the text could be re-derived from
-- the template - except the template can be edited or deleted afterwards, and
-- then a history that re-renders would show a message the customer never
-- received. History is a record of what went out, so the text is written once at
-- send time alongside the row that records the send.
--
-- Nullable: rows written before this migration have no snapshot, and the
-- history page falls back to the template's variable list for those.
ALTER TABLE messages ADD COLUMN body_text text;

-- +goose Down
ALTER TABLE messages DROP COLUMN body_text;