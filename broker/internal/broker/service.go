// broker/internal/broker/service.go
// Phase 0 stub — implements the gRPC service interfaces with correct signatures.
// Every method logs its invocation and emits an audit event.
// Replace method bodies in Phase 1; never change the interface.
package broker

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/alerting"
	"github.com/adamdekan/aikonos/broker/internal/approvalsvc"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/baseline"
	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/connectorstore"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/metrics"
	"github.com/adamdekan/aikonos/broker/internal/notify"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/ratelimit"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	"github.com/adamdekan/aikonos/broker/internal/workflowsvc"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

var tracer = otel.Tracer("aikonos.broker")

// auditEmitter is the audit seam used by handlers. *audit.Emitter satisfies it;
// tests inject a recording fake. Only Emit is needed on deps — AttachBus /
// AttachEmailResolver are called on the concrete emitter in main before wiring.
type auditEmitter interface {
	Emit(ctx context.Context, event *auditv1.AuditEvent) error
}

// scheduledRunStore is the minimal *db.ScheduledRunRepo surface the broker
// package calls: the CRUD used by the CreateScheduledRun/ListScheduledRuns/
// UpdateScheduledRun/DeleteScheduledRun RPCs, plus ClaimDue/ReportResult used
// by the south-bound gateway scheduler twins. *db.ScheduledRunRepo satisfies
// it (compile-time-asserted below). Collapses the two former test-only
// override fields (claimDue, scheduledReport — see gateway_tasks.go) onto
// this one production Deps.Scheduled field (rpc-twins-tails CP3).
type scheduledRunStore interface {
	Create(ctx context.Context, s *db.ScheduledRun) error
	Get(ctx context.Context, tenantID, id string) (*db.ScheduledRun, error)
	List(ctx context.Context, tenantID, owner string) ([]*db.ScheduledRun, error)
	Update(ctx context.Context, tenantID, id string, s *db.ScheduledRun) error
	SetState(ctx context.Context, tenantID, id string, state db.ScheduledRunState, nextFire *time.Time) error
	Delete(ctx context.Context, tenantID, id string) error
	ClaimDue(ctx context.Context, tenantID string, now time.Time, limit int) ([]*db.ScheduledRun, error)
	ReportResult(ctx context.Context, tenantID, id string, ok bool, summary string) error
}

// workspaceFS is the minimal surface files.go/scheduler.go call against
// Deps.Workspace.Local: the full workspacefs.Backend interface (Enabled/List/
// ListDir/Read/Write/Delete/Move/Mkdir, all ctx-first) plus ReadVerified,
// which is workspacefs.Store-only and deliberately not on Backend (see
// workspacefs/backend.go's doc comment — HMAC session-sidecar verification
// never applies to a remote backend). *workspacefs.Store and
// *workspacefs.Router both satisfy it (asserted below): this is the CP3 seam
// that lets Deps.Workspace.Local hold either a bare local Store or a Router
// wrapping Local+Remote+Prefs with zero call-site change in files.go/
// scheduler.go.
type workspaceFS interface {
	workspacefs.Backend
	ReadVerified(ctx context.Context, tenant, user, rel string) (data []byte, modTime time.Time, legacy bool, err error)
}

var (
	_ taskStore         = (*db.TaskRepo)(nil)
	_ scheduledRunStore = (*db.ScheduledRunRepo)(nil)
	_ workspaceFS       = (*workspacefs.Store)(nil)
	_ workspaceFS       = (*workspacefs.Router)(nil)
)

// WorkspaceDeps groups the workspace-backend dependency cluster (C5 phase 2):
// the workspace filesystem itself, the per-user backend preference resolver,
// and the tenant-wide OneDrive Graph client — the first cohesive group carved
// off Deps (fewest call sites of the phase-2 candidates).
type WorkspaceDeps struct {
	// Local backs the per-user file-explorer RPCs (same root as the Tool
	// Proxy, so uploads are visible to workspace.read/doc.read). Typed as the
	// unexported workspaceFS interface (CP3) so production wires a
	// *workspacefs.Router (Local-only until CP5 wires a remote backend +
	// preference resolver) while tests keep injecting a bare *workspacefs.Store.
	Local workspaceFS

	// Prefs backs the per-user working-folder preference RPCs (CP5:
	// GetWorkspaceBackend/SetWorkspaceBackend) and — via its
	// workspacefs.PrefResolver implementation — Router.Prefs. Nil ⇒
	// GetWorkspaceBackend reports local-only defaults and
	// SetWorkspaceBackend/ListOneDriveFolders fail closed (FailedPrecondition).
	Prefs *WorkspacePrefResolver

	// Graph is the tenant-wide OneDrive Graph client (onedrivefs.Store,
	// wrapped) used directly by SetWorkspaceBackend (EnsureFolder) and
	// ListOneDriveFolders (drive-root folder listing) — the SAME instance CP5
	// wires into Router.Remote via oneDriveBackend. Nil ⇒ those RPCs fail
	// closed (FailedPrecondition).
	Graph OneDriveGraphStore
}

// McpDeps groups the MCP-server-admin dependency cluster (C5 phase 3): the
// admin-managed MCP connection registry, its bearer-token vault store, and
// the private-network SSRF-guard override — the second cohesive group carved
// off Deps (cheaper and more cohesive than the ConnectorDeps alternative,
// whose Secrets field is also shared by M365/provider-key admin, not just
// connectors).
type McpDeps struct {
	// Connections backs the CRUD RPCs for admin-managed MCP server entries.
	Connections mcpConnectionRepo
	// Bearer backs bearer-token storage in Vault (nil ⇒ bearer add/update
	// fails closed with FailedPrecondition; none-auth connections are
	// unaffected). The full secrets.McpBearerStore interface (includes Read)
	// is used by the Tool Proxy MCP handler; VaultClient satisfies both.
	Bearer secrets.McpBearerStore
	// AllowPrivate disables the SSRF guard in the broker-side MCP client
	// (ListMcpServerTools). False in production; set true in tests only.
	AllowPrivate bool
}

// Deps holds all service dependencies — injected at startup.
type Deps struct {
	Logger     *zap.Logger
	Tasks      taskStore
	Policy     *policy.Engine
	Audit      auditEmitter
	Capability *capability.Minter
	Notify     notify.Bus
	ToolProxy  *toolproxy.Proxy
	TenantID   string

	// Scheduled backs the scheduler RPCs (user-scheduled agentic runs) and the
	// south-bound gateway scheduler twins (ClaimDue/ReportResult). Typed as
	// scheduledRunStore (rpc-twins-tails CP3) so tests inject one fake here
	// instead of the two now-removed claimDue/scheduledReport overrides;
	// *db.ScheduledRunRepo satisfies it in production.
	Scheduled scheduledRunStore
	// SchedulerMinInterval is the shortest gap a new/edited cron schedule may
	// fire at (AIKONOS_SCHEDULER_MIN_INTERVAL). 0 disables the floor. Enforced
	// write-time only, at validateScheduleTiming — see enforceCronFloor.
	SchedulerMinInterval time.Duration
	// Workspace groups the workspace-backend dependency cluster (C5 phase 2 —
	// see WorkspaceDeps): the workspace filesystem, the per-user backend
	// preference resolver, and the tenant-wide OneDrive Graph client.
	Workspace WorkspaceDeps
	// Network backs the admin network access-list RPCs; ACL is the cached
	// evaluator SubmitPlan consults at plan time (the Tool Proxy enforces too).
	// aclDecider is satisfied by *netacl.Checker and by test fakes.
	Network *db.NetworkRuleRepo
	ACL     aclDecider
	// GatewaySpiffeID is the agent-gateway SVID allowed to call the south-bound
	// scheduler claim/report RPCs. Empty ⇒ any trust-domain peer (dev).
	GatewaySpiffeID string

	// KnownUsers seeds the admin principal directory (the future user-directory
	// seam). Emails without the "user:" prefix.
	KnownUsers []string

	// Connector (per-user external-storage OAuth) deps. All nil when Vault is
	// disabled; the connector RPCs then fail closed.
	Secrets    secrets.Provider
	ConnAuth   connector.PendingAuthStore
	OAuth      *connector.Registry
	Connectors *connectorstore.Store

	// AuditReader backs the QueryAudit RPC. Nil when MinIO is not configured —
	// the handler returns ok=false (store_configured=false) safely.
	AuditReader audit.ReaderIface

	// Mcp groups the MCP-server-admin dependency cluster (C5 phase 3 — see
	// McpDeps): the admin-managed MCP connection registry, its bearer-token
	// vault store, and the private-network SSRF-guard override.
	Mcp McpDeps

	// Config is the tenant-scoped runtime config store (schema-validated, cached).
	// Nil-safe: configFor() falls back to a zero-value nopConfig that returns
	// schema defaults so the consumer (approval expiry) always works.
	Config Config

	// ToolRegistry is the tool scope + effect-class registry. Production wires a
	// real NewRegistry() with manifests loaded. When nil (older tests that don't
	// inject it), broker call sites fall back to toolregistry.PkgDefault(), which
	// holds the static baseline + authoritative effect-class map but NO manifest
	// overlay — baseline-only. Overlay (manifest) tools resolve only through an
	// injected, manifest-loaded Registry.
	ToolRegistry *toolregistry.Registry

	// AlertRepo backs the ListAlerts admin RPC. Nil when alerting persistence is
	// not configured — the handler returns an empty list safely.
	AlertRepo alerting.AlertRepo

	// Agents backs the agent CRUD RPCs. Nil when the agents feature is disabled —
	// the handlers return FailedPrecondition safely.
	Agents AgentStore

	// ApiKeys backs the per-agent API key RPCs. Nil when the feature is disabled.
	ApiKeys ApiKeyStore
	// ApiKeyPepper is the HMAC pepper for at-rest key hashing. Empty → mint/resolve
	// return FailedPrecondition (fail closed, no unpeppered fallback).
	ApiKeyPepper string

	// GatewayGrantKey is the 32-byte HMAC key used to mint and verify owner-grant
	// tokens (broker/internal/gatewaygrant). Populated at startup from Vault
	// (secret/data/broker/gateway-grant) or AIKONOS_GATEWAY_GRANT_KEY override;
	// falls back to an ephemeral key with a Warn log. South handlers use this key
	// via gatewaygrant.Mint / gatewaygrant.Verify (wired in CP2).
	GatewayGrantKey []byte
	// GatewayGrantTTL is the validity window for minted owner-grant tokens.
	// Resolved from AIKONOS_GATEWAY_GRANT_TTL (Go duration), default 1h.
	GatewayGrantTTL time.Duration

	// Providers backs the LLM provider admin + south RPCs. Nil ⇒ provider RPCs
	// return FailedPrecondition (fail closed).
	Providers LlmProviderStore
	// AllowProviderTestPrivate disables the SSRF guard for the TestLlmProvider
	// connection probe (private/loopback endpoints). False in production; set
	// true in dev/tests where a provider endpoint may be loopback.
	AllowProviderTestPrivate bool
	// UsageMetrics records per-turn token/cost counters. Nil-safe (no-op when
	// absent — audit event is still emitted).
	UsageMetrics metrics.UsageRecorder

	// TaskMetrics records task-lifecycle OTel metrics (F23): SubmitPlan
	// aggregate outcomes and InvokeTool duration. Nil-safe — call sites
	// nil-check before recording (older tests, or metrics disabled).
	TaskMetrics metrics.TaskMetricsRecorder

	// Baseline observes successful agent-bound tool invocations for automated
	// behavioral baseline learning. Nil-safe — InvokeTool nil-checks before observing (baseline
	// learning disabled, or older tests).
	Baseline baseline.Recorder

	// RateLimiter enforces per-agent/provider/tenant rate limits at InvokeTool
	// and CheckRateLimit time. Nil means rate limiting is disabled (allow all).
	RateLimiter *ratelimit.Limiter

	// RateLimitPolicies backs the admin rate-limit CRUD RPCs.
	// Nil when the DB repo is not configured — handlers return FailedPrecondition.
	RateLimitPolicies *db.RateLimitPolicyRepo

	// SpendCaps backs the admin spend-cap CRUD + GetSpendSummary RPCs
	//. Interface-typed (mirrors LlmProviderStore) so
	// broker-package tests can inject a fake; *db.SpendCapRepo satisfies it in
	// production. Nil when not configured — handlers return FailedPrecondition.
	SpendCaps SpendCapStore

	// SpendCounters is the durable per-period spend accumulator EmitLlmUsage
	// writes to and CheckRateLimit's spend-cap gate reads from. Interface-typed
	// for the same reason as SpendCaps. Nil ⇒ accumulation/enforcement are both
	// no-ops (spend caps disabled).
	SpendCounters SpendCounterStore

	// UsageEvents is the per-call LLM usage event store recordLlmUsage writes
	// the analytics row to. Interface-
	// typed for the same reason as SpendCounters. Nil ⇒ no event is recorded;
	// the durable counter and cap enforcement are unaffected.
	UsageEvents UsageEventStore

	// SpendCache is the small in-process TTL cache over spend-cap/rollup reads
	// CheckRateLimit uses to avoid a DB round trip on every LLM call; mutated
	// caps invalidate their tenant's entries immediately. Shared by value
	// across BrokerService/SandboxService since both hold a copy of Deps and
	// this field is a pointer. Nil ⇒ enforcement reads the DB uncached.
	SpendCache *spendRollupCache

	// OrgSettings backs the A-series org governance control plane (org-global
	// instruction preamble, unattended-mode master, …). Nil → handlers return
	// defaults (read) / FailedPrecondition (write).
	OrgSettings *db.OrgSettingsRepo

	// OtelEndpoint is the broker's boot-time OTLP endpoint (AIKONOS_OTEL_ENDPOINT,
	// via viper "otel.endpoint"). Reported read-only by GetObservabilityInfo;
	// empty ⇒ telemetry export disabled. Process-global, not tenant-scoped.
	OtelEndpoint string

	// UserDirectory is the passive display-name cache. Populated by upserts in
	// callerIdentity after every verified north call; read by ListAssignments to
	// enrich principal DisplayName. Nil-safe: all call sites guard for nil.
	UserDirectory *db.UserDirectoryRepo

	// Provisioning backs the email→groups JIT rule admin RPCs. Nil when the
	// provisioning repo is not wired — RPCs return FailedPrecondition safely.
	// The interface also covers SubjectsByEmail so AddProvisioningRule can
	// immediately apply exact-match rules to already-seen users.
	Provisioning provisioningAdminStore

	// SkillOverlay backs the tenant skill overlay repo. Nil when the feature is
	// not yet wired (older tests); the overlay gate is skipped when nil.
	SkillOverlay skillOverlayStore

	// AgentSkillBundles backs the admin CRUD RPCs for uploaded SKILL.md bundles
	// (CP3). Nil when the feature is not yet wired — RPCs return FailedPrecondition
	// safely. *db.AgentSkillRepo satisfies the agentSkillRepo interface.
	AgentSkillBundles agentSkillRepo

	// OverlayLoader is the service-level lazy loader (one DB read per tenant per
	// process lifetime). CP3 creates and wires it; nil-safe — when absent the
	// overlay gate in InvokeTool / SubmitPlan is skipped.
	OverlayLoader *skillOverlayLoader

	// Workflows backs the versioned workflow persistence RPCs (CP2+). Nil when
	// the feature is not yet wired — RPCs return FailedPrecondition safely.
	// Typed as the workflowsvc.Store interface (CP1 of the fable-rpc-twins
	// collapse; moved to workflowsvc in the workflowsvc-extraction CP1/CP2) so
	// tests inject one fake here instead of one of eleven now-removed per-op
	// overrides; *db.WorkflowRepo satisfies it in production.
	Workflows workflowsvc.Store

	// OBOBroker lazily bootstraps/refreshes the tenant-wide OneDrive Entra OBO
	// connection. Nil ⇒ OneDrive is
	// never tenant-managed — ensureOneDriveOBO no-ops and the legacy per-user
	// connector path is unaffected. Typed as the narrow oboBroker interface
	// (rpc-twins-tails precedent) so tests inject a fake instead of a live
	// Entra-shaped HTTP server; *connector.OBOBroker satisfies it in production.
	OBOBroker oboBroker
}

// BrokerService implements brokerv1.BrokerServiceServer (north-bound: frontend → broker).
type BrokerService struct {
	brokerv1.UnimplementedBrokerServiceServer
	deps        Deps
	dirThrottle *upsertThrottle
	oboThrottle *upsertThrottle

	// simPolicy is a test-only override for SimulatePolicy (see simPolicyFor).
	// Production code never sets it; nil ⇒ simPolicyFor falls back to
	// deps.Policy. Lives on the service, not Deps, because a production
	// caller never has a reason to set it (C5 phase 1).
	simPolicy simPolicyEngine
}

func NewBrokerService(deps Deps) *BrokerService {
	if deps.OverlayLoader == nil && deps.SkillOverlay != nil && deps.ToolRegistry != nil {
		deps.OverlayLoader = newSkillOverlayLoader(deps.ToolRegistry, deps.SkillOverlay)
	}
	return &BrokerService{deps: deps, dirThrottle: newUpsertThrottle(), oboThrottle: newUpsertThrottleWithTTL(oboThrottleTTL)}
}

// ── Envelope helpers ────────────────────────────────────────────────────────

// defaultDelegationBudget is the dev stand-in for the sender's remaining cost
// budget until capability tokens / OpenFGA provide a real figure.
const defaultDelegationBudget int64 = 1000

func envelopeTaskFromSpec(spec map[string]any) *brokerv1.EnvelopeTask {
	t := &brokerv1.EnvelopeTask{}
	if v, ok := spec["intent"].(string); ok {
		t.Intent = v
	}
	if v, ok := spec["payload_ref"].(string); ok {
		t.PayloadRef = v
	}
	if v, ok := spec["priority"].(string); ok {
		t.Priority = v
	}
	if raw, ok := spec["required_skills"].([]any); ok {
		for _, s := range raw {
			if str, ok := s.(string); ok {
				t.RequiredSkills = append(t.RequiredSkills, str)
			}
		}
	}
	// kind surfaces a skill_transfer
	// envelope through ListInboxEnvelopes so the inbox can branch on it.
	if v, ok := spec["kind"].(string); ok {
		t.Kind = v
	}
	return t
}

func delegationMaxCost(e *db.Envelope) int64 {
	if v, ok := e.DelegationSpec["max_cost_units"].(float64); ok {
		return int64(v)
	}
	return 0
}

func joinReasons(rs []string) string {
	if len(rs) == 0 {
		return "policy denied"
	}
	return strings.Join(rs, "; ")
}

// SandboxService implements brokerv1.SandboxServiceServer (south-bound: sandbox → broker).
type SandboxService struct {
	brokerv1.UnimplementedSandboxServiceServer
	deps Deps

	// Test-only overrides for SubmitPlan / userSkillDecision (see
	// planPolicyFor / skillPolicyFor in submit_ifaces.go). Production code
	// never sets these; nil ⇒ fall back to deps.Policy. Live on the service,
	// not Deps, because a production caller never has a reason to set them
	// (C5 phase 1).
	policy      planPolicyEngine
	skillPolicy skillGatePolicy
}

func NewSandboxService(deps Deps) *SandboxService {
	if deps.OverlayLoader == nil && deps.SkillOverlay != nil && deps.ToolRegistry != nil {
		deps.OverlayLoader = newSkillOverlayLoader(deps.ToolRegistry, deps.SkillOverlay)
	}
	return &SandboxService{deps: deps}
}

// webFetchHost extracts the destination hostname from a web.fetch step's args
// for the network access-list check.
func webFetchHost(args *structpb.Struct) string {
	if args == nil {
		return ""
	}
	raw, _ := args.AsMap()["url"].(string)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil {
		return u.Hostname()
	}
	return ""
}

// toolInList reports whether id appears in the parsed disabled-tools slice.
func toolInList(list []string, id string) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

// Full args go to the audit log (MinIO); plan_steps stores only this hash.
func argsHash(args *structpb.Struct) string {
	if args == nil {
		return ""
	}
	b, err := json.Marshal(args.AsMap())
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ── helpers ───────────────────────────────────────────────────────────────────

func timestampNow() *timestamppb.Timestamp {
	return timestamppb.New(time.Now().UTC())
}

// gatewayExecutionHint is the reserved skill_hint that flags a task as driven by
// an external agent harness (the Pi agent-gateway), which calls InvokeTool
// itself. Recorded as tasks.gateway_managed.
const gatewayExecutionHint = "aikonos:execution=gateway"

func hasGatewayHint(hints []string) bool {
	for _, h := range hints {
		if h == gatewayExecutionHint {
			return true
		}
	}
	return false
}

// stepScope identifies a plan step for capability-token minting.
type stepScope struct {
	Seq    int32
	ToolID string
}

// mintStepTokens mints one least-privilege Biscuit per step — carrying only the
// scope that step's tool requires — keyed by step seq and base64-encoded, ready
// for the sandbox/agent to present to InvokeTool. A step whose tool has no
// registered scope is skipped (InvokeTool would deny it anyway). Returns nil
// when the minter is disabled, mirroring the InvokeTool gate (which is a no-op
// when Capability == nil). The round-trip is symmetric with InvokeTool:
// mint(scope) → base64 → InvokeTool base64-decodes → Authorize(raw, scope).
func mintStepTokens(m *capability.Minter, reg *toolregistry.Registry, holder, taskID string, steps []stepScope) (map[int32]string, error) {
	if m == nil || len(steps) == 0 {
		return nil, nil
	}
	out := make(map[int32]string, len(steps))
	for _, st := range steps {
		scope, ok := reg.RequiredScope(st.ToolID)
		if !ok {
			continue
		}
		raw, err := m.Mint(holder, taskID, []string{scope})
		if err != nil {
			return nil, err
		}
		out[st.Seq] = base64.StdEncoding.EncodeToString(raw)
	}
	return out, nil
}

// approvalMinter adapts mintStepTokens to the approvalsvc.Minter closure
// shape approvalsvc.Resolve expects, keeping capability.Minter/
// toolregistry.Registry construction in this package — approvalsvc never
// needs to know how either is built (CP4/C6).
func approvalMinter(m *capability.Minter, reg *toolregistry.Registry) approvalsvc.Minter {
	return func(holder, taskID string, steps []approvalsvc.StepScope) (map[int32]string, error) {
		scopes := make([]stepScope, len(steps))
		for i, st := range steps {
			scopes[i] = stepScope{Seq: st.Seq, ToolID: st.ToolID}
		}
		return mintStepTokens(m, reg, holder, taskID, scopes)
	}
}

// publishTaskEvent emits a task lifecycle event to the task's NATS subject.
// Best-effort: event routing never fails an RPC. No-op when the bus is disabled.
func publishTaskEvent(ctx context.Context, deps Deps, tenantID, taskID, eventType string, payload map[string]any) {
	if deps.Notify == nil || !deps.Notify.Enabled() {
		return
	}
	ev := &notify.Event{
		EventID:    ids.EventID(),
		TaskID:     taskID,
		TenantID:   tenantID,
		Type:       eventType,
		Payload:    payload,
		OccurredAt: time.Now().UTC(),
	}
	if err := notify.PublishEvent(ctx, deps.Notify, notify.TaskSubject(tenantID, taskID), ev); err != nil {
		deps.Logger.Debug("publish task event failed", zap.Error(err), zap.String("task_id", taskID))
	}
}

// publishInboxEvent emits a delegation envelope to the recipient's inbox subject.
func publishInboxEvent(ctx context.Context, deps Deps, tenantID, recipient, envelopeID, eventType string, payload map[string]any) {
	if deps.Notify == nil || !deps.Notify.Enabled() {
		return
	}
	ev := &notify.Event{
		EventID:    ids.EventID(),
		TaskID:     envelopeID,
		TenantID:   tenantID,
		Type:       eventType,
		Payload:    payload,
		OccurredAt: time.Now().UTC(),
	}
	if err := notify.PublishEvent(ctx, deps.Notify, notify.InboxSubject(tenantID, recipient), ev); err != nil {
		deps.Logger.Debug("publish inbox event failed", zap.Error(err), zap.String("envelope_id", envelopeID))
	}
}

// notifyEventToProto converts an internal notify.Event into the wire TaskEvent.
func notifyEventToProto(ev *notify.Event) *brokerv1.TaskEvent {
	pe := &brokerv1.TaskEvent{
		EventId:    ev.EventID,
		TaskId:     ev.TaskID,
		Type:       brokerv1.TaskEventType(brokerv1.TaskEventType_value[ev.Type]),
		OccurredAt: timestamppb.New(ev.OccurredAt),
	}
	if len(ev.Payload) > 0 {
		if st, err := structpb.NewStruct(ev.Payload); err == nil {
			pe.Payload = st
		}
	}
	return pe
}

func taskToProto(t *db.Task) *brokerv1.TaskState {
	return &brokerv1.TaskState{
		TaskId:       t.TaskID.String(),
		Status:       dbStateToProtoStatus(t.State),
		CostConsumed: t.CostConsumed,
		CostBudget:   t.CostBudget,
		CreatedAt:    timestamppb.New(t.CreatedAt),
		UpdatedAt:    timestamppb.New(t.UpdatedAt),
	}
}

func dbStateToProtoStatus(s db.TaskState) brokerv1.TaskStatus {
	switch s {
	case db.TaskStateCreated:
		return brokerv1.TaskStatus_CREATED
	case db.TaskStatePlanning:
		return brokerv1.TaskStatus_PLANNING
	case db.TaskStateValidating:
		return brokerv1.TaskStatus_VALIDATING
	case db.TaskStateAwaitingApproval:
		return brokerv1.TaskStatus_AWAITING_APPROVAL
	case db.TaskStateApproved:
		return brokerv1.TaskStatus_APPROVED
	case db.TaskStateExecuting:
		return brokerv1.TaskStatus_EXECUTING
	case db.TaskStateCompleted:
		return brokerv1.TaskStatus_COMPLETED
	case db.TaskStateFailed:
		return brokerv1.TaskStatus_FAILED
	case db.TaskStateDenied:
		return brokerv1.TaskStatus_DENIED
	case db.TaskStateCancelled:
		return brokerv1.TaskStatus_CANCELLED
	case db.TaskStateTerminated:
		return brokerv1.TaskStatus_TERMINATED
	case db.TaskStateTimeout:
		return brokerv1.TaskStatus_TIMEOUT
	default:
		return brokerv1.TaskStatus_TASK_STATUS_UNSPECIFIED
	}
}

func protoStatusToDBState(s brokerv1.TaskStatus) (db.TaskState, bool) {
	switch s {
	case brokerv1.TaskStatus_PLANNING:
		return db.TaskStatePlanning, true
	case brokerv1.TaskStatus_VALIDATING:
		return db.TaskStateValidating, true
	case brokerv1.TaskStatus_EXECUTING:
		return db.TaskStateExecuting, true
	case brokerv1.TaskStatus_COMPLETED:
		return db.TaskStateCompleted, true
	case brokerv1.TaskStatus_FAILED:
		return db.TaskStateFailed, true
	default:
		return "", false
	}
}
