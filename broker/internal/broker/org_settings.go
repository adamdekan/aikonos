package broker

import (
	"context"
	"sort"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// Org governance settings (A-series). North Get/Update are tenant-admin gated;
// the south Get is SPIFFE-gated to the gateway and read-only — the gateway
// reads the instruction preamble and unattended-mode master at session
// resolution time.

func (s *BrokerService) GetOrgSettings(ctx context.Context, req *brokerv1.GetOrgSettingsRequest) (*brokerv1.GetOrgSettingsResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if s.deps.OrgSettings == nil {
		return &brokerv1.GetOrgSettingsResponse{Settings: &brokerv1.OrgSettings{}}, nil
	}
	settings, err := s.deps.OrgSettings.Get(ctx, tenant)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get org settings: %v", err)
	}
	return &brokerv1.GetOrgSettingsResponse{Settings: orgSettingsToProto(settings)}, nil
}

func (s *BrokerService) UpdateOrgSettings(ctx context.Context, req *brokerv1.UpdateOrgSettingsRequest) (*brokerv1.UpdateOrgSettingsResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}
	if s.deps.OrgSettings == nil {
		return nil, status.Error(codes.FailedPrecondition, "org settings not configured")
	}

	patch, changed := orgSettingsPatch(req.Settings)
	if len(patch) == 0 {
		// Nothing to apply — return current state without a write or audit event.
		settings, gerr := s.deps.OrgSettings.Get(ctx, tenant)
		if gerr != nil {
			return nil, status.Errorf(codes.Internal, "get org settings: %v", gerr)
		}
		return &brokerv1.UpdateOrgSettingsResponse{Settings: orgSettingsToProto(settings)}, nil
	}

	settings, err := s.deps.OrgSettings.Merge(ctx, tenant, patch, user)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update org settings: %v", err)
	}
	s.emitOrgSettingsUpdated(ctx, tenant, user, changed)
	return &brokerv1.UpdateOrgSettingsResponse{Settings: orgSettingsToProto(settings)}, nil
}

// GetOrgSettings (south) lets the gateway read governance settings at session
// resolution time. Gateway-peer gated; read-only; defaults on unset.
func (s *SandboxService) GetOrgSettings(ctx context.Context, req *brokerv1.GetOrgSettingsRequest) (*brokerv1.GetOrgSettingsResponse, error) {
	if err := s.requireGatewayPeer(ctx); err != nil {
		return nil, err
	}
	if req.TenantId == "" {
		return nil, status.Error(codes.InvalidArgument, "tenant_id required")
	}
	if s.deps.OrgSettings == nil {
		return &brokerv1.GetOrgSettingsResponse{Settings: &brokerv1.OrgSettings{}}, nil
	}
	settings, err := s.deps.OrgSettings.Get(ctx, req.TenantId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get org settings: %v", err)
	}
	return &brokerv1.GetOrgSettingsResponse{Settings: orgSettingsToProto(settings)}, nil
}

// orgSettingsToProto maps the stored settings to the wire message, populating
// every field with its effective value.
func orgSettingsToProto(s db.OrgSettings) *brokerv1.OrgSettings {
	out := &brokerv1.OrgSettings{
		UpdatedBy: s.UpdatedBy,
	}
	preamble := s.InstructionPreamble
	out.InstructionPreamble = &preamble
	unattended := s.UnattendedAllowedOrDefault()
	out.UnattendedAllowed = &unattended
	sharing := s.WorkflowSharingAllowedOrDefault()
	out.WorkflowSharingAllowed = &sharing
	connEnabled := s.ConnectorAllowlistEnabled != nil && *s.ConnectorAllowlistEnabled
	out.ConnectorAllowlistEnabled = &connEnabled
	out.ConnectorAllowlist = append([]string(nil), s.ConnectorAllowlist...)
	out.Capabilities = &brokerv1.CapabilityPolicy{
		DisabledEffectClasses: append([]string(nil), s.DisabledEffectClasses...),
	}
	if !s.UpdatedAt.IsZero() {
		out.UpdatedAt = s.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return out
}

// orgSettingsPatch builds the partial-merge patch from only the fields present
// on the request (proto3 `optional` has-bits). Returns the JSON patch map and a
// sorted list of the changed field keys for the audit event. Provenance fields
// (updated_by/updated_at) are never client-writable.
func orgSettingsPatch(in *brokerv1.OrgSettings) (map[string]any, []string) {
	patch := map[string]any{}
	if in == nil {
		return patch, nil
	}
	if in.InstructionPreamble != nil {
		patch["instruction_preamble"] = in.GetInstructionPreamble()
	}
	if in.UnattendedAllowed != nil {
		patch["unattended_allowed"] = in.GetUnattendedAllowed()
	}
	if in.WorkflowSharingAllowed != nil {
		patch["workflow_sharing_allowed"] = in.GetWorkflowSharingAllowed()
	}
	// A7: the enabled flag and the list travel together — presence of the flag
	// signals the connector-allowlist page owns this update and applies both.
	if in.ConnectorAllowlistEnabled != nil {
		patch["connector_allowlist_enabled"] = in.GetConnectorAllowlistEnabled()
		patch["connector_allowlist"] = append([]string(nil), in.GetConnectorAllowlist()...)
	}
	// A2: presence of the wrapped capabilities message signals the page owns
	// this update and replaces the disabled-effect-class set.
	if in.Capabilities != nil {
		patch["disabled_effect_classes"] = append([]string(nil), in.Capabilities.GetDisabledEffectClasses()...)
	}
	changed := make([]string, 0, len(patch))
	for k := range patch {
		changed = append(changed, k)
	}
	sort.Strings(changed)
	return patch, changed
}

// requireWorkflowSharingAllowed enforces the A6 org master before a publish.
// Fails closed on a settings read error (governance guard). When the repo is
// unconfigured, sharing is allowed (feature-not-configured → no restriction).
func requireWorkflowSharingAllowed(ctx context.Context, repo *db.OrgSettingsRepo, tenant string) error {
	if repo == nil {
		return nil
	}
	settings, err := repo.Get(ctx, tenant)
	if err != nil {
		return status.Errorf(codes.Internal, "org settings read: %v", err)
	}
	if !settings.WorkflowSharingAllowedOrDefault() {
		return status.Error(codes.PermissionDenied, "workflow sharing is disabled for this organization")
	}
	return nil
}

// emitOrgSettingsUpdated records a config-change audit event. It carries the
// changed field keys (never their values) so the WORM trail never persists
// governance-config content, and the actor.
func (s *BrokerService) emitOrgSettingsUpdated(ctx context.Context, tenantID, actor string, changed []string) {
	if s.deps.Audit == nil {
		return
	}
	fields := make([]any, len(changed))
	for i, k := range changed {
		fields[i] = k
	}
	changedList, _ := structpb.NewList(fields)
	ctxStruct, _ := structpb.NewStruct(map[string]any{
		"actor":         actor,
		"changed_count": float64(len(changed)),
	})
	if changedList != nil {
		ctxStruct.Fields["changed_keys"] = structpb.NewListValue(changedList)
	}
	if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:    ids.EventID(),
		TenantId:   tenantID,
		OccurredAt: timestampNow(),
		EventType:   "aikonos.broker.org_settings.updated",
		ActorUserId: actor,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.org_settings.updated")
	}
}
