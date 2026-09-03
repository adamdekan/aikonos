// broker/internal/db/scheduled_runs.go
// Scheduled-run repository — Postgres operations for user-scheduled agentic runs.
// Every query sets app.current_tenant for RLS (withTenant). No raw SQL outside
// this package. The gateway ticker drives firing via ClaimDue/ReportResult.
package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
)

type ScheduleKind string

const (
	ScheduleKindCron ScheduleKind = "CRON"
	ScheduleKindOnce ScheduleKind = "ONCE"
)

type ScheduledRunState string

const (
	ScheduledRunActive    ScheduledRunState = "ACTIVE"
	ScheduledRunPaused    ScheduledRunState = "PAUSED"
	ScheduledRunCompleted ScheduledRunState = "COMPLETED"
	ScheduledRunFailed    ScheduledRunState = "FAILED"
)

// cronParser accepts the standard 5-field crontab spec (no seconds field), which
// is what the create UI offers (e.g. "0 9 * * 1-5"), optionally prefixed with a
// robfig CRON_TZ=<IANA>/TZ=<IANA> token so fields evaluate in that zone (the
// guided webui prepends the creator's zone). Without a token, evaluation is UTC
// via the anchor passed to Next.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// parseCronSpec is the single parse chokepoint. It guards robfig v3.0.1's panic:
// Parse does a slice op on strings.Index(spec, " ") and blows up when a
// TZ/CRON_TZ prefix has no space (a literal space, matching robfig exactly — a
// tab does not count). Cron specs cross a north RPC trust boundary, so guard here.
func parseCronSpec(expr string) (cron.Schedule, error) {
	if (strings.HasPrefix(expr, "TZ=") || strings.HasPrefix(expr, "CRON_TZ=")) && !strings.Contains(expr, " ") {
		return nil, fmt.Errorf("timezone prefix without cron fields")
	}
	return cronParser.Parse(expr)
}

// NextCronFire returns the next firing time strictly after `after` for a standard
// 5-field cron expression. Used both at create time and when ClaimDue advances a
// recurring schedule. The after.UTC() anchor is re-projected into the schedule's
// Location by robfig when a CRON_TZ token is present; without one it keeps
// evaluation UTC and host-TZ-independent.
func NextCronFire(expr string, after time.Time) (time.Time, error) {
	sched, err := parseCronSpec(expr)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid cron expression: %w", err)
	}
	return sched.Next(after.UTC()), nil
}

// ValidCronExpr reports whether expr is a parseable 5-field cron spec.
func ValidCronExpr(expr string) bool {
	_, err := parseCronSpec(expr)
	return err == nil
}

type ScheduledRun struct {
	ScheduledRunID uuid.UUID
	TenantID       uuid.UUID
	OwnerUserID    string
	Prompt         string
	Kind           ScheduleKind
	CronExpr       string
	NextFireAt     *time.Time
	ApprovedTools  []string
	State          ScheduledRunState
	LastFireAt     *time.Time
	LastStatus     string
	LastSummary    string
	RunCount       int32
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// WorkflowLineageID is set for a workflow-mode schedule (nil for prompt-mode);
	// mutually exclusive with Prompt (DB CHECK enforces the XOR).
	WorkflowLineageID *uuid.UUID
	// WorkflowInputs are the input values fed to the workflow at fire time.
	WorkflowInputs map[string]string
	// WorkflowDisplayName is populated only by List's join against the
	// workflow's current approved version — never by Create/Get/ClaimDue.
	WorkflowDisplayName string
}

type ScheduledRunRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewScheduledRunRepo(pool *pgxpool.Pool, logger *zap.Logger) *ScheduledRunRepo {
	return &ScheduledRunRepo{pool: pool, logger: logger}
}

const scheduledRunCols = `
	scheduled_run_id, tenant_id, owner_user_id, prompt, schedule_kind, cron_expr,
	next_fire_at, approved_tools, state, last_fire_at, last_status, last_summary,
	run_count, created_by, created_at, updated_at, workflow_lineage_id, workflow_inputs`

// scanScheduledRun maps a single row in scheduledRunCols order. approved_tools /
// workflow_inputs are jsonb, scanned as raw bytes then unmarshaled (pgx would
// otherwise treat a []string target as a text[] array). nameDest, when
// non-nil, additionally scans a trailing nullable joined column (List's
// workflow display name) appended after scheduledRunCols in the query.
func scanScheduledRun(row pgx.Row, nameDest **string) (*ScheduledRun, error) {
	var r ScheduledRun
	var cronExpr, lastStatus, lastSummary *string
	var toolsJSON, inputsJSON []byte
	var workflowLineageID *uuid.UUID

	dest := []any{
		&r.ScheduledRunID, &r.TenantID, &r.OwnerUserID, &r.Prompt, &r.Kind, &cronExpr,
		&r.NextFireAt, &toolsJSON, &r.State, &r.LastFireAt, &lastStatus, &lastSummary,
		&r.RunCount, &r.CreatedBy, &r.CreatedAt, &r.UpdatedAt, &workflowLineageID, &inputsJSON,
	}
	if nameDest != nil {
		dest = append(dest, nameDest)
	}
	if err := row.Scan(dest...); err != nil {
		return nil, err
	}
	if cronExpr != nil {
		r.CronExpr = *cronExpr
	}
	if lastStatus != nil {
		r.LastStatus = *lastStatus
	}
	if lastSummary != nil {
		r.LastSummary = *lastSummary
	}
	if len(toolsJSON) > 0 {
		if err := json.Unmarshal(toolsJSON, &r.ApprovedTools); err != nil {
			return nil, fmt.Errorf("decode approved_tools: %w", err)
		}
	}
	if r.ApprovedTools == nil {
		r.ApprovedTools = []string{}
	}
	r.WorkflowLineageID = workflowLineageID
	if len(inputsJSON) > 0 {
		if err := json.Unmarshal(inputsJSON, &r.WorkflowInputs); err != nil {
			return nil, fmt.Errorf("decode workflow_inputs: %w", err)
		}
	}
	return &r, nil
}

func nullableCron(kind ScheduleKind, expr string) *string {
	if kind == ScheduleKindCron && expr != "" {
		return &expr
	}
	return nil
}

// nullableInputs marshals workflow_inputs to JSON, or nil (SQL NULL) when the
// map is empty — mirrors nullableCron's "no value → NULL" convention for a
// prompt-mode row.
func nullableInputs(m map[string]string) ([]byte, error) {
	if len(m) == 0 {
		return nil, nil
	}
	return json.Marshal(m)
}

func (r *ScheduledRunRepo) Create(ctx context.Context, s *ScheduledRun) error {
	return withConnErr(ctx, r.pool, s.TenantID.String(), func(conn *pgxpool.Conn) error {
		toolsJSON, err := json.Marshal(s.ApprovedTools)
		if err != nil {
			return fmt.Errorf("encode approved_tools: %w", err)
		}
		inputsJSON, err := nullableInputs(s.WorkflowInputs)
		if err != nil {
			return fmt.Errorf("encode workflow_inputs: %w", err)
		}

		_, err = conn.Exec(ctx, `
		INSERT INTO scheduled_runs (
			scheduled_run_id, tenant_id, owner_user_id, prompt, schedule_kind,
			cron_expr, next_fire_at, approved_tools, state, created_by,
			workflow_lineage_id, workflow_inputs
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			s.ScheduledRunID, s.TenantID, s.OwnerUserID, s.Prompt, s.Kind,
			nullableCron(s.Kind, s.CronExpr), s.NextFireAt, toolsJSON, ScheduledRunActive, s.CreatedBy,
			s.WorkflowLineageID, inputsJSON,
		)
		if err != nil {
			return fmt.Errorf("insert scheduled_run: %w", err)
		}
		r.logger.Debug("scheduled_run created", zap.String("id", s.ScheduledRunID.String()))
		return nil
	})
}

func (r *ScheduledRunRepo) Get(ctx context.Context, tenantID, id string) (*ScheduledRun, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (*ScheduledRun, error) {
		row := conn.QueryRow(ctx, `SELECT `+scheduledRunCols+` FROM scheduled_runs WHERE scheduled_run_id = $1`, id)
		return scanScheduledRun(row, nil)
	})
}

// List returns scheduled runs for a tenant. When owner is non-empty it filters to
// that owner (own-runs view, or an admin filtering by username); empty owner
// returns every run in the tenant (admin all-runs view). A workflow-mode row's
// WorkflowDisplayName is joined from the lineage's current approved version
// (design: rename drift ruled out denormalizing the name onto the row).
func (r *ScheduledRunRepo) List(ctx context.Context, tenantID, owner string) ([]*ScheduledRun, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]*ScheduledRun, error) {
		q := `
		SELECT ` + scheduledRunCols + `, wf.name
		FROM scheduled_runs
		LEFT JOIN LATERAL (
			SELECT w.name FROM workflows w
			WHERE w.lineage_id = scheduled_runs.workflow_lineage_id
			  AND w.tenant_id = scheduled_runs.tenant_id
			  AND w.approval_state = 'approved'
			ORDER BY w.version DESC
			LIMIT 1
		) wf ON scheduled_runs.workflow_lineage_id IS NOT NULL`
		args := []any{}
		if owner != "" {
			q += ` WHERE owner_user_id = $1`
			args = append(args, owner)
		}
		q += ` ORDER BY created_at DESC LIMIT 500`

		rows, err := conn.Query(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("list scheduled_runs: %w", err)
		}
		defer rows.Close()

		var out []*ScheduledRun
		for rows.Next() {
			var name *string
			s, err := scanScheduledRun(rows, &name)
			if err != nil {
				return nil, err
			}
			if name != nil {
				s.WorkflowDisplayName = *name
			}
			out = append(out, s)
		}
		return out, rows.Err()
	})
}

// Update edits a schedule's definition (prompt / kind / cron / approved tools)
// and resets next_fire_at. Only ACTIVE or PAUSED runs may be edited.
// workflow_lineage_id / workflow_inputs are deliberately absent from the SET
// list — a workflow schedule's binding is immutable post-create, and the
// broker layer never varies s.WorkflowLineageID/WorkflowInputs on an edit.
func (r *ScheduledRunRepo) Update(ctx context.Context, tenantID, id string, s *ScheduledRun) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		toolsJSON, err := json.Marshal(s.ApprovedTools)
		if err != nil {
			return fmt.Errorf("encode approved_tools: %w", err)
		}
		tag, err := conn.Exec(ctx, `
		UPDATE scheduled_runs
		SET prompt = $1, schedule_kind = $2, cron_expr = $3, next_fire_at = $4,
		    approved_tools = $5, updated_at = NOW()
		WHERE scheduled_run_id = $6 AND state IN ('ACTIVE','PAUSED')`,
			s.Prompt, s.Kind, nullableCron(s.Kind, s.CronExpr), s.NextFireAt, toolsJSON, id,
		)
		if err != nil {
			return fmt.Errorf("update scheduled_run: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("scheduled_run %s not found or not editable", id)
		}
		return nil
	})
}

// SetState pauses/resumes (or terminally marks) a schedule, setting next_fire_at
// in the same write so a resumed run is immediately schedulable and a paused one
// can never be claimed.
func (r *ScheduledRunRepo) SetState(ctx context.Context, tenantID, id string, state ScheduledRunState, nextFire *time.Time) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx, `
		UPDATE scheduled_runs SET state = $1, next_fire_at = $2, updated_at = NOW()
		WHERE scheduled_run_id = $3`,
			state, nextFire, id,
		)
		if err != nil {
			return fmt.Errorf("set scheduled_run state: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("scheduled_run %s not found", id)
		}
		return nil
	})
}

func (r *ScheduledRunRepo) Delete(ctx context.Context, tenantID, id string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx, `DELETE FROM scheduled_runs WHERE scheduled_run_id = $1`, id)
		if err != nil {
			return fmt.Errorf("delete scheduled_run: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("scheduled_run %s not found", id)
		}
		return nil
	})
}

// ClaimDue atomically claims up to `limit` ACTIVE runs whose next_fire_at has
// passed and advances each so it cannot be re-claimed before the gateway reports
// back: a CRON run's next_fire_at jumps to its next slot (computed from `now`, so
// missed slots are skipped rather than backfilled); a ONCE run's next_fire_at
// goes NULL. FOR UPDATE SKIP LOCKED keeps concurrent ticks from double-claiming.
func (r *ScheduledRunRepo) ClaimDue(ctx context.Context, tenantID string, now time.Time, limit int) ([]*ScheduledRun, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]*ScheduledRun, error) {
		tx, err := conn.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("begin tx: %w", err)
		}
		defer tx.Rollback(ctx)

		rows, err := tx.Query(ctx, `
		SELECT `+scheduledRunCols+`
		FROM scheduled_runs
		WHERE state = 'ACTIVE' AND next_fire_at IS NOT NULL AND next_fire_at <= $1
		ORDER BY next_fire_at
		FOR UPDATE SKIP LOCKED
		LIMIT $2`,
			now, limit,
		)
		if err != nil {
			return nil, fmt.Errorf("select due: %w", err)
		}
		var due []*ScheduledRun
		for rows.Next() {
			s, err := scanScheduledRun(rows, nil)
			if err != nil {
				rows.Close()
				return nil, err
			}
			due = append(due, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}

		for _, s := range due {
			var next *time.Time
			if s.Kind == ScheduleKindCron {
				n, err := NextCronFire(s.CronExpr, now)
				if err != nil {
					// A row with an unparseable cron can never advance — park it as
					// FAILED so it stops being selected, rather than looping forever.
					r.logger.Warn("scheduled_run has invalid cron; marking FAILED",
						zap.String("id", s.ScheduledRunID.String()), zap.Error(err))
					if _, err := tx.Exec(ctx, `UPDATE scheduled_runs SET state='FAILED', next_fire_at=NULL, last_status='invalid cron', updated_at=NOW() WHERE scheduled_run_id=$1`, s.ScheduledRunID); err != nil {
						return nil, err
					}
					continue
				}
				next = &n
			}
			if _, err := tx.Exec(ctx, `
			UPDATE scheduled_runs
			SET next_fire_at = $1, last_fire_at = $2, run_count = run_count + 1, updated_at = NOW()
			WHERE scheduled_run_id = $3`,
				next, now, s.ScheduledRunID,
			); err != nil {
				return nil, fmt.Errorf("advance scheduled_run: %w", err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit claim: %w", err)
		}
		return due, nil
	})
}

// ReportResult records the outcome of a fired run. A ONCE run reaches a terminal
// state (COMPLETED/FAILED); a CRON run stays ACTIVE for its next slot and just
// records last_status/last_summary.
func (r *ScheduledRunRepo) ReportResult(ctx context.Context, tenantID, id string, ok bool, summary string) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		var kind ScheduleKind
		if err := conn.QueryRow(ctx, `SELECT schedule_kind FROM scheduled_runs WHERE scheduled_run_id = $1`, id).Scan(&kind); err != nil {
			return fmt.Errorf("load scheduled_run kind: %w", err)
		}

		status := "COMPLETED"
		if !ok {
			status = "FAILED"
		}
		var err error
		if kind == ScheduleKindOnce {
			_, err = conn.Exec(ctx, `
			UPDATE scheduled_runs
			SET state = $1, last_status = $2, last_summary = $3, next_fire_at = NULL, updated_at = NOW()
			WHERE scheduled_run_id = $4`,
				ScheduledRunState(status), status, summary, id,
			)
		} else {
			_, err = conn.Exec(ctx, `
			UPDATE scheduled_runs
			SET last_status = $1, last_summary = $2, updated_at = NOW()
			WHERE scheduled_run_id = $3`,
				status, summary, id,
			)
		}
		if err != nil {
			return fmt.Errorf("report scheduled_run result: %w", err)
		}
		return nil
	})
}
