-- 047_llm_usage_events_units.sql
-- Non-per_mtok unit-quantity extension.
--
-- llm_usage_events (migration 045) recorded only token counts, so a call
-- billed in pages/images/minutes/queries had no quantity to price from.
-- quantity/unit carry the billable amount in the pricing unit's own
-- denomination (e.g. quantity=12, unit="per_page"); both stay 0/'' for the
-- existing token-billed senders, which never populate them.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS): CI applies the migration set twice
-- to prove that.

ALTER TABLE llm_usage_events
    ADD COLUMN IF NOT EXISTS quantity DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS unit     TEXT             NOT NULL DEFAULT '';

INSERT INTO schema_migrations (version, description)
VALUES ('047', 'llm_usage_events_units: quantity/unit columns for non-per_mtok pricing')
ON CONFLICT (version) DO NOTHING;
