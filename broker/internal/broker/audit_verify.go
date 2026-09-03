package broker

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

func (s *BrokerService) VerifyAuditChain(ctx context.Context, _ *brokerv1.VerifyAuditChainRequest) (*brokerv1.VerifyAuditChainResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.verify_audit_chain")
	defer span.End()

	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	// Nil reader → logging-only mode; return unconfigured result without error.
	if s.deps.AuditReader == nil {
		return &brokerv1.VerifyAuditChainResponse{StoreConfigured: false}, nil
	}

	report, configured, readErr := s.deps.AuditReader.VerifyChain(ctx, tenant)
	if readErr != nil {
		return nil, status.Errorf(codes.Internal, "audit store read failed: %v", readErr)
	}

	if s.deps.Audit != nil {
		traceID := trace.SpanContextFromContext(ctx).TraceID().String()
		ctxStruct, _ := structpb.NewStruct(map[string]any{
			"total":        report.Total,
			"ok":           report.OK,
			"breaks_count": len(report.Breaks),
		})
		if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
			EventId:     ids.EventID(),
			TraceId:     traceID,
			TenantId:    tenant,
			OccurredAt:  timestampNow(),
			ActorUserId: user,
			EventType:   "aikonos.broker.audit.verified",
			ResourceRef: "aikonos:audit:" + tenant,
			Decision:    auditv1.PolicyDecision_ALLOW,
			Context:     ctxStruct,
		}); err != nil {
			audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.audit.verified")
		}
	}

	resp := &brokerv1.VerifyAuditChainResponse{
		Total:           int32(report.Total),
		Ok:              report.OK,
		Signed:          report.Signed,
		StoreConfigured: configured,
		HeadEventId:     report.HeadID,
		TailEventId:     report.TailID,
		Truncated:       report.Truncated,
		Skipped:         int32(report.Skipped),
	}
	for _, b := range report.Breaks {
		resp.Breaks = append(resp.Breaks, &brokerv1.ChainBreak{
			EventId: b.EventID,
			Detail:  b.Detail,
		})
	}
	for _, sf := range report.SignatureFailures {
		resp.SignatureFailures = append(resp.SignatureFailures, &brokerv1.ChainBreak{
			EventId: sf.EventID,
			Detail:  sf.Detail,
		})
	}
	return resp, nil
}
