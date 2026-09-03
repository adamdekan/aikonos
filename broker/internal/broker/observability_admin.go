package broker

// observability_admin.go — GetObservabilityInfo RPC (read-only).
//
// The OTLP export endpoint is a broker deploy-time env var
// (AIKONOS_OTEL_ENDPOINT → viper "otel.endpoint") consumed once at startup to
// build the MeterProvider. It is process-global, not tenant-scoped, and cannot
// be rewired at runtime — so it is surfaced here for display only. The admin UI
// renders it read-only with a "set via env, restart required" note.

import (
	"context"

	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// exportedJobLabel is the PromQL label value that identifies broker-emitted
// series. The collector scrape overwrites the `job` label to "otel-collector",
// so dashboards must filter on `exported_job="aikonos-broker"` instead.
const exportedJobLabel = "aikonos-broker"

func (s *BrokerService) GetObservabilityInfo(ctx context.Context, _ *brokerv1.GetObservabilityInfoRequest) (*brokerv1.GetObservabilityInfoResponse, error) {
	ctx, span := tracer.Start(ctx, "broker.admin.observability.get")
	defer span.End()

	_, user, err := s.callerIdentity(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if err := s.requireTenantAdmin(ctx, user); err != nil {
		return nil, err
	}

	endpoint := s.deps.OtelEndpoint
	return &brokerv1.GetObservabilityInfoResponse{
		OtelEndpoint:  endpoint,
		ExportEnabled: endpoint != "",
		ExportedJob:   exportedJobLabel,
	}, nil
}
