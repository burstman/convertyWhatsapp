-- +goose Up
-- Member invites: the shop owner creates a member account that is activated
-- when the invitee sets a first password on the /invite/{token} page. The raw
-- token is only ever shown to the owner (one-time link); only its hash is
-- stored so a leaked database cannot redeem pending invitations.
ALTER TABLE users
    ADD COLUMN invite_token_hash TEXT,
    ADD COLUMN invite_expires_at  TIMESTAMPTZ;

CREATE UNIQUE INDEX users_invite_token_hash_idx ON users(invite_token_hash)
    WHERE invite_token_hash IS NOT NULL;

-- +goose Down
DROP INDEX IF EXISTS users_invite_token_hash_idx;
ALTER TABLE users
    DROP COLUMN IF EXISTS invite_token_hash,
    DROP COLUMN IF EXISTS invite_expires_at;