-- 062_scheduled_runs_pin_claim.sql
-- Security hardening of the scheduled-run row (p2a-workflows-scheduler).
--
-- F18: a workflow-mode schedule pins the workflow version (and a sha256 of its
-- definition) the owner scheduled. ClaimDue parks the row as workflow_changed
-- when that version is gone, no longer approved, or its definition changed; the
-- owner re-confirms by editing the schedule. Existing rows are backfilled with
-- the version they would resolve to today (the owner's pin, else the latest
-- approved version), so deploying does not park them.
--
-- F242: delegated_at is when the owner last granted the standing delegation
-- (create, or extend_delegation). ClaimDue parks a row whose owner's revocation
-- epoch (user_directory.revoked_before) is later than it. Old rows take
-- created_at, matching what the actor_revoked check compares against.
--
-- F172: claimed_at is set when ClaimDue fires a row and cleared by the result
-- report; a report for a row with no outstanding claim is refused.
--
-- Idempotent: CI applies the migration set twice.

ALTER TABLE scheduled_runs
    ADD COLUMN IF NOT EXISTS workflow_version           INT,
    ADD COLUMN IF NOT EXISTS workflow_definition_sha256 TEXT,
    ADD COLUMN IF NOT EXISTS delegated_at               TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS claimed_at                 TIMESTAMPTZ;

UPDATE scheduled_runs SET delegated_at = created_at WHERE delegated_at IS NULL;

ALTER TABLE scheduled_runs
    ALTER COLUMN delegated_at SET DEFAULT NOW(),
    ALTER COLUMN delegated_at SET NOT NULL;

-- The hash matches db.WorkflowDefinitionSHA256: sha256 over the jsonb text form.
UPDATE scheduled_runs s
SET workflow_version = w.version,
    workflow_definition_sha256 = encode(sha256(convert_to(w.definition::text, 'UTF8')), 'hex')
FROM workflows w
WHERE s.workflow_lineage_id IS NOT NULL
  AND s.workflow_version IS NULL
  AND w.tenant_id = s.tenant_id
  AND w.lineage_id = s.workflow_lineage_id
  AND w.approval_state = 'approved'
  AND w.version = COALESCE(
        (SELECT p.version FROM workflow_version_pins p
          WHERE p.tenant_id = s.tenant_id AND p.user_id = s.owner_user_id
            AND p.lineage_id = s.workflow_lineage_id),
        (SELECT max(c.version) FROM workflows c
          WHERE c.tenant_id = s.tenant_id AND c.lineage_id = s.workflow_lineage_id
            AND c.approval_state = 'approved'));

INSERT INTO schema_migrations (version, description)
VALUES ('062', 'scheduled_runs: workflow version pin + definition hash, delegated_at, claimed_at')
ON CONFLICT (version) DO NOTHING;
