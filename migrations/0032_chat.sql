-- +goose Up
-- Platform inbox: a conversation is a customer who messaged the platform's
-- single WhatsApp number, and chat_messages is the thread under it. This is a
-- separate ledger from `messages` (template/automation sends): a reply is a
-- free-form customer-service message, not a template send, and inbound
-- messages have no template at all.
--
-- Unlike the merchant-era schema this is platform-scoped: the admin console
-- owns the one WABA, so conversations are keyed by phone alone (no shop_id).
CREATE TABLE conversations (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_phone       TEXT NOT NULL UNIQUE,
    customer_name        TEXT NOT NULL DEFAULT '',
    last_message_direction TEXT NOT NULL DEFAULT 'inbound',
    last_message_body    TEXT NOT NULL DEFAULT '',
    last_message_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    unread_count         INT NOT NULL DEFAULT 0,
    -- Free-form replies are allowed only while the customer service window is
    -- open: 24h after the customer's last inbound message.
    window_expires_at    TIMESTAMPTZ NOT NULL DEFAULT to_timestamp(0),
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_conversations_last ON conversations(last_message_at DESC);

CREATE TABLE chat_messages (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id   UUID NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
    direction         TEXT NOT NULL, -- inbound | outbound
    body              TEXT NOT NULL DEFAULT '',
    meta_message_id   TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'queued', -- queued|sent|delivered|read|failed
    error_message     TEXT NOT NULL DEFAULT '',
    sent_at           TIMESTAMPTZ,
    delivered_at      TIMESTAMPTZ,
    read_at           TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_chat_messages_conversation ON chat_messages(conversation_id, created_at);
CREATE INDEX idx_chat_messages_meta ON chat_messages(meta_message_id);

-- +goose Down
DROP TABLE chat_messages;
DROP TABLE conversations;
