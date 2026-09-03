package broker

// websearch_admin.go — north-bound web.search engine config admin RPCs
//. Metadata lives in org_settings, the engine
// api_key in Vault; TestWebSearchConfig probes the configured engine with a
// minimal query. Gate: callerIdentity → requireTenantAdmin, matching
// m365_admin.go/providers_admin.go exactly.

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const webSearchTestTimeout = 12 * time.Second

// websearchSecrets is the Vault write/delete seam this file needs on top of
// CP1's read-only equivalent (toolproxy/web_search.go's webSearchKeyReader) —
// kept off the shared secrets.Provider interface for the same reason CP1
// documented: promoting it there forces every other Provider fake in the repo
// to grow three stub methods for a feature they don't touch. *secrets.
// VaultClient satisfies this via type assertion on Deps.Secrets.
type websearchSecrets interface {
	ReadWebSearchApp(ctx context.Context, tenant string) (string, bool, error)
	WriteWebSearchApp(ctx context.Context, tenant, apiKey string) error
	DeleteWebSearchApp(ctx context.Context, tenant string) error
}

// OrgSettingsWebSearchSource adapts db.OrgSettingsRepo onto
// toolproxy.WebSearchSettings so the web.search plugin (CP1) resolves the
// tenant's configured engine + max_results at call time.
type OrgSettingsWebSearchSource struct {
	orgSettings *db.OrgSettingsRepo
}

// NewOrgSettingsWebSearchSource constructs the adapter.
func NewOrgSettingsWebSearchSource(orgSettings *db.OrgSettingsRepo) *OrgSettingsWebSearchSource {
	return &OrgSettingsWebSearchSource{orgSettings: orgSettings}
}

// Resolve implements toolproxy.WebSearchSettings. An unset engine (no row, or
// engine never configured) reports configured=false — the safe "ask an
// admin" path, never a default engine.
func (s *OrgSettingsWebSearchSource) Resolve(ctx context.Context, tenant string) (string, int, bool, error) {
	if s.orgSettings == nil {
		return "", 0, false, nil
	}
	settings, err := s.orgSettings.Get(ctx, tenant)
	if err != nil {
		return "", 0, false, err
	}
	if settings.WebsearchEngine == "" {
		return "", 0, false, nil
	}
	return settings.WebsearchEngine, settings.WebsearchMaxResults, true, nil
}

func (s *BrokerService) GetWebSearchConfig(ctx context.Context, _ *brokerv1.GetWebSearchConfigRequest) (*brokerv1.GetWebSearchConfigResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.websearch.get")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	settings := db.OrgSettings{}
	if s.deps.OrgSettings != nil {
		var gerr error
		settings, gerr = s.deps.OrgSettings.Get(ctx, tenant)
		if gerr != nil {
			return nil, status.Errorf(codes.Internal, "get websearch config: %v", gerr)
		}
	}
	return &brokerv1.GetWebSearchConfigResponse{Config: webSearchToProto(settings, s.webSearchHasKey(ctx, tenant))}, nil
}

func (s *BrokerService) UpsertWebSearchConfig(ctx context.Context, req *brokerv1.UpsertWebSearchConfigRequest) (*brokerv1.UpsertWebSearchConfigResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.websearch.upsert")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	cfg := req.GetConfig()
	if cfg == nil || cfg.GetEngine() == "" {
		return nil, status.Error(codes.InvalidArgument, "config.engine is required")
	}
	if cfg.GetMaxResults() < 0 {
		return nil, status.Error(codes.InvalidArgument, "config.max_results must not be negative")
	}

	// Key handling mirrors UpsertM365Connection: write to Vault when supplied,
	// otherwise preserve the stored has_key value — a blank key on an edit
	// must never silently wipe what's already there.
	hasKey := false
	if req.ApiKey != "" {
		ws, ok := s.deps.Secrets.(websearchSecrets)
		if !ok {
			s.deps.Logger.Warn("UpsertWebSearchConfig: Secrets does not support web.search key storage")
			return nil, status.Errorf(codes.Internal, "failed to store api key")
		}
		if werr := ws.WriteWebSearchApp(ctx, tenant, req.ApiKey); werr != nil {
			s.deps.Logger.Warn("UpsertWebSearchConfig: write key failed", zap.Error(werr))
			return nil, status.Errorf(codes.Internal, "failed to store api key")
		}
		hasKey = true
	} else {
		hasKey = s.webSearchHasKey(ctx, tenant)
	}

	if s.deps.OrgSettings == nil {
		return nil, status.Error(codes.FailedPrecondition, "org settings not configured")
	}
	patch := map[string]any{
		"websearch_engine":      cfg.GetEngine(),
		"websearch_max_results": int(cfg.GetMaxResults()),
	}
	settings, merr := s.deps.OrgSettings.Merge(ctx, tenant, patch, user)
	if merr != nil {
		return nil, status.Errorf(codes.Internal, "upsert websearch config: %v", merr)
	}

	changed := []string{"engine", "max_results"}
	if req.ApiKey != "" {
		changed = append(changed, "api_key")
	}
	s.emitWebSearchAudit(ctx, span.SpanContext().TraceID().String(), tenant, user, "websearch.config.upsert", changed)

	out := webSearchToProto(settings, hasKey)
	return &brokerv1.UpsertWebSearchConfigResponse{Config: out}, nil
}

func (s *BrokerService) DeleteWebSearchConfig(ctx context.Context, _ *brokerv1.DeleteWebSearchConfigRequest) (*brokerv1.DeleteWebSearchConfigResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.websearch.delete")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	if s.deps.OrgSettings != nil {
		patch := map[string]any{
			"websearch_engine":      "",
			"websearch_max_results": 0,
		}
		if _, merr := s.deps.OrgSettings.Merge(ctx, tenant, patch, user); merr != nil {
			s.deps.Logger.Warn("DeleteWebSearchConfig: repo error", zap.Error(merr))
			return nil, status.Errorf(codes.Internal, "failed to clear websearch config")
		}
	}
	// Best-effort: delete the key even if the metadata row was already absent.
	if ws, ok := s.deps.Secrets.(websearchSecrets); ok {
		if derr := ws.DeleteWebSearchApp(ctx, tenant); derr != nil {
			s.deps.Logger.Debug("DeleteWebSearchConfig: delete key best-effort failed", zap.Error(derr))
		}
	}

	s.emitWebSearchAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"websearch.config.delete", []string{"api_key", "engine", "max_results"})
	return &brokerv1.DeleteWebSearchConfigResponse{}, nil
}

// TestWebSearchConfig probes the configured (or supplied-unsaved) engine with
// one minimal query using the supplied-or-stored key. It never writes
// anything — a pure probe, like TestLlmProvider/TestM365Connection.
func (s *BrokerService) TestWebSearchConfig(ctx context.Context, req *brokerv1.TestWebSearchConfigRequest) (*brokerv1.TestWebSearchConfigResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.websearch.test")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	engine, apiKey := s.resolveWebSearchTestInputs(ctx, tenant, req)
	resp := s.runWebSearchProbe(ctx, engine, apiKey)

	s.emitWebSearchAudit(ctx, span.SpanContext().TraceID().String(), tenant, user, "websearch.config.test", nil)
	return resp, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

// webSearchHasKey reports whether an engine api_key is stored for the tenant.
// Any Vault error is treated as "no key" (fail closed on the UI hint, not on
// the RPC itself — Get/Upsert must still succeed).
func (s *BrokerService) webSearchHasKey(ctx context.Context, tenant string) bool {
	ws, ok := s.deps.Secrets.(websearchSecrets)
	if !ok {
		return false
	}
	_, hasKey, err := ws.ReadWebSearchApp(ctx, tenant)
	return err == nil && hasKey
}

// resolveWebSearchTestInputs resolves the engine + api_key to probe: an
// unsaved config from the request takes priority (test-before-save), falling
// back field-by-field to the tenant's stored config — mirroring
// resolveM365TestApp's blank-reuses-stored rule.
func (s *BrokerService) resolveWebSearchTestInputs(ctx context.Context, tenant string, req *brokerv1.TestWebSearchConfigRequest) (engine, apiKey string) {
	engine = req.GetConfig().GetEngine()
	apiKey = req.GetApiKey()

	if engine == "" && s.deps.OrgSettings != nil {
		if settings, gerr := s.deps.OrgSettings.Get(ctx, tenant); gerr == nil {
			engine = settings.WebsearchEngine
		}
	}
	if apiKey == "" {
		if ws, ok := s.deps.Secrets.(websearchSecrets); ok {
			if stored, sok, rerr := ws.ReadWebSearchApp(ctx, tenant); rerr == nil && sok {
				apiKey = stored
			}
		}
	}
	return engine, apiKey
}

// runWebSearchProbe dispatches to the configured engine, reusing the exact
// searchEngine implementation web.search calls (toolproxy.ProbeSearchEngine)
// rather than duplicating three per-engine HTTP shapes in this package
//. An unknown/unset engine or missing key fails
// closed with a typed {ok:false} response — never a default engine
//.
func (s *BrokerService) runWebSearchProbe(ctx context.Context, engine, apiKey string) *brokerv1.TestWebSearchConfigResponse {
	if engine == "" {
		return &brokerv1.TestWebSearchConfigResponse{Ok: false, Detail: "no engine configured"}
	}
	if apiKey == "" {
		return &brokerv1.TestWebSearchConfigResponse{Ok: false, Detail: "no api key supplied and none stored"}
	}

	// Reuses the AllowProviderTestPrivate knob already wired for
	// TestLlmProvider — no new config surface.
	if err := toolproxy.ProbeSearchEngine(ctx, engine, apiKey, s.deps.AllowProviderTestPrivate, webSearchTestTimeout); err != nil {
		return &brokerv1.TestWebSearchConfigResponse{Ok: false, Detail: err.Error()}
	}
	return &brokerv1.TestWebSearchConfigResponse{Ok: true, Detail: fmt.Sprintf("%s search probe succeeded", engine)}
}

// webSearchToProto maps the stored settings + resolved has_key to the wire message.
func webSearchToProto(s db.OrgSettings, hasKey bool) *brokerv1.WebSearchConfig {
	out := &brokerv1.WebSearchConfig{
		Engine:     s.WebsearchEngine,
		MaxResults: int32(s.WebsearchMaxResults),
		HasKey:     hasKey,
		UpdatedBy:  s.UpdatedBy,
	}
	if !s.UpdatedAt.IsZero() {
		out.UpdatedAt = s.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return out
}

// emitWebSearchAudit fires an ALLOW audit event for a web.search config
// mutation or probe. Only field names travel in the event, never values.
func (s *BrokerService) emitWebSearchAudit(ctx context.Context, traceID, tenant, actor, eventType string, changed []string) {
	if s.deps.Audit == nil {
		return
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{"actor": actor})
	if len(changed) > 0 {
		fields := make([]any, len(changed))
		for i, k := range changed {
			fields[i] = k
		}
		if changedList, lerr := structpb.NewList(fields); lerr == nil && ctxStruct != nil {
			ctxStruct.Fields["changed_keys"] = structpb.NewListValue(changedList)
		}
	}
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		ResourceRef: "aikonos:websearchconfig:" + tenant,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}
}
