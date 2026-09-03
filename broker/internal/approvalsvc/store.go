// Package approvalsvc holds the broker's approval-gate core: n-of-m
// threshold + separation-of-duty construction at plan-approval time (Build)
// and the approve/deny resolution + capability-token minting shared by the
// north (OIDC) ApproveTask and south (SPIFFE) ApproveGatewayTask RPCs
// (Resolve). Extracted from broker/internal/broker/service_plan.go (the
// inline requiresN/SoD/approverSet/CreateApprovalRequest block in SubmitPlan)
// and broker/internal/broker/gateway_tasks.go (resolveApprovalAndMint, moved
// as-is) — architecture-deepening CP4 (C6); see .
// Pure extraction: no approval-threshold, separation-of-duty, mint, or audit
// content change.
//
// Mirrors the workflowsvc precedent (broker/internal/workflowsvc): plain
// functions over narrow interfaces, not a stateful struct with methods.
//
// Old (broker) → new (approvalsvc):
//
//	SubmitPlan's inline approval-gate block → Build
//	resolveApprovalAndMint                  → Resolve
//	stepScope (mint-scope shape)             → StepScope
//
// package broker keeps SubmitPlan/ApproveTask/ApproveGatewayTask themselves —
// identity resolution, state-machine transitions outside the gate, and
// capability-token construction (mintStepTokens, capability.Minter +
// toolregistry.Registry, unchanged) stay there; only the approval-gate rules
// moved. Resolve takes a Minter closure instead of the concrete
// capability.Minter/toolregistry.Registry pair so this package stays
// decoupled from their construction.
package approvalsvc

import (
	"context"

	"github.com/google/uuid"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// Store is the task/approval persistence surface Build and Resolve need — a
// subset of broker's taskStore interface. *db.TaskRepo satisfies it (asserted
// in broker/internal/broker/submit_ifaces.go's own taskStore ⊇ this set).
type Store interface {
	CreateApprovalRequest(ctx context.Context, ar *db.ApprovalRequest) (uuid.UUID, error)
	GetPendingApprovalByTask(ctx context.Context, tenantID, taskID string) (*db.ApprovalRequest, error)
	ResolveApproval(ctx context.Context, tenantID, approvalID, approver string, approved bool, reason string) (db.ApprovalState, bool, error)
	Transition(ctx context.Context, tenantID, taskID string, from, to db.TaskState) error
	Get(ctx context.Context, tenantID, taskID string) (*db.Task, error)
	ListMintableSteps(ctx context.Context, tenantID, taskID string) ([]db.ExecutableStep, error)
}

var _ Store = (*db.TaskRepo)(nil)

// AccessPolicy is the FGA surface Build (ReadTuples for the approver set) and
// Resolve (CheckFGA for can_approve) need. *policy.Engine satisfies it.
type AccessPolicy interface {
	FGAEnabled() bool
	ReadTuples(ctx context.Context, f policy.ReadFilter) ([]policy.Relation, error)
	CheckFGA(ctx context.Context, user, relation, object string) (bool, error)
}

// ConfigProvider is the tenant-scoped runtime config surface Build needs
// (approval_expiry_hours, approval_required_n). Satisfied by broker's Config
// interface (*config.Store / nopConfig) — duck-typed, no import needed.
type ConfigProvider interface {
	GetInt(ctx context.Context, tenant, key string) int
}

// AuditEmitter is the audit seam Build uses for the insufficient_approvers
// event. *audit.Emitter satisfies it; tests inject a recording fake.
type AuditEmitter interface {
	Emit(ctx context.Context, event *auditv1.AuditEvent) error
}

// StepScope identifies a plan step for capability-token minting — mirrors
// broker's private stepScope. A plain data shape, not worth sharing across
// the package boundary for two fields.
type StepScope struct {
	Seq    int32
	ToolID string
}

// Minter mints one capability token per step scope on an APPROVED
// resolution. Satisfied by a closure the broker package builds around its
// existing mintStepTokens(*capability.Minter, *toolregistry.Registry, ...)
// helper, which stays where it is — this package never needs to know how a
// capability.Minter or toolregistry.Registry is constructed.
type Minter func(holder, taskID string, steps []StepScope) (map[int32]string, error)
