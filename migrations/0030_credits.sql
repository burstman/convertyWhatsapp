-- +goose Up
-- Prepaid message credit, billed per client (across all of the client's shops).
-- Money is stored in micro-dollars (1e-6 USD) as BIGINT to avoid floats.

-- Platform-editable settings (price, etc.). Values are text; callers parse.
CREATE TABLE app_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO app_settings (key, value) VALUES ('price_per_message_micros', '30000')
ON CONFLICT (key) DO NOTHING;

-- One prepaid balance per client.
CREATE TABLE client_credits (
    client_id      UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    balance_micros BIGINT NOT NULL DEFAULT 0,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Append-only ledger: every top-up, adjustment and per-message charge.
CREATE TABLE credit_ledger (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    client_id            UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    delta_micros         BIGINT NOT NULL,
    balance_after_micros BIGINT NOT NULL,
    reason               TEXT NOT NULL,
    message_id           UUID,
    note                 TEXT NOT NULL DEFAULT '',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_credit_ledger_client ON credit_ledger(client_id, created_at DESC);

-- Messages record which client paid and the price charged, so usage and revenue
-- are reconstructable without re-deriving ownership later.
ALTER TABLE messages
    ADD COLUMN client_id    UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN price_micros BIGINT NOT NULL DEFAULT 0;

UPDATE messages m SET client_id = s.client_id FROM shops s WHERE m.shop_id = s.id;

CREATE INDEX idx_messages_client_id ON messages(client_id);

-- +goose Down
DROP INDEX IF EXISTS idx_messages_client_id;
ALTER TABLE messages
    DROP COLUMN IF EXISTS price_micros,
    DROP COLUMN IF EXISTS client_id;
DROP TABLE IF EXISTS credit_ledger;
DROP TABLE IF EXISTS client_credits;
DROP TABLE IF EXISTS app_settings;
