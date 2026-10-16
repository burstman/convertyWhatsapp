-- +goose Up
-- Voice notes and other media arrive as webhook inbound messages with a Meta
-- media id, not a text body. The platform downloads the bytes once and stores
-- them here so the inbox can play them back. media_bytes is NULL (and the
-- media_* text fields empty) for plain text messages and for media that is
-- still being fetched or that a purge already removed.
ALTER TABLE chat_messages
    ADD COLUMN media_kind       TEXT NOT NULL DEFAULT '',
    ADD COLUMN media_mime       TEXT NOT NULL DEFAULT '',
    ADD COLUMN media_bytes      BYTEA,
    ADD COLUMN media_duration_ms INT NOT NULL DEFAULT 0,
    ADD COLUMN media_filename   TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_chat_messages_media_age ON chat_messages(media_kind, created_at)
    WHERE media_kind <> '' AND media_bytes IS NOT NULL;

-- +goose Down
ALTER TABLE chat_messages
    DROP COLUMN media_kind,
    DROP COLUMN media_mime,
    DROP COLUMN media_bytes,
    DROP COLUMN media_duration_ms,
    DROP COLUMN media_filename;