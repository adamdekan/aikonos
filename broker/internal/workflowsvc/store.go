// Package workflowsvc holds the workflow-feature core: repo-facing store
// interface, FGA access-policy surface, and the business logic shared by the
// north (OIDC) and south (SPIFFE) gRPC paths in package broker. Extracted from
// broker/internal/broker/service_workflows.go (workflowsvc-extraction CP1);
// see  for the extraction contract.
//
// package broker keeps identity resolution (callerIdentity, requireGatewayPeer,
// workflowCallerNorth/South), the two skill:workflows FGA gates, and the 23
// thin gRPC wrappers. Every wrapper resolves (tenantID, ownerUserID) from its
// own trust anchor, then calls into this package.
//
// Old (broker, unexported) → new (workflowsvc, exported) 1:1 mapping:
//
//	saveWorkflowCore          → Save
//	getWorkflowCore           → Get
//	listWorkflowsCore         → List
//	rateCore                  → Rate
//	proposeCore               → Propose
//	decideCore                → Decide
//	computeRequires           → ComputeRequires
//	publishCore               → Publish
//	forkCore                  → Fork
//	pinCore                   → Pin
//	clearPinCore              → ClearPin
//	listWorkflowVersionsCore  → ListVersions
//	deleteCore                → Delete
//	workflowStore (interface) → Store
//	workflowAccessPolicy      → AccessPolicy
//
// package broker retains thin unexported shims under the old names (used
// directly by pre-existing tests that call the former *Core functions) that
// delegate to the exported functions here — see service_workflows.go.
package workflowsvc

import (
	"context"

	"github.com/google/uuid"

	"github.com/adamdekan/aikonos/broker/internal/db"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// Store is the full WorkflowRepo surface every workflow operation needs
// (renamed from broker's workflowStore — CP1 of the workflowsvc extraction,
// itself collapsed from 12 former per-op micro-interfaces during the
// fable-rpc-twins collapse). *db.WorkflowRepo satisfies this; see the
// compile-time assertion below. broker.Deps.Workflows carries this type.
type Store interface {
	GetCurrent(ctx context.Context, tenant string, lineageID uuid.UUID) (*db.WorkflowRow, error)
	CreateVersion(ctx context.Context, tenant string, row db.WorkflowRow) (db.WorkflowRow, error)
	ProposeVersion(ctx context.Context, tenant string, row db.WorkflowRow) (db.WorkflowRow, error)
	ApproveVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int) error
	RejectVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int, reason string) error
	GetVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int) (*db.WorkflowRow, error)
	MarkSuccessRated(ctx context.Context, tenant string, lineageID uuid.UUID, version int) error
	PublishVersion(ctx context.Context, tenant string, lineageID uuid.UUID, version int, groups []string, definitionJSON []byte) error
	SetVersionPin(ctx context.Context, tenant, userID string, lineageID uuid.UUID, version int) error
	ClearVersionPin(ctx context.Context, tenant, userID string, lineageID uuid.UUID) error
	ResolveVersionForUser(ctx context.Context, tenant, userID string, lineageID uuid.UUID) (int, error)
	DeleteLineage(ctx context.Context, tenant string, lineageID uuid.UUID) (int, error)
	// ListByOwner returns the highest approved version per owned lineage.
	// afterLineage (canonical-uuid keyset cursor) + limit bound the fetch;
	// empty cursor and limit<=0 mean unbounded (legacy).
	ListByOwner(ctx context.Context, tenant, ownerUserID, afterLineage string, limit int) ([]*db.WorkflowRow, error)
	// ListVisibleShared returns published workflows visible to the given groups.
	// groups may be nil/empty; the implementation returns nothing in that case.
	// afterLineage + limit bound the fetch (same contract as ListByOwner).
	ListVisibleShared(ctx context.Context, tenant string, groups []string, afterLineage string, limit int) ([]*db.WorkflowRow, error)
	// ListVersions returns versions of a lineage, newest first. beforeVersion>0
	// keyset-filters below it; limit>0 caps the count; both <=0 = unbounded.
	ListVersions(ctx context.Context, tenant string, lineageID uuid.UUID, beforeVersion, limit int) ([]*db.WorkflowRow, error)
}

var _ Store = (*db.WorkflowRepo)(nil)

// AccessPolicy is the minimal policy surface List needs: group membership
// discovery + per-skill FGA check (renamed from broker's workflowAccessPolicy).
// *policy.Engine satisfies this — an unrelated concern from Store, so it is
// not folded into it.
type AccessPolicy interface {
	// FGAEnabled reports whether a real OpenFGA store is configured.
	FGAEnabled() bool
	// ListObjects returns all objects of objectType where user+relation holds.
	ListObjects(ctx context.Context, user, relation, objectType string) ([]string, error)
	// CheckFGA checks user+relation→object in OpenFGA.
	CheckFGA(ctx context.Context, user, relation, object string) (bool, error)
}

// EnvelopeStore is the minimal surface Propose needs to persist an inbox
// notification envelope. The broker's Deps.Tasks satisfies this in
// production and is passed directly on both call paths: inline as
// s.deps.Tasks on north, and via SandboxService.sandboxEnvelopesFor on south.
type EnvelopeStore interface {
	CreateEnvelope(ctx context.Context, e *db.Envelope, initial db.EnvelopeState) (uuid.UUID, error)
}

// AuditEmitter is the audit seam used by every operation here (mirrors
// broker's auditEmitter). *audit.Emitter satisfies it; tests inject a
// recording fake.
type AuditEmitter interface {
	Emit(ctx context.Context, event *auditv1.AuditEvent) error
}
