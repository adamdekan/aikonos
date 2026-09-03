// broker/internal/db/envelopes.go
// Persistence for inter-agent delegation envelopes (Phase 4).
// An envelope is a delegated task offer from one user/agent to another; the
// recipient accepts or rejects it from their inbox. Every query sets
// app.current_tenant for RLS — no raw SQL outside this package.
package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

// EnvelopeState mirrors the envelopes.state CHECK constraint.
type EnvelopeState string

const (
	EnvelopePending   EnvelopeState = "PENDING"
	EnvelopeDelivered EnvelopeState = "DELIVERED"
	EnvelopeAccepted  EnvelopeState = "ACCEPTED"
	EnvelopeRejected  EnvelopeState = "REJECTED"
	EnvelopeCompleted EnvelopeState = "COMPLETED"
	EnvelopeExpired   EnvelopeState = "EXPIRED"
	EnvelopeDismissed EnvelopeState = "DISMISSED"
)

// Envelope is the operational record of a delegation offer.
type Envelope struct {
	EnvelopeID     uuid.UUID
	ParentTaskID   *uuid.UUID
	TenantID       uuid.UUID
	FromUserID     string
	ToTarget       map[string]any // {"type":"user|group|role","id":"..."}
	TaskSpec       map[string]any // mirrors EnvelopeTask proto
	DelegationSpec map[string]any // mirrors EnvelopeDelegation proto
	Depth          int16
	State          EnvelopeState
	TraceID        string
	ExpiresAt      time.Time
	CreatedAt      time.Time
}

// CreateEnvelope inserts an envelope in the given initial state and returns its ID.
func (r *TaskRepo) CreateEnvelope(ctx context.Context, e *Envelope, initial EnvelopeState) (uuid.UUID, error) {
	return withConn(ctx, r.pool, e.TenantID.String(), func(conn *pgxpool.Conn) (uuid.UUID, error) {
		toTarget, _ := json.Marshal(e.ToTarget)
		taskSpec, _ := json.Marshal(e.TaskSpec)
		delegationSpec, _ := json.Marshal(e.DelegationSpec)
		depth := e.Depth
		if depth < 1 {
			depth = 1
		}

		var id uuid.UUID
		err := conn.QueryRow(ctx, `
		INSERT INTO envelopes (
			parent_task_id, tenant_id, from_user_id, to_target,
			task_spec, delegation_spec, depth, state, expires_at, trace_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING envelope_id`,
			e.ParentTaskID, e.TenantID, e.FromUserID, toTarget,
			taskSpec, delegationSpec, depth, string(initial), e.ExpiresAt, e.TraceID,
		).Scan(&id)
		if err != nil {
			return uuid.Nil, fmt.Errorf("insert envelope: %w", err)
		}

		r.logger.Info("envelope created",
			zap.String("envelope_id", id.String()),
			zap.String("from", e.FromUserID),
			zap.String("state", string(initial)),
		)
		return id, nil
	})
}

// ListInbox returns envelopes addressed to a user. Pending/delivered always;
// accepted/rejected/completed only when includeResolved is true.
func (r *TaskRepo) ListInbox(ctx context.Context, tenantID, userID string, includeResolved bool) ([]*Envelope, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]*Envelope, error) {
		states := "('PENDING','DELIVERED')"
		if includeResolved {
			states = "('PENDING','DELIVERED','ACCEPTED','REJECTED','COMPLETED','DISMISSED')"
		}
		rows, err := conn.Query(ctx, fmt.Sprintf(`
		SELECT envelope_id, parent_task_id, tenant_id, from_user_id, to_target,
		       task_spec, delegation_spec, depth, state, expires_at, created_at, trace_id
		FROM envelopes
		WHERE to_target->>'type' = 'user'
		  AND to_target->>'id' = $1
		  AND state IN %s
		ORDER BY created_at DESC
		LIMIT 100`, states), userID)
		if err != nil {
			return nil, fmt.Errorf("list inbox: %w", err)
		}
		defer rows.Close()

		return scanEnvelopes(rows)
	})
}

// GetEnvelope fetches a single envelope by ID. Returns an error matching
// db.ErrNotFound (errors.Is) if not found.
func (r *TaskRepo) GetEnvelope(ctx context.Context, tenantID, envelopeID string) (*Envelope, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (*Envelope, error) {
		rows, err := conn.Query(ctx, `
		SELECT envelope_id, parent_task_id, tenant_id, from_user_id, to_target,
		       task_spec, delegation_spec, depth, state, expires_at, created_at, trace_id
		FROM envelopes WHERE envelope_id = $1`, envelopeID)
		if err != nil {
			return nil, fmt.Errorf("query envelope: %w", err)
		}
		defer rows.Close()

		envs, err := scanEnvelopes(rows)
		if err != nil {
			return nil, err
		}
		if len(envs) == 0 {
			return nil, ErrNotFound
		}
		return envs[0], nil
	})
}

// RespondEnvelope transitions a pending/delivered envelope to ACCEPTED or
// REJECTED. Only the addressed recipient may respond. Returns the new state.
func (r *TaskRepo) RespondEnvelope(ctx context.Context, tenantID, envelopeID, userID string, accepted bool) (EnvelopeState, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (EnvelopeState, error) {
		newState := EnvelopeAccepted
		tsCol := "accepted_at"
		if !accepted {
			newState = EnvelopeRejected
			tsCol = "completed_at"
		}

		tag, err := conn.Exec(ctx, fmt.Sprintf(`
		UPDATE envelopes
		SET state = $1, %s = NOW()
		WHERE envelope_id = $2
		  AND to_target->>'type' = 'user'
		  AND to_target->>'id' = $3
		  AND state IN ('PENDING','DELIVERED')`, tsCol),
			string(newState), envelopeID, userID,
		)
		if err != nil {
			return "", fmt.Errorf("respond envelope: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return "", fmt.Errorf("envelope %s not pending for user %s", envelopeID, userID)
		}

		r.logger.Info("envelope responded",
			zap.String("envelope_id", envelopeID),
			zap.String("user", userID),
			zap.String("state", string(newState)),
		)
		return newState, nil
	})
}

// DismissEnvelope transitions a PENDING or DELIVERED envelope to DISMISSED.
// Only the addressed recipient may dismiss. Returns the new state or an error
// when the envelope is not found, already terminal, or not addressed to userID.
func (r *TaskRepo) DismissEnvelope(ctx context.Context, tenantID, envelopeID, userID string) (EnvelopeState, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (EnvelopeState, error) {
		tag, err := conn.Exec(ctx, `
		UPDATE envelopes
		SET state = $1, completed_at = NOW()
		WHERE envelope_id = $2
		  AND to_target->>'type' = 'user'
		  AND to_target->>'id' = $3
		  AND state IN ('PENDING','DELIVERED')`,
			string(EnvelopeDismissed), envelopeID, userID,
		)
		if err != nil {
			return "", fmt.Errorf("dismiss envelope: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return "", fmt.Errorf("envelope %s not pending for user %s", envelopeID, userID)
		}

		r.logger.Info("envelope dismissed",
			zap.String("envelope_id", envelopeID),
			zap.String("user", userID),
		)
		return EnvelopeDismissed, nil
	})
}

// CountEnvelopesByPayloadRef returns how many PENDING/DELIVERED envelopes
// still reference payloadRef in task_spec — the refcount check skill-transfer
// staging GC runs before deleting a shared xfer-<uuid> segment: a group
// fan-out send creates several envelopes pointing at one snapshot, so the
// segment must survive until every envelope pointing at it has settled.
func (r *TaskRepo) CountEnvelopesByPayloadRef(ctx context.Context, tenantID, payloadRef string) (int, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) (int, error) {
		var n int
		err := conn.QueryRow(ctx, `
		SELECT count(*) FROM envelopes
		WHERE tenant_id = $1
		  AND task_spec->>'payload_ref' = $2
		  AND state IN ('PENDING','DELIVERED')`, tenantID, payloadRef,
		).Scan(&n)
		if err != nil {
			return 0, fmt.Errorf("count envelopes by payload_ref: %w", err)
		}
		return n, nil
	})
}

func scanEnvelopes(rows pgx.Rows) ([]*Envelope, error) {
	var out []*Envelope
	for rows.Next() {
		var (
			e                              Envelope
			toTarget, taskSpec, delegation []byte
		)
		if err := rows.Scan(&e.EnvelopeID, &e.ParentTaskID, &e.TenantID, &e.FromUserID,
			&toTarget, &taskSpec, &delegation, &e.Depth, &e.State, &e.ExpiresAt,
			&e.CreatedAt, &e.TraceID); err != nil {
			return nil, fmt.Errorf("scan envelope: %w", err)
		}
		if len(toTarget) > 0 {
			if err := json.Unmarshal(toTarget, &e.ToTarget); err != nil {
				return nil, fmt.Errorf("unmarshal to_target: %w", err)
			}
		}
		if len(taskSpec) > 0 {
			if err := json.Unmarshal(taskSpec, &e.TaskSpec); err != nil {
				return nil, fmt.Errorf("unmarshal task_spec: %w", err)
			}
		}
		if len(delegation) > 0 {
			if err := json.Unmarshal(delegation, &e.DelegationSpec); err != nil {
				return nil, fmt.Errorf("unmarshal delegation_spec: %w", err)
			}
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}
