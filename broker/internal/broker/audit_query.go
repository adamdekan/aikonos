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

func (s *BrokerService) QueryAudit(ctx context.Context, req *brokerv1.QueryAuditRequest) (*brokerv1.QueryAuditResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.query_audit")
	defer span.End()

	// Tenant comes only from the verified caller identity — never from the request.
	tenant, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	// Nil reader → logging-only mode; return empty result without error.
	if s.deps.AuditReader == nil {
		return &brokerv1.QueryAuditResponse{StoreConfigured: false}, nil
	}

	f := audit.QueryFilter{
		ActorUserID: req.ActorUserId,
		EventType:   req.EventType,
		Decision:    req.Decision,
		Limit:       int(req.Limit),
		Cursor:      req.Cursor,
	}
	if req.StartTime != nil {
		f.StartTime = req.StartTime.AsTime()
	}
	if req.EndTime != nil {
		f.EndTime = req.EndTime.AsTime()
	}

	events, nextCursor, ok, readErr := s.deps.AuditReader.Query(ctx, tenant, f)
	if readErr != nil {
		return nil, status.Errorf(codes.Internal, "failed to read audit events: %v", readErr)
	}

	// Emit a aikonos.broker.audit.queried ALLOW event (fire-and-forget).
	if s.deps.Audit != nil {
		traceID := trace.SpanContextFromContext(ctx).TraceID().String()
		ctxStruct, _ := structpb.NewStruct(map[string]any{
			"actor_filter":      req.ActorUserId,
			"event_type_filter": req.EventType,
			"limit":             req.Limit,
			"cursor":            req.Cursor,
		})
		if err := s.deps.Audit.Emit(ctx, &auditv1.AuditEvent{
			EventId:     ids.EventID(),
			TraceId:     traceID,
			TenantId:    tenant,
			OccurredAt:  timestampNow(),
			ActorUserId: user,
			EventType:   "aikonos.broker.audit.queried",
			ResourceRef: "aikonos:audit:" + tenant,
			Decision:    auditv1.PolicyDecision_ALLOW,
			Context:     ctxStruct,
		}); err != nil {
			audit.RecordEmitFailure(ctx, s.deps.Logger, err, "aikonos.broker.audit.queried")
		}
	}

	return &brokerv1.QueryAuditResponse{
		Events:          events,
		NextCursor:      nextCursor,
		StoreConfigured: ok,
	}, nil
}
