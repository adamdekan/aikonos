package broker

import (
	"context"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// approvalExpiryStore is the *db.TaskRepo surface SweepExpiredApprovals needs.
type approvalExpiryStore interface {
	ListExpiredApprovals(ctx context.Context) ([]db.ExpiredApproval, error)
	ExpireApproval(ctx context.Context, tenantID, approvalID string) (bool, error)
	Transition(ctx context.Context, tenantID, taskID string, from, to db.TaskState) error
	Get(ctx context.Context, tenantID, taskID string) (*db.Task, error)
}

var _ approvalExpiryStore = (*db.TaskRepo)(nil)

// SweepExpiredApprovals resolves every PENDING approval past expires_at to
// EXPIRED and its task from AWAITING_APPROVAL to TIMEOUT, one audit event each
// (AG-07, M-09). The approve path already refuses expired rows; the sweep only
// makes the outcome visible and frees the task. A task no longer awaiting
// approval (cancelled meanwhile) keeps its state. Returns the number expired.
func SweepExpiredApprovals(ctx context.Context, store approvalExpiryStore, em auditEmitter, log *zap.Logger) int {
	rows, err := store.ListExpiredApprovals(ctx)
	if err != nil {
		log.Warn("approval expiry: list failed", zap.Error(err))
		return 0
	}
	n := 0
	for _, r := range rows {
		ok, err := store.ExpireApproval(ctx, r.TenantID, r.ApprovalID)
		if err != nil {
			log.Warn("approval expiry: expire failed", zap.String("approval_id", r.ApprovalID), zap.Error(err))
			continue
		}
		if !ok {
			continue // resolved or expired by someone else meanwhile
		}
		n++
		if terr := store.Transition(ctx, r.TenantID, r.TaskID, db.TaskStateAwaitingApproval, db.TaskStateTimeout); terr != nil {
			log.Info("approval expiry: task not awaiting approval, state kept", zap.String("task_id", r.TaskID), zap.Error(terr))
		}
		task, _ := store.Get(ctx, r.TenantID, r.TaskID) // audit-only, as in ApproveTask
		const eventType = "aikonos.broker.approval.expired"
		if err := em.Emit(ctx, withOBO(&auditv1.AuditEvent{
			EventId:     ids.EventID(),
			TenantId:    r.TenantID,
			OccurredAt:  timestampNow(),
			EventType:   eventType,
			ResourceRef: "aikonos:task:" + r.TaskID,
			Decision:    auditv1.PolicyDecision_DENY,
		}, task)); err != nil {
			audit.RecordEmitFailure(ctx, log, err, eventType)
		}
	}
	return n
}
