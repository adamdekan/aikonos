// broker/internal/policy/engine.go
// Policy engine — combines OpenFGA (ReBAC) + OPA (ABAC).
//
// Phase 3: OPA ABAC is wired for real over OPA's REST Data API. The engine
// evaluates the `aikonos/plan_validation` and `aikonos/tool_invocation` Rego
// policies (served by the OPA sidecar at localhost:8181) to drive plan
// validation and the human-approval flow.
//
// OpenFGA (ReBAC) is wired for real (Check/Read/Write/Delete in fga.go). When no
// store id is configured CheckFGA falls back to a documented allow-all stub and
// reads return empty, so a store-less dev cluster still functions; OPA continues
// to enforce ABAC/approvals regardless.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/adamdekan/aikonos/broker/internal/effectclass"
	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

type Config struct {
	OpenFGAEndpoint string
	OPAEndpoint     string
	OpenFGAStoreID  string // empty → CheckFGA runs in dev stub mode (allow-all)
	OpenFGAModelID  string // optional; pins an authorization model version
}

// Decision is the outcome of a single tool-call / effect-class evaluation.
type Decision struct {
	Allow         bool
	NeedsApproval bool
	NeedsStepUp   bool
	Deny          bool
	Reason        string   // Human-readable; safe to surface to user/agent
	Reasons       []string // Individual deny reasons (from OPA)
	PolicyRuleID  string   // Internal ref for audit
}

// PlanDecision is the outcome of structural plan validation.
type PlanDecision struct {
	Allow      bool
	Violations []string // Human-readable; returned to the agent for replanning
}

// PlanContext carries the task/actor attributes the plan_validation policy
// needs that don't live on the Plan message itself.
type PlanContext struct {
	CostBudget          int64
	EffectClassCeiling  planv1.EffectClass
	ActorIsControlPlane bool
	ActorSpiffeID       string
}

type Engine struct {
	cfg            Config
	logger         *zap.Logger
	httpClient     *http.Client
	opaBase        string
	fga            *fgaClient
	extraToolGates []ToolGate
}

func NewEngine(ctx context.Context, cfg Config) (*Engine, error) {
	logger, _ := zap.NewProduction()
	e := &Engine{
		cfg:        cfg,
		logger:     logger,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		opaBase:    strings.TrimRight(cfg.OPAEndpoint, "/"),
		fga:        newFGAClient(cfg.OpenFGAEndpoint, cfg.OpenFGAStoreID, cfg.OpenFGAModelID, logger),
	}

	fgaMode := "stub (allow-all)"
	if e.fga.enabled() {
		fgaMode = "live (store " + cfg.OpenFGAStoreID + ")"
	}
	logger.Info("Policy engine initialized",
		zap.String("openfga", cfg.OpenFGAEndpoint),
		zap.String("openfga_mode", fgaMode),
		zap.String("opa", cfg.OPAEndpoint),
	)
	return e, nil
}

// queryOPA POSTs {"input": input} to /v1/data/<path> and unmarshals the
// returned document into out. A missing/empty result leaves out at its zero
// value (an undefined OPA document is a valid "nothing matched" outcome).
func (e *Engine) queryOPA(ctx context.Context, path string, input any, out any) error {
	body, err := json.Marshal(map[string]any{"input": input})
	if err != nil {
		return fmt.Errorf("marshal opa input: %w", err)
	}

	url := e.opaBase + "/v1/data/" + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build opa request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("opa request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("opa returned status %d for %s", resp.StatusCode, path)
	}

	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode opa response: %w", err)
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return nil // no document — caller's zero value stands (deny-by-default)
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("unmarshal opa result: %w", err)
	}
	return nil
}

// DecidePlan runs the whole-plan structural policy (aikonos/plan_validation).
// A plan that produces violations is sent back to the agent to replan.
func (e *Engine) DecidePlan(ctx context.Context, plan *planv1.Plan, pctx PlanContext) (*PlanDecision, error) {
	input := buildPlanInput(plan, pctx)

	var res struct {
		Allow      bool     `json:"allow"`
		Violations []string `json:"violations"`
	}
	if err := e.queryOPA(ctx, "aikonos/plan_validation", input, &res); err != nil {
		return nil, err
	}

	e.logger.Debug("policy.DecidePlan",
		zap.String("plan_id", plan.PlanId),
		zap.Bool("allow", res.Allow),
		zap.Int("violations", len(res.Violations)),
	)
	return &PlanDecision{Allow: res.Allow, Violations: res.Violations}, nil
}

// DecideToolCall evaluates whether a tool/step may run given its effect class
// and context (aikonos/tool_invocation). This is the critical security check —
// never bypass it.
func (e *Engine) DecideToolCall(ctx context.Context, q ToolQuery) (*Decision, error) {
	input := e.buildToolInput(q)

	core, err := e.evaluateGate(ctx, "aikonos/tool_invocation", input)
	if err != nil {
		return nil, err
	}

	// Aggregate extra gates (monotonic-stricter). No-op when none are registered.
	res, policyRuleID := e.aggregateExtraGates(ctx, input, core)

	reason := ""
	if len(res.DenyReasons) > 0 {
		reason = strings.Join(res.DenyReasons, "; ")
	} else if res.RequireApproval {
		reason = fmt.Sprintf("effect class %s requires human approval", effectClassString(q.EffectClass))
	}

	e.logger.Debug("policy.DecideToolCall",
		zap.String("user_id", q.UserID),
		zap.String("tool_id", q.ToolID),
		zap.String("effect_class", effectClassString(q.EffectClass)),
		zap.Bool("allow", res.Allow),
		zap.Bool("approval", res.RequireApproval),
		zap.Bool("step_up", res.RequireStepUp),
		zap.Bool("deny", res.Deny),
	)

	return &Decision{
		Allow:         res.Allow,
		NeedsApproval: res.RequireApproval,
		NeedsStepUp:   res.RequireStepUp,
		Deny:          res.Deny,
		Reason:        reason,
		Reasons:       res.DenyReasons,
		PolicyRuleID:  policyRuleID,
	}, nil
}

// EnvelopeSendContext carries the sender/recipient attributes the
// envelope_send policy needs that aren't on the wire envelope itself.
type EnvelopeSendContext struct {
	FGASendDecision     string   // from CheckFGA; "allow"/"deny". Empty → "allow"
	Depth               int      // delegation chain depth (top-level send = 1)
	EffectClass         string   // delegated task effect class (lowercase Rego form)
	SenderScopes        []string // capability scopes the sender holds
	SenderRiskScore     int
	SenderRemainingCost int64
	SenderSuspended     bool
	SenderGroupID       string
	RecipientGroupID    string
	Relationship        string // "direct_report" | "peer" | …
}

// EnvelopeDecision is the outcome of evaluating a delegation send.
type EnvelopeDecision struct {
	Allow               bool
	AutoAccept          bool
	RequireManualAccept bool
	ScopeViolations     []string
	DenyReasons         []string
}

// DecideEnvelopeSend evaluates a delegation against aikonos/envelope_send:
// scope attenuation, depth limit, cost budget, sender risk, and the
// auto-accept / manual-acceptance signals.
func (e *Engine) DecideEnvelopeSend(ctx context.Context, maxCostUnits int64, attenuatedScopes []string, sc EnvelopeSendContext) (*EnvelopeDecision, error) {
	fga := sc.FGASendDecision
	if fga == "" {
		fga = "allow"
	}
	depth := sc.Depth
	if depth < 1 {
		depth = 1
	}
	input := map[string]any{
		"fga_send_decision": fga,
		"relationship":      sc.Relationship,
		"envelope": map[string]any{
			"task": map[string]any{"effect_class": sc.EffectClass},
			"delegation": map[string]any{
				"depth":             depth,
				"attenuated_scopes": attenuatedScopes,
				"max_cost_units":    maxCostUnits,
			},
		},
		"sender": map[string]any{
			"capability_scopes":     sc.SenderScopes,
			"risk_score":            sc.SenderRiskScore,
			"remaining_cost_budget": sc.SenderRemainingCost,
			"is_suspended":          sc.SenderSuspended,
			"group_id":              sc.SenderGroupID,
		},
		"recipient": map[string]any{"group_id": sc.RecipientGroupID},
	}

	var res struct {
		Allow               bool     `json:"allow"`
		AutoAccept          bool     `json:"auto_accept"`
		RequireManualAccept bool     `json:"require_manual_acceptance"`
		ScopeViolations     []string `json:"scope_violations"`
		DenyReasons         []string `json:"deny_reasons"`
	}
	if err := e.queryOPA(ctx, "aikonos/envelope_send", input, &res); err != nil {
		return nil, err
	}

	e.logger.Debug("policy.DecideEnvelopeSend",
		zap.Bool("allow", res.Allow),
		zap.Bool("auto_accept", res.AutoAccept),
		zap.Int("scope_violations", len(res.ScopeViolations)),
		zap.Int("deny_reasons", len(res.DenyReasons)),
	)
	return &EnvelopeDecision{
		Allow:               res.Allow,
		AutoAccept:          res.AutoAccept,
		RequireManualAccept: res.RequireManualAccept,
		ScopeViolations:     res.ScopeViolations,
		DenyReasons:         res.DenyReasons,
	}, nil
}

// CheckFGA checks a relationship in OpenFGA (ReBAC). When a store id is
// configured it calls the OpenFGA Check API and fails closed on transport
// error. When no store is configured it falls back to the dev stub (allow-all);
// the OPA layer still enforces ABAC/approvals.
func (e *Engine) CheckFGA(ctx context.Context, user, relation, object string) (bool, error) {
	if !e.fga.enabled() {
		e.logger.Debug("policy.CheckFGA (stub — always allow)",
			zap.String("user", user),
			zap.String("relation", relation),
			zap.String("object", object),
		)
		return true, nil
	}

	allowed, err := e.fga.check(ctx, user, relation, object)
	if err != nil {
		e.logger.Warn("policy.CheckFGA error (failing closed)",
			zap.String("user", user),
			zap.String("relation", relation),
			zap.String("object", object),
			zap.Error(err),
		)
		return false, err
	}
	e.logger.Debug("policy.CheckFGA",
		zap.String("user", user),
		zap.String("relation", relation),
		zap.String("object", object),
		zap.Bool("allowed", allowed),
	)
	return allowed, nil
}

// Relation is a single (user, relation, object) tuple to persist in OpenFGA.
type Relation struct {
	User     string
	Relation string
	Object   string
}

// WriteRelations records relationship tuples in OpenFGA so later CheckFGA calls
// resolve (e.g. a new task's owner/approver). No-op when OpenFGA is not
// configured — the dev allow-all stub needs no tuples. Best-effort: the caller
// should log but not fail its RPC if this errors.
func (e *Engine) WriteRelations(ctx context.Context, rels ...Relation) error {
	if !e.fga.enabled() || len(rels) == 0 {
		return nil
	}
	tuples := make([]fgaTupleKey, 0, len(rels))
	for _, r := range rels {
		tuples = append(tuples, fgaTupleKey{User: r.User, Relation: r.Relation, Object: r.Object})
	}
	if err := e.fga.write(ctx, tuples); err != nil {
		e.logger.Warn("policy.WriteRelations failed", zap.Int("count", len(rels)), zap.Error(err))
		return err
	}
	e.logger.Debug("policy.WriteRelations", zap.Int("count", len(rels)))
	return nil
}

// FGAEnabled reports whether a real OpenFGA store is configured. Callers that
// must not silently no-op (e.g. an admin tuple write) gate on this — the dev
// stub allows reads to return empty but can't actually persist a mutation.
func (e *Engine) FGAEnabled() bool { return e.fga.enabled() }

// ReadFilter is a partial tuple query for ReadTuples — any field empty matches
// all, and Object may be a bare type prefix (e.g. "group:").
type ReadFilter struct {
	User     string
	Relation string
	Object   string
}

const (
	fgaReadPageSize = 100
	fgaReadMaxPages = 20
)

// ReadTuples returns all tuples matching f, following continuation tokens up to
// a page cap. Dev fallback (no store): returns (nil, nil) — there are no tuples
// to enumerate, so the admin view renders empty rather than erroring.
func (e *Engine) ReadTuples(ctx context.Context, f ReadFilter) ([]Relation, error) {
	if !e.fga.enabled() {
		return nil, nil
	}
	filter := fgaReadTupleKey{User: f.User, Relation: f.Relation, Object: f.Object}
	var out []Relation
	cont := ""
	truncated := false
	for page := 0; page < fgaReadMaxPages; page++ {
		keys, next, err := e.fga.read(ctx, filter, fgaReadPageSize, cont)
		if err != nil {
			e.logger.Warn("policy.ReadTuples failed", zap.Any("filter", f), zap.Error(err))
			return nil, err
		}
		for _, k := range keys {
			out = append(out, Relation{User: k.User, Relation: k.Relation, Object: k.Object})
		}
		if next == "" {
			break
		}
		cont = next
		if page == fgaReadMaxPages-1 {
			truncated = true
		}
	}
	if truncated {
		e.logger.Warn("policy.ReadTuples hit the page cap; results truncated",
			zap.Any("filter", f), zap.Int("count", len(out)),
			zap.Int("max_pages", fgaReadMaxPages), zap.Int("page_size", fgaReadPageSize))
	}
	e.logger.Debug("policy.ReadTuples", zap.Any("filter", f), zap.Int("count", len(out)))
	return out, nil
}

// ListUserGroups returns the group names a user is a direct member of (the
// `member` relation on group objects), for network-rule group scoping. Disabled
// FGA → nil (group-scoped rules simply don't match).
func (e *Engine) ListUserGroups(ctx context.Context, user string) ([]string, error) {
	if !e.fga.enabled() {
		return nil, nil
	}
	rels, err := e.ReadTuples(ctx, ReadFilter{User: "user:" + user, Relation: "member", Object: "group:"})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rels {
		if name, ok := strings.CutPrefix(r.Object, "group:"); ok && name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// ListObjects returns the full object refs (e.g. "agent:<id>") that satisfy
// user+relation for the given objectType via the OpenFGA ListObjects API.
// Disabled FGA → (nil, nil). Live path fails closed on error.
func (e *Engine) ListObjects(ctx context.Context, user, relation, objectType string) ([]string, error) {
	if !e.fga.enabled() {
		return nil, nil
	}
	refs, err := e.fga.listObjects(ctx, user, relation, objectType)
	if err != nil {
		e.logger.Warn("policy.ListObjects error (failing closed)",
			zap.String("user", user),
			zap.String("relation", relation),
			zap.String("object_type", objectType),
			zap.Error(err),
		)
		return nil, err
	}
	e.logger.Debug("policy.ListObjects",
		zap.String("user", user),
		zap.String("relation", relation),
		zap.String("object_type", objectType),
		zap.Int("count", len(refs)),
	)
	return refs, nil
}

// DeleteRelations removes relationship tuples. No-op when OpenFGA is not
// configured (mirrors WriteRelations). Idempotent: deleting an absent tuple
// succeeds.
func (e *Engine) DeleteRelations(ctx context.Context, rels ...Relation) error {
	if !e.fga.enabled() || len(rels) == 0 {
		return nil
	}
	tuples := make([]fgaTupleKey, 0, len(rels))
	for _, r := range rels {
		tuples = append(tuples, fgaTupleKey{User: r.User, Relation: r.Relation, Object: r.Object})
	}
	if err := e.fga.delete(ctx, tuples); err != nil {
		e.logger.Warn("policy.DeleteRelations failed", zap.Int("count", len(rels)), zap.Error(err))
		return err
	}
	e.logger.Debug("policy.DeleteRelations", zap.Int("count", len(rels)))
	return nil
}

// ── Input builders ────────────────────────────────────────────────────────────

func buildPlanInput(plan *planv1.Plan, pctx PlanContext) map[string]any {
	steps := make([]map[string]any, 0, len(plan.Steps))
	for _, s := range plan.Steps {
		var args map[string]any
		if s.Args != nil {
			args = s.Args.AsMap()
		}
		steps = append(steps, map[string]any{
			"seq":             s.Seq,
			"tool_id":         s.ToolId,
			"effect_class":    effectClassString(s.EffectClass),
			"estimated_cost":  s.EstimatedCost,
			"reads_sensitive": s.ReadsSensitive,
			"justification":   s.Justification,
			"args":            args,
		})
	}
	return map[string]any{
		"plan": map[string]any{
			"plan_id":             plan.PlanId,
			"steps":               steps,
			"has_dlp_attestation": plan.HasDlpAttestation,
		},
		"task": map[string]any{
			"cost_budget":          pctx.CostBudget,
			"effect_class_ceiling": effectClassString(pctx.EffectClassCeiling),
		},
		"actor": map[string]any{
			"is_control_plane": pctx.ActorIsControlPlane,
			"spiffe_id":        pctx.ActorSpiffeID,
		},
	}
}

func (e *Engine) buildToolInput(q ToolQuery) map[string]any {
	now := q.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	fga := q.FGADecision
	if fga == "" {
		fga = "allow" // OpenFGA stub default — see CheckFGA
	}
	return map[string]any{
		"fga_decision": fga,
		"tool":         map[string]any{"id": q.ToolID, "effect_class": effectClassString(q.EffectClass)},
		"user":         map[string]any{"risk_score": q.RiskScore, "is_on_call": q.IsOnCall},
		"resource":     map[string]any{"sensitivity": q.ResourceSensitivity},
		"approval":     map[string]any{"is_approved": q.IsApproved},
		"plan":         map[string]any{"reads_sensitive": q.ReadsSensitive, "has_dlp_attestation": q.HasDLPAttestation},
		"actor":        map[string]any{"spiffe_id": q.ActorSpiffeID},
		"env":          map[string]any{"hour": now.Hour(), "day": strings.ToLower(now.Weekday().String())},
	}
}

// effectClassString maps the proto enum to the lowercase strings the Rego
// policies expect (READ_ONLY → "read_only").
// Delegates to effectclass.RegoString; returns "unspecified" for the zero value.
func effectClassString(ec planv1.EffectClass) string {
	if s := effectclass.RegoString(ec); s != "" {
		return s
	}
	return "unspecified"
}

// ToolQuery is the per-step input to DecideToolCall.
type ToolQuery struct {
	UserID              string
	TenantID            string
	ToolID              string
	EffectClass         planv1.EffectClass
	RiskScore           int
	ResourceRef         string
	ResourceSensitivity string
	ActorSpiffeID       string
	IsOnCall            bool
	IsApproved          bool
	ReadsSensitive      bool
	HasDLPAttestation   bool
	FGADecision         string    // from CheckFGA; "allow"/"deny". Empty → "allow"
	Now                 time.Time // for business-hours rule; zero → time.Now().UTC()
}
