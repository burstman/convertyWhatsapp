-- +goose Up
INSERT INTO app_settings (key, value) VALUES
  ('meta.last_verified_at', ''),
  ('meta.last_verified_status', ''),
  ('meta.last_verified_details', '')
ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM app_settings WHERE key IN ('meta.last_verified_at', 'meta.last_verified_status', 'meta.last_verified_details');
