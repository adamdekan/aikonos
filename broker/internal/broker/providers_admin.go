package broker

// providers_admin.go — north-bound LLM provider admin RPCs.
// Gate: callerIdentity (rejects forged user_id + svc- subjects) then
// requireTenantAdmin. Pattern matches roles.go + config_admin.go exactly.

import (
	"context"
	"errors"
	"net/url"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// LlmProviderStore is the minimal db.LlmProviderRepo surface the broker
// handlers need. *db.LlmProviderRepo satisfies this; tests inject a fake.
type LlmProviderStore interface {
	List(ctx context.Context, tenant string) ([]db.Provider, error)
	Get(ctx context.Context, tenant, id string) (db.Provider, error)
	Upsert(ctx context.Context, tenant string, p db.Provider) error
	Delete(ctx context.Context, tenant, id string) error
	SetDefault(ctx context.Context, tenant, id string) error
	SetDefaultVision(ctx context.Context, tenant, id string) error
	SetFallback(ctx context.Context, tenant, id string) error
	SetDefaultFor(ctx context.Context, tenant, capability, providerID string) error
	DefaultsFor(ctx context.Context, tenant string) (map[string]string, error)
}

// Provider families (the `api` wire value). A family bundles auth mechanism +
// URL shape + body shape. google-gemini and aws-bedrock are OpenAI-compatible
// on the wire (Bearer + /chat/completions against
// generativelanguage.googleapis.com/v1beta/openai and
// bedrock-runtime.<region>.amazonaws.com/openai/v1); they exist as distinct ids
// for family-specific validation, placeholders and help text.
const (
	apiOpenAICompletions = "openai-completions"
	apiAnthropicMessages = "anthropic-messages"
	// apiAzureOpenAI is the classic Azure AI Foundry deployment route
	// (api-key header + ?api-version=<ver> + /openai/deployments/<name>).
	apiAzureOpenAI  = "azure-openai"
	apiGoogleGemini = "google-gemini"
	apiAWSBedrock   = "aws-bedrock"
)

// allowedAPIs is the closed set of LLM wire-protocol values the broker accepts.
var allowedAPIs = map[string]bool{
	apiOpenAICompletions: true,
	apiAnthropicMessages: true,
	apiAzureOpenAI:       true,
	apiGoogleGemini:      true,
	apiAWSBedrock:        true,
}

// Per-model modalities. Empty means chat, so a pre-catalog row keeps working.
const (
	modeChat               = "chat"
	modeEmbedding          = "embedding"
	modeImageGeneration    = "image_generation"
	modeAudioSpeech        = "audio_speech"
	modeAudioTranscription = "audio_transcription"
	modeRerank             = "rerank"
	modeOCR                = "ocr"
	modeModeration         = "moderation"
)

var allowedModelModes = map[string]bool{
	modeChat:               true,
	modeEmbedding:          true,
	modeImageGeneration:    true,
	modeAudioSpeech:        true,
	modeAudioTranscription: true,
	modeRerank:             true,
	modeOCR:                true,
	modeModeration:         true,
}

// allowedPricingUnits is the closed set of billing units a model's pricing may
// declare. Only per_mtok has a token-costing basis today (see costMicrosFor);
// the rest are configurable-but-not-yet-executable modalities.
var allowedPricingUnits = map[string]bool{
	db.PricingUnitPerMTok: true,
	"per_image":           true,
	"per_megapixel":       true,
	"per_mchar":           true,
	"per_minute":          true,
	"per_second":          true,
	"per_page":            true,
	"per_1k_queries":      true,
	"per_request":         true,
	"free":                true,
}

// allowedCapabilities is the closed set of llm_provider_defaults capabilities.
// chat/vision/fallback have legacy boolean twins; the rest are modalities whose
// default names the provider a future consumer would route that mode to.
var allowedCapabilities = map[string]bool{
	db.CapabilityChat:      true,
	db.CapabilityVision:    true,
	db.CapabilityFallback:  true,
	modeEmbedding:          true,
	modeImageGeneration:    true,
	modeAudioSpeech:        true,
	modeAudioTranscription: true,
	modeRerank:             true,
	modeOCR:                true,
}

// providerConfigKeys is the closed set of family-specific config keys. A family
// absent from the map accepts no config at all (azure's api-version keeps its
// own column). Unknown keys are rejected so a typo cannot silently do nothing.
var providerConfigKeys = map[string]map[string]bool{
	apiAWSBedrock: {"region": true},
}

// effectiveMode resolves a stored/requested mode to its meaning: empty is chat.
func effectiveMode(mode string) string {
	if mode == "" {
		return modeChat
	}
	return mode
}

// providersFor returns the LlmProviderStore or nil when Providers is unset.
func (s *BrokerService) providersFor() LlmProviderStore {
	return s.deps.Providers
}

// ── North RPCs ────────────────────────────────────────────────────────────────

func (s *BrokerService) ListLlmProviders(ctx context.Context, _ *brokerv1.ListLlmProvidersRequest) (*brokerv1.ListLlmProvidersResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.llm.list")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	store := s.providersFor()
	if store == nil {
		return &brokerv1.ListLlmProvidersResponse{}, nil
	}

	rows, err := store.List(ctx, tenant)
	if err != nil {
		s.deps.Logger.Warn("ListLlmProviders: repo error", zap.String("tenant", tenant), zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to list providers")
	}

	out := make([]*brokerv1.LlmProvider, 0, len(rows))
	for _, r := range rows {
		out = append(out, providerToProtoAdmin(r))
	}

	// The defaults map is the authoritative view; the per-provider booleans on
	// each row are the same data recomputed for the gateway's older readers.
	defaults, err := store.DefaultsFor(ctx, tenant)
	if err != nil {
		s.deps.Logger.Warn("ListLlmProviders: defaults error", zap.String("tenant", tenant), zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to list provider defaults")
	}
	return &brokerv1.ListLlmProvidersResponse{Providers: out, Defaults: defaults}, nil
}

func (s *BrokerService) UpsertLlmProvider(ctx context.Context, req *brokerv1.UpsertLlmProviderRequest) (*brokerv1.UpsertLlmProviderResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.llm.upsert")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	p := req.GetProvider()
	if p == nil {
		return nil, status.Error(codes.InvalidArgument, "provider required")
	}
	if err := validateProvider(p); err != nil {
		return nil, err
	}

	// Key handling: write to Vault when provided, otherwise preserve the
	// stored has_key value for an existing provider (empty key on update must
	// not silently wipe a stored secret).
	hasKey := false
	if req.ApiKey != "" {
		if s.deps.Secrets != nil {
			if werr := s.deps.Secrets.WriteProviderKey(ctx, tenant, p.Id, req.ApiKey); werr != nil {
				s.deps.Logger.Warn("UpsertLlmProvider: write key failed", zap.Error(werr))
				return nil, status.Errorf(codes.Internal, "failed to store provider key")
			}
		}
		hasKey = true
	} else if store := s.providersFor(); store != nil {
		// Preserve existing has_key value when no new key supplied.
		existing, getErr := store.Get(ctx, tenant, p.Id)
		if getErr == nil {
			hasKey = existing.HasKey
		}
		// ErrProviderNotFound → new provider with no key → hasKey stays false.
	}

	row := db.Provider{
		ID:                    p.Id,
		Name:                  p.Name,
		Endpoint:              p.Endpoint,
		API:                   p.Api,
		APIVersion:            p.ApiVersion,
		Models:                modelsFromProto(p.Models),
		Enabled:               p.Enabled,
		VisionCapable:         p.VisionCapable,
		HasKey:                hasKey,
		PriceInMicrosPerMTok:  p.PriceInMicrosPerMtok,
		PriceOutMicrosPerMTok: p.PriceOutMicrosPerMtok,
		Config:                p.Config,
		UpdatedBy:             user,
	}

	store := s.providersFor()
	if store == nil {
		return nil, status.Error(codes.FailedPrecondition, "provider store not configured")
	}
	if err := store.Upsert(ctx, tenant, row); err != nil {
		s.deps.Logger.Warn("UpsertLlmProvider: repo error", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to upsert provider")
	}

	s.emitLlmProviderAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"llm.provider.upsert", p.Id)
	return &brokerv1.UpsertLlmProviderResponse{}, nil
}

func (s *BrokerService) DeleteLlmProvider(ctx context.Context, req *brokerv1.DeleteLlmProviderRequest) (*brokerv1.DeleteLlmProviderResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.llm.delete")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "id required")
	}

	store := s.providersFor()
	if store != nil {
		if err := store.Delete(ctx, tenant, req.Id); err != nil {
			s.deps.Logger.Warn("DeleteLlmProvider: repo error", zap.Error(err))
			return nil, status.Errorf(codes.Internal, "failed to delete provider")
		}
	}
	// Best-effort: delete key even if the DB row was already absent.
	if s.deps.Secrets != nil {
		if err := s.deps.Secrets.DeleteProviderKey(ctx, tenant, req.Id); err != nil {
			s.deps.Logger.Debug("DeleteLlmProvider: delete key best-effort failed", zap.Error(err))
		}
	}

	s.emitLlmProviderAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"llm.provider.delete", req.Id)
	return &brokerv1.DeleteLlmProviderResponse{}, nil
}

// The three SetX RPCs below keep their proto surface but carry no logic of
// their own: each names a capability and routes through setDefaultForCapability
// so an eligibility rule can never drift between them and
// SetDefaultProviderFor.

func (s *BrokerService) SetDefaultProvider(ctx context.Context, req *brokerv1.SetDefaultProviderRequest) (*brokerv1.SetDefaultProviderResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.llm.set_default")
	defer span.End()

	if err := s.setDefaultForCapability(ctx, span.SpanContext().TraceID().String(),
		db.CapabilityChat, req.Id, "llm.provider.set_default", false); err != nil {
		return nil, err
	}
	return &brokerv1.SetDefaultProviderResponse{}, nil
}

// SetFallbackProvider marks a provider as the tenant fallback. Deliberately
// unguarded by vision_capable (unlike SetDefaultVisionProvider): the fallback
// serves chat, and the vision-capable requirement is applied at selection time
// in the gateway.
func (s *BrokerService) SetFallbackProvider(ctx context.Context, req *brokerv1.SetFallbackProviderRequest) (*brokerv1.SetFallbackProviderResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.llm.set_fallback")
	defer span.End()

	if err := s.setDefaultForCapability(ctx, span.SpanContext().TraceID().String(),
		db.CapabilityFallback, req.Id, "llm.provider.set_fallback", false); err != nil {
		return nil, err
	}
	return &brokerv1.SetFallbackProviderResponse{}, nil
}

func (s *BrokerService) SetDefaultVisionProvider(ctx context.Context, req *brokerv1.SetDefaultVisionProviderRequest) (*brokerv1.SetDefaultVisionProviderResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.llm.set_default_vision")
	defer span.End()

	if err := s.setDefaultForCapability(ctx, span.SpanContext().TraceID().String(),
		db.CapabilityVision, req.Id, "llm.provider.set_default_vision", false); err != nil {
		return nil, err
	}
	return &brokerv1.SetDefaultVisionProviderResponse{}, nil
}

// SetDefaultProviderFor points one capability at a provider, or clears it with
// an empty provider_id. Generalizes the three RPCs above: a new capability needs
// no new column, index, RPC or UI control.
func (s *BrokerService) SetDefaultProviderFor(ctx context.Context, req *brokerv1.SetDefaultProviderForRequest) (*brokerv1.SetDefaultProviderForResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.llm.set_default_for")
	defer span.End()

	if err := s.setDefaultForCapability(ctx, span.SpanContext().TraceID().String(),
		req.Capability, req.ProviderId, "llm.provider.set_default_for", true); err != nil {
		return nil, err
	}
	return &brokerv1.SetDefaultProviderForResponse{}, nil
}

// setDefaultForCapability is the single authority path behind every
// capability-default mutation: admin gate → capability + eligibility check →
// one llm_provider_defaults write → one audit event. An empty providerID clears
// the capability; allowClear=false is for the three legacy RPCs, whose contract
// has no clear verb and whose empty id is a caller mistake.
func (s *BrokerService) setDefaultForCapability(ctx context.Context, traceID, capability, providerID, eventType string, allowClear bool) error {
	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return err
	}
	if providerID == "" && !allowClear {
		return status.Error(codes.InvalidArgument, "id required")
	}
	if !allowedCapabilities[capability] {
		return status.Errorf(codes.InvalidArgument, "unknown capability %q", capability)
	}

	store := s.providersFor()
	if store == nil {
		return status.Error(codes.FailedPrecondition, "provider store not configured")
	}

	if providerID != "" {
		p, gerr := store.Get(ctx, tenant, providerID)
		if gerr != nil {
			if errors.Is(gerr, db.ErrProviderNotFound) {
				return status.Errorf(codes.NotFound, "provider %q not found", providerID)
			}
			s.deps.Logger.Warn("setDefaultForCapability: get error", zap.Error(gerr))
			return status.Errorf(codes.Internal, "failed to look up provider")
		}
		if err := capabilityEligible(capability, p); err != nil {
			return err
		}
	}

	if err := store.SetDefaultFor(ctx, tenant, capability, providerID); err != nil {
		if errors.Is(err, db.ErrProviderNotFound) {
			return status.Errorf(codes.NotFound, "provider %q not found", providerID)
		}
		s.deps.Logger.Warn("setDefaultForCapability: repo error",
			zap.String("capability", capability), zap.Error(err))
		return status.Errorf(codes.Internal, "failed to set %s default", capability)
	}

	s.emitLlmProviderAudit(ctx, traceID, tenant, user, eventType, providerID)
	return nil
}

// capabilityEligible rejects a provider that cannot serve the capability at all.
// vision fails closed on the provider-level flag (analyze_image would otherwise
// route image content to a text-only provider); chat and fallback need any
// model; every modality capability needs a model declaring that mode, so a
// default can never name a provider with no route for it.
func capabilityEligible(capability string, p db.Provider) error {
	switch capability {
	case db.CapabilityVision:
		if !p.VisionCapable {
			return status.Errorf(codes.InvalidArgument, "provider %q is not vision_capable", p.ID)
		}
		return nil
	case db.CapabilityChat, db.CapabilityFallback:
		if len(p.Models) == 0 {
			return status.Errorf(codes.InvalidArgument, "provider %q has no models", p.ID)
		}
		return nil
	}
	for _, m := range p.Models {
		if effectiveMode(m.Mode) == capability {
			return nil
		}
	}
	return status.Errorf(codes.InvalidArgument, "provider %q has no %s model", p.ID, capability)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// validateProvider checks the fields the spec requires before any DB write.
func validateProvider(p *brokerv1.LlmProvider) error {
	if p.Id == "" {
		return status.Error(codes.InvalidArgument, "provider.id required")
	}
	if p.Name == "" {
		return status.Error(codes.InvalidArgument, "provider.name required")
	}
	u, err := url.Parse(p.Endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return status.Errorf(codes.InvalidArgument, "endpoint must be an http/https URL, got %q", p.Endpoint)
	}
	if len(p.Models) == 0 {
		return status.Error(codes.InvalidArgument, "at least one model required")
	}
	for _, m := range p.Models {
		if m.PriceIn < 0 || m.PriceOut < 0 || m.PriceCacheRead < 0 || m.PriceCacheWrite < 0 {
			return status.Errorf(codes.InvalidArgument, "model %q has a negative price", m.Id)
		}
		if m.Mode != "" && !allowedModelModes[m.Mode] {
			return status.Errorf(codes.InvalidArgument, "model %q has unknown mode %q", m.Id, m.Mode)
		}
		if err := validateModelPricing(m.Id, m.Pricing); err != nil {
			return err
		}
	}
	if p.PriceInMicrosPerMtok < 0 || p.PriceOutMicrosPerMtok < 0 {
		return status.Error(codes.InvalidArgument, "price_in_micros_per_mtok / price_out_micros_per_mtok must not be negative")
	}
	if !allowedAPIs[p.Api] {
		return status.Errorf(codes.InvalidArgument,
			"api must be one of {openai-completions, anthropic-messages, azure-openai, google-gemini, aws-bedrock}, got %q", p.Api)
	}
	// The classic Azure deployment route requires an api-version query parameter;
	// without it every upstream request is a 404. Fail fast at config time.
	if p.Api == apiAzureOpenAI && p.ApiVersion == "" {
		return status.Error(codes.InvalidArgument, "api_version is required when api is azure-openai")
	}
	for k := range p.Config {
		if !providerConfigKeys[p.Api][k] {
			return status.Errorf(codes.InvalidArgument, "config key %q is not valid for api %q", k, p.Api)
		}
	}
	return nil
}

// validateModelPricing enforces the unit-discriminated pricing contract: a
// known unit, no negative amounts, and tiers only where a token basis exists to
// tier (per_mtok), strictly ascending so costMicrosFor's "last matching tier
// wins" scan is unambiguous.
func validateModelPricing(modelID string, pr *brokerv1.LlmModelPricing) error {
	if pr == nil {
		return nil
	}
	if !allowedPricingUnits[pr.Unit] {
		return status.Errorf(codes.InvalidArgument, "model %q has unknown pricing unit %q", modelID, pr.Unit)
	}
	if pr.InMicros < 0 || pr.OutMicros < 0 || pr.CacheReadMicros < 0 || pr.CacheWriteMicros < 0 {
		return status.Errorf(codes.InvalidArgument, "model %q has a negative pricing amount", modelID)
	}
	if len(pr.Tiers) > 0 && pr.Unit != db.PricingUnitPerMTok {
		return status.Errorf(codes.InvalidArgument, "model %q: pricing tiers require unit %q, got %q", modelID, db.PricingUnitPerMTok, pr.Unit)
	}
	var prev int64
	for _, t := range pr.Tiers {
		if t.MinContextTokens <= 0 {
			return status.Errorf(codes.InvalidArgument, "model %q: tier min_context_tokens must be > 0", modelID)
		}
		if t.MinContextTokens <= prev {
			return status.Errorf(codes.InvalidArgument, "model %q: pricing tiers must ascend by min_context_tokens", modelID)
		}
		if t.InMicros < 0 || t.OutMicros < 0 {
			return status.Errorf(codes.InvalidArgument, "model %q: tier has a negative amount", modelID)
		}
		prev = t.MinContextTokens
	}
	return nil
}

// providerToProtoAdmin converts a db.Provider to the north-wire form: api_key
// is always empty (stripped), has_key is set from the stored boolean.
func providerToProtoAdmin(r db.Provider) *brokerv1.LlmProvider {
	return &brokerv1.LlmProvider{
		Id:                    r.ID,
		Name:                  r.Name,
		Endpoint:              r.Endpoint,
		Api:                   r.API,
		ApiVersion:            r.APIVersion,
		Models:                modelsToProto(r.Models),
		Enabled:               r.Enabled,
		IsDefault:             r.IsDefault,
		VisionCapable:         r.VisionCapable,
		IsDefaultVision:       r.IsDefaultVision,
		IsFallback:            r.IsFallback,
		HasKey:                r.HasKey,
		PriceInMicrosPerMtok:  r.PriceInMicrosPerMTok,
		PriceOutMicrosPerMtok: r.PriceOutMicrosPerMTok,
		Config:                r.Config,
		UpdatedBy:             r.UpdatedBy,
		// ApiKey intentionally omitted — north List must never return key bytes.
	}
}

func modelsToProto(ms []db.Model) []*brokerv1.LlmModel {
	out := make([]*brokerv1.LlmModel, 0, len(ms))
	for _, m := range ms {
		out = append(out, &brokerv1.LlmModel{
			Id:              m.ID,
			PriceIn:         m.PriceIn,
			PriceOut:        m.PriceOut,
			PriceCacheRead:  m.PriceCacheRead,
			PriceCacheWrite: m.PriceCacheWrite,
			ContextWindow:   int32(m.ContextWindow),
			MaxTokens:       int32(m.MaxTokens),
			Mode:            m.Mode,
			Pricing:         pricingToProto(m.Pricing),
		})
	}
	return out
}

func modelsFromProto(ms []*brokerv1.LlmModel) []db.Model {
	out := make([]db.Model, 0, len(ms))
	for _, m := range ms {
		out = append(out, db.Model{
			ID:              m.Id,
			PriceIn:         m.PriceIn,
			PriceOut:        m.PriceOut,
			PriceCacheRead:  m.PriceCacheRead,
			PriceCacheWrite: m.PriceCacheWrite,
			ContextWindow:   int(m.ContextWindow),
			MaxTokens:       int(m.MaxTokens),
			Mode:            m.Mode,
			Pricing:         pricingFromProto(m.Pricing),
		})
	}
	return out
}

func pricingToProto(pr *db.ModelPricing) *brokerv1.LlmModelPricing {
	if pr == nil {
		return nil
	}
	out := &brokerv1.LlmModelPricing{
		Unit:             pr.Unit,
		InMicros:         pr.InMicros,
		OutMicros:        pr.OutMicros,
		CacheReadMicros:  pr.CacheReadMicros,
		CacheWriteMicros: pr.CacheWriteMicros,
	}
	for _, t := range pr.Tiers {
		out.Tiers = append(out.Tiers, &brokerv1.LlmPricingTier{
			MinContextTokens: t.MinContextTokens,
			InMicros:         t.InMicros,
			OutMicros:        t.OutMicros,
		})
	}
	return out
}

func pricingFromProto(pr *brokerv1.LlmModelPricing) *db.ModelPricing {
	if pr == nil {
		return nil
	}
	out := &db.ModelPricing{
		Unit:             pr.Unit,
		InMicros:         pr.InMicros,
		OutMicros:        pr.OutMicros,
		CacheReadMicros:  pr.CacheReadMicros,
		CacheWriteMicros: pr.CacheWriteMicros,
	}
	for _, t := range pr.Tiers {
		out.Tiers = append(out.Tiers, db.PricingTier{
			MinContextTokens: t.MinContextTokens,
			InMicros:         t.InMicros,
			OutMicros:        t.OutMicros,
		})
	}
	return out
}

// emitLlmProviderAudit fires an ALLOW audit event for LLM provider mutations.
func (s *BrokerService) emitLlmProviderAudit(ctx context.Context, traceID, tenant, actor, eventType, providerID string) {
	if s.deps.Audit == nil {
		return
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{
		"actor":       actor,
		"provider_id": providerID,
	})
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		ResourceRef: "aikonos:llmprovider:" + providerID,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}
}
