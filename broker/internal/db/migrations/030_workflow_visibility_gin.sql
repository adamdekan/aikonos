-- F19: GIN index matching the ?| (overlap) predicate in
-- WorkflowRepo.ListVisibleShared (broker/internal/db/workflows.go), which is
-- exercised on every ListWorkflows call for a viewer whose groups resolve to
-- a non-empty set:
--
--   SELECT DISTINCT ON (lineage_id) ...
--   FROM workflows
--   WHERE status            = 'published'
--     AND approval_state    = 'approved'
--     AND visibility_groups ?| array(SELECT jsonb_array_elements_text($1::jsonb))
--   ORDER BY lineage_id, version DESC
--
-- jsonb_path_ops does not support ?|; the default GIN opclass does.
CREATE INDEX IF NOT EXISTS idx_workflows_visibility_groups_gin
    ON workflows USING GIN (visibility_groups);
INSERT INTO schema_migrations (version, description)
VALUES ('030', 'workflows: GIN index on visibility_groups for the ?| overlap predicate')
ON CONFLICT (version) DO NOTHING;
