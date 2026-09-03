package broker

// config_admin.go — GetPlatformConfig / SetPlatformConfig RPCs.
// Both are tenant-admin gated. The Config interface is injected via Deps so
// tests can substitute a fake without a real DB.

import (
	"context"
	"errors"
	"strconv"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/config"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/notify"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// Config is the tenant-scoped runtime config interface injected into Deps.
// Satisfied by *config.Store; fakeable in tests.
type Config interface {
	GetInt(ctx context.Context, tenant, key string) int
	GetString(ctx context.Context, tenant, key string) string
	List(ctx context.Context, tenant string) ([]config.Entry, error)
	Set(ctx context.Context, tenant, key, value, actor string) error
}

// nopConfig is returned by configFor when Deps.Config is nil. It always
// returns schema defaults so the consumer (approval expiry) is safe even
// when the DB is absent.
type nopConfig struct{}

func (nopConfig) GetInt(_ context.Context, _, key string) int {
	k, ok := config.Schema[key]
	if !ok {
		return 0
	}
	n, _ := strconv.Atoi(k.Default)
	return n
}

func (nopConfig) GetString(_ context.Context, _, key string) string {
	k, ok := config.Schema[key]
	if !ok {
		return ""
	}
	return k.Default
}

func (nopConfig) List(_ context.Context, _ string) ([]config.Entry, error) {
	out := make([]config.Entry, 0, len(config.Schema))
	for name, k := range config.Schema {
		out = append(out, config.Entry{
			Key:     name,
			Value:   k.Default,
			Default: k.Default,
			Kind:    k.Kind,
			Doc:     k.Doc,
		})
	}
	return out, nil
}

func (nopConfig) Set(_ context.Context, _, _, _, _ string) error {
	return errors.New("config store not configured")
}

// configFor returns the injected Config store or a safe nopConfig fallback.
func (s *BrokerService) configFor() Config {
	if s.deps.Config != nil {
		return s.deps.Config
	}
	return nopConfig{}
}

// cfg returns the injected Config store or a safe nopConfig fallback. Mirrors
// BrokerService.configFor for the south-bound SandboxService (SubmitPlan +
// InvokeTool read disabled_tools). Nil-safe: a nil store disables nothing.
func (s *SandboxService) cfg() Config {
	if s.deps.Config != nil {
		return s.deps.Config
	}
	return nopConfig{}
}

// kindString maps config.Kind to the wire string used by the UI.
func kindString(k config.Kind) string {
	switch k {
	case config.KindInt:
		return "int"
	case config.KindBool:
		return "bool"
	default:
		return "string"
	}
}

func (s *BrokerService) GetPlatformConfig(ctx context.Context, _ *brokerv1.GetPlatformConfigRequest) (*brokerv1.GetPlatformConfigResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.config.get")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	entries, err := s.configFor().List(ctx, tenant)
	if err != nil {
		s.deps.Logger.Warn("GetPlatformConfig: List failed", zap.String("tenant", tenant), zap.Error(err))
		// fail-safe: List already returns defaults on error, but if a real error
		// propagated here, surface it rather than silently returning stale data.
		return nil, status.Errorf(codes.Internal, "failed to read config: %v", err)
	}

	out := make([]*brokerv1.ConfigEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, &brokerv1.ConfigEntry{
			Key:          e.Key,
			Value:        e.Value,
			DefaultValue: e.Default,
			Kind:         kindString(e.Kind),
			Doc:          e.Doc,
		})
	}

	s.emitConfigAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.config.listed", "", "")
	return &brokerv1.GetPlatformConfigResponse{Entries: out}, nil
}

func (s *BrokerService) SetPlatformConfig(ctx context.Context, req *brokerv1.SetPlatformConfigRequest) (*brokerv1.SetPlatformConfigResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.config.set")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	if err := s.configFor().Set(ctx, tenant, req.Key, req.Value, user); err != nil {
		if errors.Is(err, config.ErrUnknownKey) {
			return nil, status.Errorf(codes.InvalidArgument, "unknown config key %q", req.Key)
		}
		// Validate errors carry a user-readable message (range check, type mismatch).
		// Any other error is a repo/DB failure — surface as Internal without leaking
		// driver details.
		if isConfigValidationErr(err) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid config value: %v", err)
		}
		s.deps.Logger.Warn("SetPlatformConfig: repo error", zap.String("tenant", tenant), zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to set config")
	}

	s.emitConfigChanged(ctx, span.SpanContext().TraceID().String(), tenant, user, req.Key, req.Value)
	s.emitConfigAudit(ctx, span.SpanContext().TraceID().String(), tenant, user,
		"aikonos.broker.config.changed", req.Key, req.Value)
	return &brokerv1.SetPlatformConfigResponse{}, nil
}

// emitConfigChanged publishes a fire-and-forget NATS event for config changes.
// Nil-guarded; uses deps.Notify when available.
func (s *BrokerService) emitConfigChanged(ctx context.Context, _, tenant, actor, key, value string) {
	if s.deps.Notify == nil || !s.deps.Notify.Enabled() {
		return
	}
	ev := &notify.Event{
		EventID:    ids.EventID(),
		TenantID:   tenant,
		Type:       "CONFIG_CHANGED",
		Payload:    map[string]any{"key": key, "value": value, "actor": actor},
		OccurredAt: time.Now().UTC(),
	}
	subject := "aikonos.broker.config.changed." + tenant
	go func() {
		if err := notify.PublishEvent(ctx, s.deps.Notify, subject, ev); err != nil {
			s.deps.Logger.Debug("emitConfigChanged: publish failed", zap.Error(err))
		}
	}()
}

// emitConfigAudit fires an ALLOW audit event for config read/write actions.
// Nil-guarded on deps.Audit.
func (s *BrokerService) emitConfigAudit(ctx context.Context, traceID, tenant, actor, eventType, key, value string) {
	if s.deps.Audit == nil {
		return
	}
	fields := map[string]any{"actor": actor}
	if key != "" {
		fields["key"] = key
		fields["value"] = value
	}
	ctxStruct, _ := structpb.NewStruct(fields)
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestampNow(),
		ActorUserId: actor,
		EventType:   eventType,
		ResourceRef: "aikonos:tenant:" + tenant,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, eventType)
	}
}

// isConfigValidationErr reports whether err is a schema Validate rejection
// (user-input error) rather than a repo/DB failure.
func isConfigValidationErr(err error) bool {
	var ve *config.ValidationError
	return errors.As(err, &ve)
}

// configForSandbox returns the Config for use inside the SandboxService
// (SubmitPlan approval expiry consumer). Mirrors configFor on BrokerService.
func (s *SandboxService) configFor() Config {
	if s.deps.Config != nil {
		return s.deps.Config
	}
	return nopConfig{}
}
