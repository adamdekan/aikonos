-- 064_scim_token_expiry.sql
-- F13: SCIM bearer tokens expire. A token can block every user and edit group
-- membership, so a leaked one must not work forever. New tokens live 365 days;
-- tokens that exist when this runs get 365 days from now (no lock-out on
-- upgrade). ResolveToken refuses an expired token; rotating it (Provisioning →
-- Rotate) issues a fresh one.
--
-- Idempotent: CI applies the migration set twice.

ALTER TABLE scim_tokens
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '365 days');

INSERT INTO schema_migrations (version, description)
VALUES ('064', 'scim_tokens.expires_at: SCIM bearer tokens expire (F13)')
ON CONFLICT (version) DO NOTHING;
