-- +goose Up
-- Platform role model: the operator runs the service; clients pay for it.
-- Replace owner/member with admin/client, give clients an account status and
-- let one client own many shops. Team invites are removed.

-- 1. Account status.
ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'active';

-- 2. Drop the team/member model entirely (invites + member accounts).
DELETE FROM users WHERE role = 'member';
DROP INDEX IF EXISTS users_invite_token_hash_idx;
ALTER TABLE users
    DROP COLUMN IF EXISTS invite_token_hash,
    DROP COLUMN IF EXISTS invite_expires_at;

-- 3. Roles: existing shop owners are the platform's paying clients.
UPDATE users SET role = 'client' WHERE role = 'owner';
ALTER TABLE users ALTER COLUMN role SET DEFAULT 'client';

-- 4. Shops get an owning client.
ALTER TABLE shops ADD COLUMN client_id UUID REFERENCES users(id) ON DELETE CASCADE;

UPDATE shops s SET client_id = u.id
FROM users u
WHERE u.shop_id = s.id AND u.role = 'client';

ALTER TABLE shops ALTER COLUMN client_id SET NOT NULL;
CREATE INDEX idx_shops_client_id ON shops(client_id);

-- 5. Templates become scope-based (client/global). Backfill client ownership.
ALTER TABLE templates
    ADD COLUMN scope     TEXT NOT NULL DEFAULT 'client',
    ADD COLUMN client_id UUID REFERENCES users(id) ON DELETE CASCADE;

UPDATE templates t SET client_id = s.client_id FROM shops s WHERE t.shop_id = s.id;

DROP INDEX IF EXISTS idx_templates_shop_name_language;
DROP INDEX IF EXISTS idx_templates_shop_approval;

ALTER TABLE templates DROP COLUMN shop_id;

CREATE UNIQUE INDEX idx_templates_client_name_language
    ON templates(client_id, meta_template_name, language) WHERE scope = 'client';
CREATE UNIQUE INDEX idx_templates_global_name_language
    ON templates(meta_template_name, language) WHERE scope = 'global';
CREATE INDEX idx_templates_client ON templates(client_id) WHERE scope = 'client';

-- 6. users.shop_id is gone; a client's shops are the rows that point at them.
ALTER TABLE users DROP COLUMN shop_id;

-- +goose Down
-- Best-effort reversal. Members/invites cannot be restored.
ALTER TABLE users ADD COLUMN shop_id UUID REFERENCES shops(id) ON DELETE CASCADE;
UPDATE users u SET shop_id = s.id FROM shops s WHERE s.client_id = u.id;
DELETE FROM users WHERE shop_id IS NULL;
ALTER TABLE users ALTER COLUMN shop_id SET NOT NULL;
ALTER TABLE users ALTER COLUMN role SET DEFAULT 'owner';
UPDATE users SET role = 'owner' WHERE role = 'client';

ALTER TABLE templates
    ADD COLUMN shop_id UUID REFERENCES shops(id) ON DELETE CASCADE;
UPDATE templates t SET shop_id = s.id FROM shops s WHERE s.client_id = t.client_id;
DROP INDEX IF EXISTS idx_templates_client;
DROP INDEX IF EXISTS idx_templates_global_name_language;
DROP INDEX IF EXISTS idx_templates_client_name_language;
DELETE FROM templates WHERE shop_id IS NULL;
ALTER TABLE templates ALTER COLUMN shop_id SET NOT NULL;
CREATE UNIQUE INDEX idx_templates_shop_name_language ON templates(shop_id, meta_template_name, language);
CREATE INDEX idx_templates_shop_approval ON templates(shop_id, approval_status);
ALTER TABLE templates DROP COLUMN IF EXISTS client_id, DROP COLUMN IF EXISTS scope;

DROP INDEX IF EXISTS idx_shops_client_id;
ALTER TABLE shops DROP COLUMN IF EXISTS client_id;
ALTER TABLE users DROP COLUMN IF EXISTS status;
