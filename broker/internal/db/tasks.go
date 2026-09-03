// broker/internal/db/tasks.go
// Task repository — all Postgres operations for the task state machine.
// Every query sets app.current_tenant for RLS enforcement.
// No raw SQL outside this package — all callers go through these methods.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/metrics"
)

// ErrNotFound is the package-wide sentinel for a missing row. Repo methods
// translate pgx.ErrNoRows to this via errors.Is at the point of return (wrapped
// or bare) so callers can match with errors.Is(err, db.ErrNotFound) regardless
// of any %w wrapping added along the way.
var ErrNotFound = errors.New("not found")

// ErrBudgetExceeded is returned by IncrementCost when the post-update total
// exceeds the task's cost_budget. The UPDATE still applies (always-record
// semantics) — cost_consumed already reflects the new total by the time this
// is returned; callers decide how to react (pre-gate future calls, audit the
// crossing) without retrying or rolling back the write.
var ErrBudgetExceeded = errors.New("cost budget exceeded")

// ErrClientRequestIDExists is returned by Create when t.ClientRequestID is
// non-empty and a task with that (tenant_id, client_request_id) pair already
// exists (migration 031's partial unique index). Create mutates t.TaskID (and
// the other identifying fields callers rely on) to the existing row before
// returning this sentinel, so callers that only need the task handle can
// treat it as an idempotent replay rather than a genuine error.
var ErrClientRequestIDExists = errors.New("client_request_id already exists for tenant")

// clientRequestIDUniqueIndex is the partial unique index name from migration
// 031 — used to distinguish this specific unique-violation from any other.
const clientRequestIDUniqueIndex = "idx_tasks_tenant_client_request_id"

// TaskState mirrors the TaskStatus proto enum values stored in Postgres.
type TaskState string

const (
	TaskStateCreated          TaskState = "CREATED"
	TaskStatePlanning         TaskState = "PLANNING"
	TaskStateValidating       TaskState = "VALIDATING"
	TaskStateAwaitingApproval TaskState = "AWAITING_APPROVAL"
	TaskStateApproved         TaskState = "APPROVED"
	TaskStateExecuting        TaskState = "EXECUTING"
	TaskStateCompleted        TaskState = "COMPLETED"
	TaskStateFailed           TaskState = "FAILED"
	TaskStateDenied           TaskState = "DENIED"
	TaskStateCancelled        TaskState = "CANCELLED"
	TaskStateTerminated       TaskState = "TERMINATED"
	TaskStateTimeout          TaskState = "TIMEOUT"
)

// Task is the operational record — not the audit record (that's in MinIO).
type Task struct {
	TaskID       uuid.UUID  `db:"task_id"`
	ParentTaskID *uuid.UUID `db:"parent_task_id"`
	TenantID     uuid.UUID  `db:"tenant_id"`
	OwnerUserID  string     `db:"owner_user_id"`
	State        TaskState  `db:"state"`
	Prompt       string     `db:"prompt"`
	CostBudget   int64      `db:"cost_budget"`
	CostConsumed int64      `db:"cost_consumed"`
	ReplanCount  int16      `db:"replan_count"`
	TraceID      string     `db:"trace_id"`
	CreatedAt    time.Time  `db:"created_at"`
	UpdatedAt    time.Time  `db:"updated_at"`
	Deadline     *time.Time `db:"deadline"`
	CompletedAt  *time.Time `db:"completed_at"`
	// GatewayManaged marks a task whose tool execution is driven by an external
	// agent harness (the Pi agent-gateway) that calls InvokeTool itself
	// (see migration 003).
	GatewayManaged bool `db:"gateway_managed"`
	// AgentID binds the task to a named agent (migration 013). When set, the
	// broker enforces the agent's skill boundary at SubmitPlan regardless of
	// gateway claims — the check is independent of the caller.
	AgentID *uuid.UUID `db:"agent_id"`
	// ClientRequestID is an optional caller-supplied idempotency key (migration
	// 031, F22). Empty means "no idempotency requested" — Create behaves
	// exactly as before.
	ClientRequestID string `db:"client_request_id"`
}

// TaskRepo handles all task persistence.
type TaskRepo struct {
	pool     *pgxpool.Pool
	logger   *zap.Logger
	recorder metrics.TaskMetricsRecorder
}

func NewTaskRepo(pool *pgxpool.Pool, logger *zap.Logger) *TaskRepo {
	return &TaskRepo{pool: pool, logger: logger}
}

// SetRecorder wires task-lifecycle metrics into the repo (F23). Optional and
// nil-safe by omission — additive so existing NewTaskRepo callers/tests
// compile unchanged; Transition simply skips the metric emit when unset.
func (r *TaskRepo) SetRecorder(recorder metrics.TaskMetricsRecorder) {
	r.recorder = recorder
}

// withTenant returns a context that sets app.current_tenant for RLS.
// Called at the start of every query. The tenant id is a bound parameter
// (never string-interpolated) so it cannot break out of the SQL statement;
// the empty-string GUC restored by db.NewPool's AfterRelease hook is the
// fail-closed default, so a connection that skips withTenant sees no
// previous tenant's data instead of silently inheriting it.
func withTenant(ctx context.Context, conn *pgxpool.Conn, tenantID string) error {
	_, err := conn.Exec(ctx, `SELECT set_config('app.current_tenant', $1, false)`, tenantID)
	return err
}

// Create inserts a new task in CREATED state.
func (r *TaskRepo) Create(ctx context.Context, t *Task) error {
	return withConnErr(ctx, r.pool, t.TenantID.String(), func(conn *pgxpool.Conn) error {
		_, err := conn.Exec(ctx, `
		INSERT INTO tasks (
			task_id, parent_task_id, tenant_id, owner_user_id,
			state, prompt, cost_budget, trace_id, deadline, gateway_managed, agent_id,
			client_request_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			t.TaskID, t.ParentTaskID, t.TenantID, t.OwnerUserID,
			TaskStateCreated, t.Prompt, t.CostBudget, t.TraceID, t.Deadline, t.GatewayManaged, t.AgentID,
			t.ClientRequestID,
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if t.ClientRequestID != "" && errors.As(err, &pgErr) &&
				pgErr.Code == "23505" && pgErr.ConstraintName == clientRequestIDUniqueIndex {
				existing, getErr := r.getByClientRequestID(ctx, conn, t.TenantID.String(), t.ClientRequestID)
				if getErr != nil {
					return fmt.Errorf("resolve existing task for client_request_id: %w", getErr)
				}
				t.TaskID = existing.TaskID
				t.TraceID = existing.TraceID
				return ErrClientRequestIDExists
			}
			return fmt.Errorf("insert task: %w", err)
		}

		r.logger.Debug("task created", zap.String("task_id", t.TaskID.String()))
		return nil
	})
}

// getByClientRequestID looks up the existing task row for a (tenant,
// client_request_id) pair — used by Create to resolve a duplicate insert to
// the row that already won the race.
func (r *TaskRepo) getByClientRequestID(ctx context.Context, conn *pgxpool.Conn, tenantID, clientRequestID string) (*Task, error) {
	rows, err := conn.Query(ctx, `
		SELECT task_id, parent_task_id, tenant_id, owner_user_id,
		       state, prompt, cost_budget, cost_consumed, replan_count,
		       trace_id, created_at, updated_at, deadline, completed_at,
		       gateway_managed, agent_id, client_request_id
		FROM tasks WHERE tenant_id = $1 AND client_request_id = $2`, tenantID, clientRequestID)
	if err != nil {
		return nil, fmt.Errorf("query task by client_request_id: %w", err)
	}
	defer rows.Close()

	t, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[Task])
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan task: %w", err)
	}
	return &t, nil
}

// Get fetches a task by ID. Returns an error matching db.ErrNotFound
// (errors.Is) if not found.
func (r *TaskRepo) Get(ctx context.Context, tenantID, taskID string) (*Task, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (*Task, error) {
		rows, err := conn.Query(ctx, `
		SELECT task_id, parent_task_id, tenant_id, owner_user_id,
		       state, prompt, cost_budget, cost_consumed, replan_count,
		       trace_id, created_at, updated_at, deadline, completed_at,
		       gateway_managed, agent_id, client_request_id
		FROM tasks WHERE task_id = $1`, taskID)
		if err != nil {
			return nil, fmt.Errorf("query task: %w", err)
		}
		defer rows.Close()

		t, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[Task])
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrNotFound
			}
			return nil, fmt.Errorf("scan task: %w", err)
		}
		return &t, nil
	})
}

// Transition atomically updates a task's state.
// Only valid transitions are permitted — invalid ones return an error.
// This is the single place where state machine transitions are enforced.
func (r *TaskRepo) Transition(ctx context.Context, tenantID, taskID string, from, to TaskState) error {
	if !isValidTransition(from, to) {
		return fmt.Errorf("invalid state transition: %s → %s", from, to)
	}

	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		tag, err := conn.Exec(ctx, `
		UPDATE tasks SET state = $1
		WHERE task_id = $2 AND state = $3`,
			to, taskID, from,
		)
		if err != nil {
			return fmt.Errorf("update state: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("state transition failed: task %s was not in state %s", taskID, from)
		}

		r.logger.Info("task state transition",
			zap.String("task_id", taskID),
			zap.String("from", string(from)),
			zap.String("to", string(to)),
		)
		if r.recorder != nil {
			r.recorder.RecordTransition(ctx, string(from), string(to), tenantID)
		}
		return nil
	})
}

// IncrementCost adds cost units to the running total. The UPDATE always
// applies (always-record semantics — a completed tool call's cost is never
// lost); the returned error only signals what the caller should do next:
// ErrNotFound (errors.Is) when taskID matched no row, ErrBudgetExceeded
// (errors.Is) when the post-update total exceeds the budget.
func (r *TaskRepo) IncrementCost(ctx context.Context, tenantID, taskID string, units int64) error {
	return withConnErr(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) error {
		var newTotal, budget int64
		err := conn.QueryRow(ctx, `
		UPDATE tasks
		SET cost_consumed = cost_consumed + $1
		WHERE task_id = $2
		RETURNING cost_consumed, cost_budget`,
			units, taskID,
		).Scan(&newTotal, &budget)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("increment cost %s: %w", taskID, ErrNotFound)
			}
			return fmt.Errorf("increment cost: %w", err)
		}

		if newTotal > budget {
			return fmt.Errorf("cost budget exceeded: consumed=%d budget=%d: %w", newTotal, budget, ErrBudgetExceeded)
		}
		return nil
	})
}

// ListPending returns tasks for a user that need attention (awaiting approval, etc.)
func (r *TaskRepo) ListPending(ctx context.Context, tenantID, userID string) ([]*Task, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]*Task, error) {
		rows, err := conn.Query(ctx, `
		SELECT task_id, parent_task_id, tenant_id, owner_user_id,
		       state, prompt, cost_budget, cost_consumed, replan_count,
		       trace_id, created_at, updated_at, deadline, completed_at,
		       gateway_managed, agent_id
		FROM tasks
		WHERE owner_user_id = $1
		  AND state NOT IN ('COMPLETED','FAILED','DENIED','CANCELLED','TERMINATED','TIMEOUT')
		ORDER BY created_at DESC
		LIMIT 50`,
			userID,
		)
		if err != nil {
			return nil, fmt.Errorf("list pending: %w", err)
		}
		defer rows.Close()

		tasks, err := pgx.CollectRows(rows, pgx.RowToStructByName[Task])
		if err != nil {
			return nil, fmt.Errorf("scan tasks: %w", err)
		}

		result := make([]*Task, len(tasks))
		for i := range tasks {
			result[i] = &tasks[i]
		}
		return result, nil
	})
}

// isValidTransition defines the task state machine.
// Modifying this is a significant architectural decision — document in DECISIONS.md.
func isValidTransition(from, to TaskState) bool {
	allowed := map[TaskState][]TaskState{
		TaskStateCreated:          {TaskStatePlanning, TaskStateCancelled},
		TaskStatePlanning:         {TaskStateValidating, TaskStateDenied, TaskStateCancelled},
		TaskStateValidating:       {TaskStateAwaitingApproval, TaskStateApproved, TaskStateDenied, TaskStateCancelled},
		TaskStateAwaitingApproval: {TaskStateApproved, TaskStateDenied, TaskStateCancelled, TaskStateTimeout},
		// APPROVED → FAILED covers a task refused at tool time, after approval but
		// before anything ran: InvokeTool's capability gate, cost-budget pre-gate,
		// rate limiter, and org effect-class switch all return before dispatch, so
		// the task never becomes EXECUTING and the caller reports FAILED from
		// APPROVED. Note there is deliberately no APPROVED → COMPLETED edge — a
		// task cannot complete work it never started, and EXECUTING is written by
		// InvokeTool immediately before the side effect runs.
		TaskStateApproved:  {TaskStateExecuting, TaskStateFailed, TaskStateCancelled},
		TaskStateExecuting: {TaskStatePlanning, TaskStateCompleted, TaskStateFailed, TaskStateTerminated, TaskStateTimeout, TaskStateCancelled},
		// Terminal states — no outgoing transitions
		TaskStateCompleted:  {},
		TaskStateFailed:     {},
		TaskStateDenied:     {},
		TaskStateCancelled:  {},
		TaskStateTerminated: {},
		TaskStateTimeout:    {},
	}

	for _, valid := range allowed[from] {
		if valid == to {
			return true
		}
	}
	return false
}
