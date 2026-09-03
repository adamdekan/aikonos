-- 026_workflow_approval.sql
-- Adds the approval-state model to workflows. A version may be 'approved'
-- (immediately active — all existing rows and owner saves), 'proposed' (awaiting
-- owner review — created by the self-improvement path on a negative signal), or
-- 'rejected' (owner declined). GetCurrent and ResolveVersionForUser filter to
-- 'approved' only, so proposed/rejected versions are never silently current.
--
-- DEFAULT 'approved' keeps every existing row and every owner-authored save active
-- without any back-fill.

ALTER TABLE workflows ADD COLUMN IF NOT EXISTS approval_state TEXT NOT NULL DEFAULT 'approved'
    CHECK (approval_state IN ('approved','proposed','rejected'));
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS approved_at TIMESTAMPTZ;
ALTER TABLE workflows ADD COLUMN IF NOT EXISTS approval_reason TEXT NOT NULL DEFAULT '';

INSERT INTO schema_migrations (version, description)
VALUES ('026', 'workflows: approval-state model (proposed/approved/rejected)')
ON CONFLICT (version) DO NOTHING;
