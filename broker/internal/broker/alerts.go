package broker

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/alerting"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const defaultAlertLimit = 100

// ListAlerts returns recent security alerts for the caller's tenant.
// Tenant-admin gated: reuses requireTenantAdmin + callerIdentity.
func (s *BrokerService) ListAlerts(ctx context.Context, req *brokerv1.ListAlertsRequest) (*brokerv1.ListAlertsResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, req.UserId)
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	if s.deps.AlertRepo == nil {
		return &brokerv1.ListAlertsResponse{}, nil
	}

	limit := int(req.Limit)
	if limit <= 0 {
		limit = defaultAlertLimit
	}

	// tenant is the UUID validated by callerIdentity — matches the UUID stored
	// by the alerting engine, which uses ev.TenantId from the audit event.
	rows, err := s.deps.AlertRepo.List(ctx, tenant, limit)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list alerts: %v", err)
	}

	out := make([]*brokerv1.AlertRecord, 0, len(rows))
	for _, r := range rows {
		out = append(out, storedAlertToProto(r))
	}
	return &brokerv1.ListAlertsResponse{Alerts: out}, nil
}

func storedAlertToProto(a alerting.StoredAlert) *brokerv1.AlertRecord {
	summary, _ := structpb.NewStruct(a.Summary)
	return &brokerv1.AlertRecord{
		AlertId:  a.ID,
		TenantId: a.TenantID,
		Rule:     a.Rule,
		Severity: a.Severity,
		Summary:  summary,
		FiredAt:  timestamppb.New(a.FiredAt),
	}
}
