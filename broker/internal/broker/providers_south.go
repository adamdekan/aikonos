package broker

// providers_south.go — SPIFFE-gated south RPCs for the agent-gateway.
// GetLlmProviders returns full provider records including api_key (for
// provider registration). EmitLlmUsage records metrics and emits an audit
// event; both are fire-and-forget — failures are logged but never returned.

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/metrics"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// microUSDPerUSD converts a USD float (the Pi-reported per-turn cost) to
// integer micro-USD for durable accumulation.
const microUSDPerUSD = 1_000_000

// costMicrosFor computes the micro-USD cost EmitLlmUsage should accumulate, in
// priority order: the request's own cost (Pi-reported, per-model accurate), else
// the request's model's own pricing record, else the flat per-provider token-rate
// fallback; unpriced providers accumulate 0 cost with tokens still recorded.
func costMicrosFor(req *brokerv1.EmitLlmUsageRequest, provider db.Provider) int64 {
	if req.Cost > 0 {
		return int64(math.Round(req.Cost * microUSDPerUSD))
	}
	if pr := modelPricingFor(provider, req.Model); pr != nil {
		return modelCostMicros(pr, req)
	}
	in := (req.TokensIn * provider.PriceInMicrosPerMTok) / 1_000_000
	out := (req.TokensOut * provider.PriceOutMicrosPerMTok) / 1_000_000
	return in + out
}

// modelPricingFor returns the pricing record the request's model declares, or
// nil when the model is unknown to the provider or carries no pricing.
func modelPricingFor(provider db.Provider, model string) *db.ModelPricing {
	for _, m := range provider.Models {
		if m.ID == model {
			return m.Pricing
		}
	}
	return nil
}

// pricingUnitFree and pricingUnitPer1kQueries are two of allowedPricingUnits
// (providers_admin.go) that modelCostMicros treats specially: free is always
// 0, and per_1k_queries divides its request quantity by 1000 before pricing.
// Every other non-per_mtok unit prices 1 quantity unit = 1 InMicros.
const (
	pricingUnitFree         = "free"
	pricingUnitPer1kQueries = "per_1k_queries"
)

// modelCostMicros prices one call from a per-model pricing record.
//
// per_mtok is priced from the call's token counts (the only unit with a
// token-costing basis). "free" is 0 by definition. Every other unit
// (per_image, per_minute, per_page, per_1k_queries…) is priced from the
// request's own quantity/unit — EmitLlmUsage's non-per_mtok unit-quantity
// extension — and costs 0 when the request's
// unit doesn't match the pricing unit or carries no quantity: the admin
// priced the model, the caller just reported no billable quantity for this
// call. Both zero-cost cases still suppress the unpriced-provider warning —
// the model is priced, by design or because this call didn't bill it.
func modelCostMicros(pr *db.ModelPricing, req *brokerv1.EmitLlmUsageRequest) int64 {
	switch pr.Unit {
	case db.PricingUnitPerMTok:
		inRate, outRate := pr.InMicros, pr.OutMicros
		// Context-tiered rates (OpenRouter shape): the highest tier the call's input
		// side reaches replaces the base in/out rates. Tiers are validated strictly
		// ascending, so the last match is the highest. Cache rates are never tiered.
		ctxTokens := req.TokensIn + req.CacheRead
		for _, t := range pr.Tiers {
			if ctxTokens >= t.MinContextTokens {
				inRate, outRate = t.InMicros, t.OutMicros
			}
		}
		// Divide once at the end rather than per term: four separate integer
		// divisions each truncate, and this is the durable spend meter.
		return (req.TokensIn*inRate +
			req.TokensOut*outRate +
			req.CacheRead*pr.CacheReadMicros +
			req.CacheWrite*pr.CacheWriteMicros) / 1_000_000
	case pricingUnitFree:
		return 0
	default:
		if req.Unit != pr.Unit || req.Quantity <= 0 {
			return 0
		}
		divisor := 1.0
		if pr.Unit == pricingUnitPer1kQueries {
			divisor = 1000
		}
		return int64(math.Round(req.Quantity * float64(pr.InMicros) / divisor))
	}
}

// GetLlmProviders returns all tenant providers, including their api_key
// fetched from Vault. Only the gateway SVID may call this.
func (s *SandboxService) GetLlmProviders(ctx context.Context, req *brokerv1.GetLlmProvidersRequest) (*brokerv1.GetLlmProvidersResponse, error) {
	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}

	store := s.deps.Providers
	if store == nil {
		return &brokerv1.GetLlmProvidersResponse{}, nil
	}

	rows, err := store.List(ctx, req.TenantId)
	if err != nil {
		s.deps.Logger.Warn("GetLlmProviders: repo error", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to list providers")
	}

	out := make([]*brokerv1.LlmProvider, 0, len(rows))
	for _, r := range rows {
		p := providerToProtoAdmin(r) // sets has_key, empty api_key
		// Fetch the key from Vault for the gateway's provider registration.
		if s.deps.Secrets != nil && r.HasKey {
			key, ok, kerr := s.deps.Secrets.ReadProviderKey(ctx, req.TenantId, r.ID)
			if kerr != nil {
				s.deps.Logger.Warn("GetLlmProviders: read key failed", zap.String("provider", r.ID), zap.Error(kerr))
			} else if ok {
				p.ApiKey = key
			} else {
				// HasKey is true (a key was registered) but Vault has no
				// secret at that path — the Vault-wipe signature (e.g. an
				// inmem Vault restart). Silence here is what produced the
				// on-prem no-response incident: the gateway got has_key=true
				// with an empty api_key and never surfaced why.
				s.deps.Logger.Warn("provider key missing from Vault — re-enter it in Admin → LLM Providers",
					zap.String("provider", r.ID))
			}
		}
		out = append(out, p)
	}

	// Fail open to an empty map: an unresolvable defaults read must not block
	// provider registration, and the gateway's own selection falls to tier 2
	// (first usable provider) when a capability has no default.
	defaults := map[string]string{}
	if d, err := store.DefaultsFor(ctx, req.TenantId); err != nil {
		s.deps.Logger.Warn("GetLlmProviders: defaults error", zap.String("tenant", req.TenantId), zap.Error(err))
	} else {
		defaults = d
	}
	return &brokerv1.GetLlmProvidersResponse{Providers: out, Defaults: defaults}, nil
}

// EmitLlmUsage records token/cost metrics and writes an llm.usage audit event.
// Fire-and-forget: metric and audit failures are logged but never returned to
// the caller. The gateway must never have a model-turn blocked by an emit failure.
func (s *SandboxService) EmitLlmUsage(ctx context.Context, req *brokerv1.EmitLlmUsageRequest) (*brokerv1.EmitLlmUsageResponse, error) {
	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}
	s.deps.recordLlmUsage(ctx, req)
	return &brokerv1.EmitLlmUsageResponse{}, nil
}

// recordLlmUsage is the single accounting path for a completed billable LLM
// call — metrics, TPM counters, audit, durable spend. On Deps so the north
// TestLlmProvider probe books its own usage through exactly this path rather
// than a parallel one. Never returns an error: no caller can act on a failure.
func (d Deps) recordLlmUsage(ctx context.Context, req *brokerv1.EmitLlmUsageRequest) {
	// Update rate-limit token counters for TPM enforcement.
	if d.RateLimiter != nil {
		total := req.TokensIn + req.TokensOut + req.CacheRead + req.CacheWrite
		d.RateLimiter.RecordLlmTokens(ctx, req.TenantId, req.AgentId, req.Provider, total)
	}

	// Audit emission runs in a detached goroutine so a slow NATS flush or
	// reconnect never blocks the model turn. Use context.Background() because
	// the request ctx is cancelled when the handler returns.
	go d.emitLlmUsageAudit(context.Background(), req)

	// One cost per call, handed to all three consumers: the metrics counter, the
	// durable counter (the authoritative cap meter) and the analytics event row
	// must never disagree on what a call cost.
	costMicros := d.resolveCostMicros(ctx, req)

	// Recorded after the resolve, not before it: most providers return no cost,
	// so req.Cost is usually 0 and metrics.Recorder skips a zero cost — which
	// meant llm_cost_total was never created at all while the DB row carried the
	// real server-priced value.
	if d.UsageMetrics != nil {
		d.UsageMetrics.RecordUsage(ctx, metrics.UsageAttrs{
			Provider:         req.Provider,
			Model:            req.Model,
			Tenant:           req.TenantId,
			Agent:            req.AgentId,
			TokensIn:         req.TokensIn,
			TokensOut:        req.TokensOut,
			TokensCacheRead:  req.CacheRead,
			TokensCacheWrite: req.CacheWrite,
			Cost:             float64(costMicros) / microUSDPerUSD,
		})
	}

	// Both writes are synchronous, unlike the audit emit above: as a detached
	// goroutine the spend write silently lost the record whenever the process
	// died between the RPC returning and the write landing. The caller is
	// already fire-and-forget on its own side, so this adds no model-turn
	// latency.
	d.accumulateLlmSpend(ctx, req, costMicros)
	d.recordUsageEvent(ctx, req, costMicros)
}

// unpricedProviderWarned dedups the "provider has no pricing" warning to once
// per (tenant, provider) per process — it fires from the per-call usage path,
// so an undeduped warn would be one line per model turn forever.
var unpricedProviderWarned sync.Map

// resolveCostMicros computes the call's cost_micros once (the request's own
// cost when present, else the provider's flat token-rate fallback) for both the
// durable counter and the analytics event.
func (d Deps) resolveCostMicros(ctx context.Context, req *brokerv1.EmitLlmUsageRequest) int64 {
	var provider db.Provider
	if req.Cost <= 0 && d.Providers != nil {
		var err error
		provider, err = d.Providers.Get(ctx, req.TenantId, req.Provider)
		if err != nil {
			// Unresolvable/unpriced provider: still record tokens, cost 0.
			d.Logger.Debug("resolveCostMicros: provider lookup failed, recording cost 0", zap.String("provider", req.Provider), zap.Error(err))
		}
	}
	costMicros := costMicrosFor(req, provider)
	// Tokens burned at zero cost means the provider carries no per-token pricing
	// (migration 042's default) — its spend stays invisible to caps forever, by
	// config rather than by accident. Say so once instead of accruing $0 silently.
	// A model that carries its own pricing record is priced by definition (free,
	// or a unit the pipeline cannot meter yet), so it is not a config gap.
	if costMicros == 0 && req.TokensIn+req.TokensOut > 0 && modelPricingFor(provider, req.Model) == nil {
		if _, dup := unpricedProviderWarned.LoadOrStore(req.TenantId+"|"+req.Provider, struct{}{}); !dup {
			d.Logger.Warn("llm provider has no per-token pricing — set it in Admin → LLM Providers — spend is not being tracked",
				zap.String("provider", req.Provider), zap.String("tenant", req.TenantId))
		}
	}
	return costMicros
}

// accumulateLlmSpend adds costMicros onto the tenant/user/agent/period counter.
// Nil repo is a no-op; failures are logged, never surfaced.
func (d Deps) accumulateLlmSpend(ctx context.Context, req *brokerv1.EmitLlmUsageRequest, costMicros int64) {
	if d.SpendCounters == nil {
		return
	}
	periodStart := db.SpendPeriodStart(time.Now())
	if err := d.SpendCounters.Accumulate(ctx, req.TenantId, req.UserId, req.AgentId, periodStart, costMicros, req.TokensIn, req.TokensOut); err != nil {
		d.Logger.Warn("accumulateLlmSpend: accumulate failed", zap.Error(err))
	}
}

// UsageEventStore is the minimal db.UsageEventRepo surface recordLlmUsage
// needs. *db.UsageEventRepo satisfies this; tests inject a fake. The retention
// sweep (PruneBefore) is driven from main.go against the concrete repo, so it
// is deliberately not part of this interface.
type UsageEventStore interface {
	Insert(ctx context.Context, ev db.UsageEvent) error
	// SessionTotals backs the north GetSessionUsage read (session_usage.go).
	SessionTotals(ctx context.Context, tenantID, userID, sessionID string) ([]db.SessionUsageRow, error)
}

// usageGroupCacheTTL bounds how stale an event's user_groups snapshot may be.
// Analytics-only, so staleness costs a reporting dimension, never a decision:
// a membership change shows up on the next refresh.
const usageGroupCacheTTL = 5 * time.Minute

type usageGroupEntry struct {
	groups    []string
	expiresAt time.Time
}

// usageGroupCache caches the FGA group snapshot per "tenant|user". Package-level
// like unpricedProviderWarned above — the emit path is per-call, so an
// uncached lookup would be one OpenFGA round trip per model turn.
// ponytail: entries are only overwritten on next access, never swept — ceiling
// is O(distinct tenant|user pairs seen since process start). Add a sweep if
// that grows unbounded.
var usageGroupCache sync.Map // "tenant|user" → usageGroupEntry

// recordUsageEvent writes the per-call analytics row
//. Nil repo is a no-op; an insert failure
// is logged and swallowed — EmitLlmUsage's fire-and-forget contract means a
// model turn must never fail over an analytics write.
func (d Deps) recordUsageEvent(ctx context.Context, req *brokerv1.EmitLlmUsageRequest, costMicros int64) {
	if d.UsageEvents == nil {
		return
	}
	if err := d.UsageEvents.Insert(ctx, db.UsageEvent{
		TenantID:   req.TenantId,
		UserID:     req.UserId,
		AgentID:    req.AgentId,
		RunID:      req.RunId,
		SessionID:  req.SessionId,
		Source:     req.Source,
		Provider:   req.Provider,
		Model:      req.Model,
		TokensIn:   req.TokensIn,
		TokensOut:  req.TokensOut,
		CacheRead:  req.CacheRead,
		CacheWrite: req.CacheWrite,
		CostMicros: costMicros,
		Quantity:   req.Quantity,
		Unit:       req.Unit,
		UserGroups: d.usageUserGroups(ctx, req.TenantId, req.UserId),
	}); err != nil {
		d.Logger.Warn("recordUsageEvent: insert failed", zap.Error(err))
	}
}

// usageUserGroups returns the user's FGA group ids (bare, "group:" stripped) for
// the event's reporting dimension, TTL-cached per (tenant, user). Fails open to
// empty on a nil policy engine or an FGA error: this is analytics, never a gate.
func (d Deps) usageUserGroups(ctx context.Context, tenant, user string) []string {
	if d.Policy == nil || user == "" {
		return nil
	}
	key := tenant + "|" + user
	if cached, ok := usageGroupCache.Load(key); ok {
		if e := cached.(usageGroupEntry); time.Now().Before(e.expiresAt) {
			return e.groups
		}
	}
	refs, err := d.Policy.ListObjects(ctx, "user:"+user, "member", "group")
	if err != nil {
		d.Logger.Debug("usageUserGroups: group discovery failed, recording no groups", zap.Error(err))
		return nil
	}
	groups := make([]string, 0, len(refs))
	for _, ref := range refs {
		// The picker and the Pi tool disagree on whether ids carry the type
		// prefix (same tolerance as workflowsvc.Publish); store bare ids.
		if gid := strings.TrimPrefix(ref, "group:"); gid != "" {
			groups = append(groups, gid)
		}
	}
	sort.Strings(groups)
	usageGroupCache.Store(key, usageGroupEntry{groups: groups, expiresAt: time.Now().Add(usageGroupCacheTTL)})
	return groups
}

// emitLlmUsageAudit writes the llm.usage audit event. Called from a detached
// goroutine; nil audit emitter is a no-op; failures are logged, never surfaced.
func (d Deps) emitLlmUsageAudit(ctx context.Context, req *brokerv1.EmitLlmUsageRequest) {
	if d.Audit == nil {
		return
	}
	ctxFields := map[string]any{
		"provider":    req.Provider,
		"model":       req.Model,
		"tokens_in":   req.TokensIn,
		"tokens_out":  req.TokensOut,
		"cache_read":  req.CacheRead,
		"cache_write": req.CacheWrite,
		"cost":        req.Cost,
		"agent_id":    req.AgentId,
	}
	ctxStruct, _ := structpb.NewStruct(ctxFields)
	ev := &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    req.TenantId,
		OccurredAt:  timestampNow(),
		ActorUserId: req.UserId,
		EventType:   "llm.usage",
		ResourceRef: "aikonos:llmprovider:" + req.Provider,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}
	if err := d.Audit.Emit(ctx, ev); err != nil {
		d.Logger.Debug("emitLlmUsageAudit: emit failed", zap.Error(err))
	}
}
