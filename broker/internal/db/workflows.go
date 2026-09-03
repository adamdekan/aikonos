// broker/internal/db/workflows.go
// Repository for versioned workflows and per-user version pins.
// All queries set app.current_tenant for RLS (withTenant). No raw SQL outside
// this package. definition is stored/retrieved as json.RawMessage — the workflow
// package is never imported here so persistence stays decoupled from the model.
package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// WorkflowRow is one immutable version of a workflow as stored in Postgres.
// definition is the raw JSONB blob (CP1 ToJSON output); callers unmarshal it into
// their domain type — this layer stays deliberately schema-agnostic.
type WorkflowRow struct {
	ID               uuid.UUID
	LineageID        uuid.UUID
	Version          int
	TenantID         uuid.UUID
	OwnerUserID      string
	Name             string
	Description      string
	Status           string
	VisibilityKind   string
	VisibilityGroups json.RawMessage
	Definition       json.RawMessage
	ParentLineageID  *uuid.UUID
	CreatedAt        time.Time
	// Approval-state model (migration 026). Owner saves default to 'approved';
	// self-improvement proposals arrive as 'proposed' and require explicit approval.
	ApprovalState  string     // 'approved' | 'proposed' | 'rejected'
	ApprovedAt     *time.Time // set when ApprovalState transitions to 'approved'
	ApprovalReason string     // populated on rejection
	// SuccessRatedAt is set by RateWorkflowRun(SUCCESS) (migration 027).
	// Informational only: the publish gate on it was removed 2026-07-21 (see
	//  change log) — no code reads it as a gate.
	SuccessRatedAt *time.Time
	// BoundAgentID is the agent the lineage is bound to (F9, migration 038);
	// nil = personal workflow. Lineage-immutable: only a brand-new lineage takes
	// it from the request — edits/proposals/forks inherit it.
	BoundAgentID *uuid.UUID
}

// WorkflowRepo persists workflows and version pins. Every method acquires a
// connection from pool and calls withTenant before any query.
type WorkflowRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

// NewWorkflowRepo constructs the repo.
func NewWorkflowRepo(pool *pgxpool.Pool, logger *zap.Logger) *WorkflowRepo {
	return &WorkflowRepo{pool: pool, logger: logger}
}

const workflowCols = `id, lineage_id, version, tenant_id, owner_user_id, name,
	description, status, visibility_kind, visibility_groups, definition,
	parent_lineage_id, created_at, approval_state, approved_at, approval_reason,
	success_rated_at, bound_agent_id`

func scanWorkflow(row pgx.Row) (*WorkflowRow, error) {
	var r WorkflowRow
	var visGroups, def []byte
	if err := row.Scan(
		&r.ID, &r.LineageID, &r.Version, &r.TenantID, &r.OwnerUserID, &r.Name,
		&r.Description, &r.Status, &r.VisibilityKind, &visGroups, &def,
		&r.ParentLineageID, &r.CreatedAt,
		&r.ApprovalState, &r.ApprovedAt, &r.ApprovalReason,
		&r.SuccessRatedAt, &r.BoundAgentID,
	); err != nil {
		return nil, err
	}
	r.VisibilityGroups = json.RawMessage(visGroups)
	r.Definition = json.RawMessage(def)
	return &r, nil
}

// CreateVersion inserts a new immutable version for the lineage. version is set
// to max(existing versions)+1 for the lineage, or 1 for a brand-new lineage.
// The caller supplies a fresh lineage_id when creating the first version.
//
// The MAX(version) read and the INSERT run inside a single transaction so
// concurrent creates for the same lineage cannot race on the UNIQUE constraint.
func (r *WorkflowRepo) CreateVersion(ctx context.Context, tenant string, row WorkflowRow) (WorkflowRow, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (WorkflowRow, error) {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return WorkflowRow{}, fmt.Errorf("begin tx: %w", err)
		}
		defer tx.Rollback(ctx)

		// Determine next version inside the transaction — MAX(version)+1 or 1 if new.
		var nextVersion int
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(version), 0) + 1 FROM workflows WHERE lineage_id = $1`,
			row.LineageID,
		).Scan(&nextVersion); err != nil {
			return WorkflowRow{}, fmt.Errorf("compute next version: %w", err)
		}

		// Owner-authored saves are immediately active; default to 'approved' when the
		// caller leaves ApprovalState empty (the self-improvement path sets 'proposed').
		approvalState := row.ApprovalState
		if approvalState == "" {
			approvalState = "approved"
		}

		out, scanErr := scanWorkflow(tx.QueryRow(ctx, `
		INSERT INTO workflows
		    (lineage_id, version, tenant_id, owner_user_id, name, description,
		     status, visibility_kind, visibility_groups, definition, parent_lineage_id,
		     approval_state, bound_agent_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		RETURNING `+workflowCols,
			row.LineageID, nextVersion, row.TenantID, row.OwnerUserID, row.Name, row.Description,
			row.Status, row.VisibilityKind, []byte(row.VisibilityGroups), []byte(row.Definition),
			row.ParentLineageID, approvalState, row.BoundAgentID,
		))
		if scanErr != nil {
			return WorkflowRow{}, fmt.Errorf("insert workflow version: %w", scanErr)
		}
		if err := tx.Commit(ctx); err != nil {
			return WorkflowRow{}, fmt.Errorf("commit workflow version: %w", err)
		}

		r.logger.Debug("workflow version created",
			zap.String("lineage_id", row.LineageID.String()),
			zap.Int("version", nextVersion),
		)
		return *out, nil
	})
}

// GetCurrent returns the highest-version row for the lineage.
func (r *WorkflowRepo) GetCurrent(ctx context.Context, tenant string, lineageID uuid.UUID) (*WorkflowRow, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (*WorkflowRow, error) {
		row := conn.QueryRow(ctx,
			`SELECT `+workflowCols+` FROM workflows WHERE lineage_id = $1 AND approval_state = 'approved' ORDER BY version DESC LIMIT 1`,
			lineageID,
		)
		w, err := scanWorkflow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("workflow lineage %s not found", lineageID)
		}
		return w, err
	})
}

// GetVersion returns a specific version of a workflow lineage.
func (r *WorkflowRepo) GetVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int) (*WorkflowRow, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (*WorkflowRow, error) {
		row := conn.QueryRow(ctx,
			`SELECT `+workflowCols+` FROM workflows WHERE lineage_id = $1 AND version = $2`,
			lineageID, version,
		)
		w, err := scanWorkflow(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("workflow lineage %s version %d not found", lineageID, version)
		}
		return w, err
	})
}

// ListVersions returns versions of a lineage, newest first. beforeVersion>0
// keyset-filters to versions strictly below it (the pagination cursor); limit>0
// caps the row count. beforeVersion<=0 and limit<=0 reproduce the legacy
// unbounded query (all versions).
func (r *WorkflowRepo) ListVersions(ctx context.Context, tenant string, lineageID uuid.UUID, beforeVersion, limit int) ([]*WorkflowRow, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) ([]*WorkflowRow, error) {
		q := `SELECT ` + workflowCols + ` FROM workflows WHERE lineage_id = $1`
		args := []any{lineageID}
		if beforeVersion > 0 {
			args = append(args, beforeVersion)
			q += fmt.Sprintf(" AND version < $%d", len(args))
		}
		q += " ORDER BY version DESC"
		if limit > 0 {
			args = append(args, limit)
			q += fmt.Sprintf(" LIMIT $%d", len(args))
		}
		rows, err := conn.Query(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("list workflow versions: %w", err)
		}
		defer rows.Close()
		return collectWorkflows(rows)
	})
}

// ListByOwner returns the highest approved version per lineage owned by
// ownerUserID within the tenant. Used for the library view. Proposed/rejected
// versions are reachable via ListVersions but must not be the representative row.
// afterLineage keyset-filters to lineages ordered after it (the pagination
// cursor, a canonical uuid string whose lexical order matches Postgres uuid
// order); limit>0 caps the number of distinct lineages returned. An empty
// cursor and limit<=0 reproduce the legacy unbounded query.
func (r *WorkflowRepo) ListByOwner(ctx context.Context, tenant, ownerUserID, afterLineage string, limit int) ([]*WorkflowRow, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) ([]*WorkflowRow, error) {
		// DISTINCT ON keeps only the highest approved version per lineage (ORDER BY
		// ensures the first row seen is the max approved version). LIMIT applies
		// after the collapse, so it caps distinct lineages, not raw versions.
		q := `
		SELECT DISTINCT ON (lineage_id) ` + workflowCols + `
		FROM workflows
		WHERE owner_user_id = $1 AND approval_state = 'approved'`
		args := []any{ownerUserID}
		if afterLineage != "" {
			args = append(args, afterLineage)
			q += fmt.Sprintf(" AND lineage_id > $%d::uuid", len(args))
		}
		q += " ORDER BY lineage_id, version DESC"
		if limit > 0 {
			args = append(args, limit)
			q += fmt.Sprintf(" LIMIT $%d", len(args))
		}
		rows, err := conn.Query(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("list workflows by owner: %w", err)
		}
		defer rows.Close()
		return collectWorkflows(rows)
	})
}

// ProposeVersion inserts a new version with approval_state='proposed'. Uses the
// same MAX(version)+1-in-transaction pattern as CreateVersion so concurrent
// proposals for the same lineage cannot race on the UNIQUE constraint.
func (r *WorkflowRepo) ProposeVersion(ctx context.Context, tenant string, row WorkflowRow) (WorkflowRow, error) {
	row.ApprovalState = "proposed"
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (WorkflowRow, error) {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return WorkflowRow{}, fmt.Errorf("begin tx: %w", err)
		}
		defer tx.Rollback(ctx)

		var nextVersion int
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(version), 0) + 1 FROM workflows WHERE lineage_id = $1`,
			row.LineageID,
		).Scan(&nextVersion); err != nil {
			return WorkflowRow{}, fmt.Errorf("compute next version: %w", err)
		}

		out, scanErr := scanWorkflow(tx.QueryRow(ctx, `
		INSERT INTO workflows
		    (lineage_id, version, tenant_id, owner_user_id, name, description,
		     status, visibility_kind, visibility_groups, definition, parent_lineage_id,
		     approval_state, bound_agent_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'proposed', $12)
		RETURNING `+workflowCols,
			row.LineageID, nextVersion, row.TenantID, row.OwnerUserID, row.Name, row.Description,
			row.Status, row.VisibilityKind, []byte(row.VisibilityGroups), []byte(row.Definition),
			row.ParentLineageID, row.BoundAgentID,
		))
		if scanErr != nil {
			return WorkflowRow{}, fmt.Errorf("insert proposed workflow version: %w", scanErr)
		}
		if err := tx.Commit(ctx); err != nil {
			return WorkflowRow{}, fmt.Errorf("commit proposed workflow version: %w", err)
		}

		r.logger.Debug("workflow version proposed",
			zap.String("lineage_id", row.LineageID.String()),
			zap.Int("version", nextVersion),
		)
		return *out, nil
	})
}

// ApproveVersion transitions a 'proposed' version to 'approved'. Returns an
// error when no proposed row exists for (lineage, version) — callers must check
// the state before calling.
func (r *WorkflowRepo) ApproveVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx, `
		UPDATE workflows
		SET approval_state = 'approved', approved_at = NOW()
		WHERE lineage_id = $1 AND version = $2 AND approval_state = 'proposed'`,
			lineageID, version,
		)
		if err != nil {
			return fmt.Errorf("approve workflow version: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("workflow lineage %s version %d not found or not in proposed state", lineageID, version)
		}
		return nil
	})
}

// RejectVersion transitions a 'proposed' version to 'rejected', recording the
// owner's reason. Returns an error when no proposed row exists.
func (r *WorkflowRepo) RejectVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int, reason string) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx, `
		UPDATE workflows
		SET approval_state = 'rejected', approval_reason = $3
		WHERE lineage_id = $1 AND version = $2 AND approval_state = 'proposed'`,
			lineageID, version, reason,
		)
		if err != nil {
			return fmt.Errorf("reject workflow version: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("workflow lineage %s version %d not found or not in proposed state", lineageID, version)
		}
		return nil
	})
}

// ListProposals returns all versions with approval_state='proposed' owned by
// ownerUserID within the tenant. This is the owner's review queue.
func (r *WorkflowRepo) ListProposals(ctx context.Context, tenant, ownerUserID string) ([]*WorkflowRow, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) ([]*WorkflowRow, error) {
		rows, err := conn.Query(ctx, `
		SELECT `+workflowCols+`
		FROM workflows
		WHERE owner_user_id = $1 AND approval_state = 'proposed'
		ORDER BY created_at DESC`,
			ownerUserID,
		)
		if err != nil {
			return nil, fmt.Errorf("list proposals: %w", err)
		}
		defer rows.Close()
		return collectWorkflows(rows)
	})
}

func collectWorkflows(rows pgx.Rows) ([]*WorkflowRow, error) {
	var out []*WorkflowRow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListVisibleShared returns the highest approved version per lineage for
// published workflows whose visibility_groups JSONB array overlaps any of
// the given groups. Used by CP8 to surface shared workflows to group members.
//
// groups must not be empty; callers guard before calling (empty groups → no
// shared workflows are visible, consistent with the FGA-off allow-all posture).
// afterLineage/limit bound the fetch identically to ListByOwner (keyset on the
// canonical-uuid cursor + LIMIT on distinct lineages); empty cursor and limit<=0
// reproduce the legacy unbounded query.
func (r *WorkflowRepo) ListVisibleShared(ctx context.Context, tenant string, groups []string, afterLineage string, limit int) ([]*WorkflowRow, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) ([]*WorkflowRow, error) {
		// Build a JSON array of group IDs for the ?| (overlap) operator so the
		// query is a single parameterised statement with no string interpolation.
		groupsJSON, err := json.Marshal(groups)
		if err != nil {
			return nil, fmt.Errorf("marshal groups: %w", err)
		}

		// DISTINCT ON picks the highest approved version per lineage from the set
		// of published rows whose visibility_groups overlaps the caller's groups.
		q := `
		SELECT DISTINCT ON (lineage_id) ` + workflowCols + `
		FROM workflows
		WHERE status            = 'published'
		  AND approval_state    = 'approved'
		  AND visibility_groups ?| array(SELECT jsonb_array_elements_text($1::jsonb))`
		args := []any{string(groupsJSON)}
		if afterLineage != "" {
			args = append(args, afterLineage)
			q += fmt.Sprintf(" AND lineage_id > $%d::uuid", len(args))
		}
		q += " ORDER BY lineage_id, version DESC"
		if limit > 0 {
			args = append(args, limit)
			q += fmt.Sprintf(" LIMIT $%d", len(args))
		}
		rows, err := conn.Query(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("list visible shared workflows: %w", err)
		}
		defer rows.Close()
		return collectWorkflows(rows)
	})
}

// ── Publish ───────────────────────────────────────────────────────────────────

// MarkSuccessRated stamps success_rated_at = NOW() on the row for (lineage,
// version) — idempotent when already set (only updates if NULL). Called by
// rateCore when rating == RATING_SUCCESS.
// Logs a warning (not an error) on 0 rows affected so a wrong lineage/version
// is debuggable without failing the rating call.
func (r *WorkflowRepo) MarkSuccessRated(ctx context.Context, tenant string, lineageID uuid.UUID, version int) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx, `
		UPDATE workflows
		SET success_rated_at = NOW()
		WHERE lineage_id = $1 AND version = $2 AND success_rated_at IS NULL`,
			lineageID, version,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// Either the version doesn't exist, or it was already stamped (idempotent).
			// Warn so a wrong lineage/version is debuggable; not an error — the rating
			// audit event still fires and the caller is non-fatal on this path.
			r.logger.Warn("MarkSuccessRated: 0 rows updated — version not found or already stamped",
				zap.String("lineage_id", lineageID.String()),
				zap.Int("version", version),
			)
		}
		return nil
	})
}

// ErrVersionNotFound is returned by PublishVersion when the (lineage, version)
// row does not exist at all. The caller surfaces this as NotFound.
var ErrVersionNotFound = fmt.Errorf("workflow version not found")

// PublishVersion transitions a version to published/shared, sets visibility_groups,
// and re-serializes the definition (caller computes requires before passing it
// here).
//
// Returns ErrVersionNotFound when (lineage, version) does not exist.
func (r *WorkflowRepo) PublishVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int, groups []string, definitionJSON []byte) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		groupsJSON, err := json.Marshal(groups)
		if err != nil {
			return fmt.Errorf("marshal groups: %w", err)
		}

		tag, err := conn.Exec(ctx, `
		UPDATE workflows
		SET status            = 'published',
		    visibility_kind   = 'shared',
		    visibility_groups = $3,
		    definition        = $4
		WHERE lineage_id = $1
		  AND version    = $2`,
			lineageID, version, groupsJSON, definitionJSON,
		)
		if err != nil {
			return fmt.Errorf("publish workflow version: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrVersionNotFound
		}
		return nil
	})
}

// ── Version pins ──────────────────────────────────────────────────────────────

// SetVersionPin upserts a user's version pin for a lineage.
func (r *WorkflowRepo) SetVersionPin(ctx context.Context, tenant, userID string, lineageID uuid.UUID, version int) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		tenantUUID, err := uuid.Parse(tenant)
		if err != nil {
			return fmt.Errorf("invalid tenant id: %w", err)
		}
		_, err = conn.Exec(ctx, `
		INSERT INTO workflow_version_pins (tenant_id, user_id, lineage_id, version)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (tenant_id, user_id, lineage_id)
		DO UPDATE SET version = EXCLUDED.version, updated_at = NOW()`,
			tenantUUID, userID, lineageID, version,
		)
		if err != nil {
			return fmt.Errorf("set version pin: %w", err)
		}
		return nil
	})
}

// GetVersionPin returns the pinned version for (user, lineage). ok=false when no
// pin exists.
func (r *WorkflowRepo) GetVersionPin(ctx context.Context, tenant, userID string, lineageID uuid.UUID) (int, bool, error) {
	return withConn2(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (int, bool, error) {
		tenantUUID, err := uuid.Parse(tenant)
		if err != nil {
			return 0, false, fmt.Errorf("invalid tenant id: %w", err)
		}
		var version int
		err = conn.QueryRow(ctx,
			`SELECT version FROM workflow_version_pins WHERE tenant_id = $1 AND user_id = $2 AND lineage_id = $3`,
			tenantUUID, userID, lineageID,
		).Scan(&version)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, false, nil
		}
		if err != nil {
			return 0, false, fmt.Errorf("get version pin: %w", err)
		}
		return version, true, nil
	})
}

// ClearVersionPin removes a user's version pin for a lineage. No error when no
// pin exists (idempotent).
func (r *WorkflowRepo) ClearVersionPin(ctx context.Context, tenant, userID string, lineageID uuid.UUID) error {
	return withConnErr(ctx, r.pool, tenant, func(conn *pgxpool.Conn) error {
		tenantUUID, err := uuid.Parse(tenant)
		if err != nil {
			return fmt.Errorf("invalid tenant id: %w", err)
		}
		_, err = conn.Exec(ctx,
			`DELETE FROM workflow_version_pins WHERE tenant_id = $1 AND user_id = $2 AND lineage_id = $3`,
			tenantUUID, userID, lineageID,
		)
		if err != nil {
			return fmt.Errorf("clear version pin: %w", err)
		}
		return nil
	})
}

// DeleteLineage removes every version of a workflow lineage and all per-user
// version pins for it, in a single transaction. Tenant-scoped (RLS). Returns the
// number of version rows deleted (0 when the lineage does not exist for the
// tenant). Owner authorization is enforced by the caller — the RPC layer verifies
// caller == lineage owner before invoking this.
func (r *WorkflowRepo) DeleteLineage(ctx context.Context, tenant string, lineageID uuid.UUID) (int, error) {
	return withConn(ctx, r.pool, tenant, func(conn *pgxpool.Conn) (int, error) {
		tenantUUID, err := uuid.Parse(tenant)
		if err != nil {
			return 0, fmt.Errorf("invalid tenant id: %w", err)
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return 0, fmt.Errorf("begin tx: %w", err)
		}
		defer tx.Rollback(ctx)

		// Pins live in a separate table with no FK to workflows; remove them first.
		if _, err := tx.Exec(ctx,
			`DELETE FROM workflow_version_pins WHERE tenant_id = $1 AND lineage_id = $2`,
			tenantUUID, lineageID,
		); err != nil {
			return 0, fmt.Errorf("delete version pins: %w", err)
		}

		tag, err := tx.Exec(ctx,
			`DELETE FROM workflows WHERE tenant_id = $1 AND lineage_id = $2`,
			tenantUUID, lineageID,
		)
		if err != nil {
			return 0, fmt.Errorf("delete workflow versions: %w", err)
		}

		if err := tx.Commit(ctx); err != nil {
			return 0, fmt.Errorf("commit delete lineage: %w", err)
		}

		deleted := int(tag.RowsAffected())
		r.logger.Debug("workflow lineage deleted",
			zap.String("lineage_id", lineageID.String()),
			zap.Int("versions_deleted", deleted),
		)
		return deleted, nil
	})
}

// ResolveVersionForUser returns the pinned version when one exists, otherwise
// returns the current (highest approved) version number for the lineage. This
// is the downgrade-resolution path.
//
// Note: a pin targeting a now-rejected version is returned as-is — this repo
// layer has no approval-state guard on the pin value. The RPC layer (CP6b/CP9)
// is responsible for validating a pin target's approval_state before storing
// the pin; by contract, pins only ever target approved versions.
func (r *WorkflowRepo) ResolveVersionForUser(ctx context.Context, tenant, userID string, lineageID uuid.UUID) (int, error) {
	pinned, ok, err := r.GetVersionPin(ctx, tenant, userID, lineageID)
	if err != nil {
		return 0, err
	}
	if ok {
		return pinned, nil
	}
	current, err := r.GetCurrent(ctx, tenant, lineageID)
	if err != nil {
		return 0, err
	}
	return current.Version, nil
}
