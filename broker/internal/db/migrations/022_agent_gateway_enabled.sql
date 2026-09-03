ALTER TABLE agents ADD COLUMN IF NOT EXISTS gateway_enabled BOOLEAN NOT NULL DEFAULT false;

INSERT INTO schema_migrations (version, description)
    VALUES ('022', 'agents: gateway_enabled flag')
    ON CONFLICT (version) DO NOTHING;
