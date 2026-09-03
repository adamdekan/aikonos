// broker/internal/audit/metrics.go — OTel counter for dropped audit emits.
// The counter is registered once on the global meter (same pattern as
// broker/internal/metrics/llm.go). A no-op MeterProvider produces a no-op
// counter, so callers need no nil guard.
package audit

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
)

const auditMeterName = "github.com/adamdekan/aikonos/broker/audit"

var auditEmitFailures metric.Int64Counter
var auditEventsDropped metric.Int64Counter
var auditChainUnsigned metric.Int64Counter

func init() {
	m := otel.Meter(auditMeterName)
	auditEmitFailures, _ = m.Int64Counter(
		"audit_emit_failures_total",
		metric.WithDescription("Number of audit Emit calls that returned a non-nil error (compliance trail gap)"),
	)
	auditEventsDropped, _ = m.Int64Counter(
		"audit_events_dropped_total",
		metric.WithDescription("Number of audit events dropped due to a full upload queue (queue-full backpressure)"),
	)
	auditChainUnsigned, _ = m.Int64Counter(
		"audit_chain_unsigned_total",
		metric.WithDescription("Number of VerifyChain checks that found a tenant's entire (non-empty) audit chain chained but unsigned — the Vault audit-signing key was unavailable for every event; was boot-log-only before this counter"),
	)
}

// RecordEmitFailure logs a dropped audit event at Error and increments the
// audit_emit_failures_total OTel counter. logger may be nil — the counter is
// always incremented regardless. eventType labels the counter for alerting.
func RecordEmitFailure(ctx context.Context, logger *zap.Logger, err error, eventType string) {
	auditEmitFailures.Add(ctx, 1, metric.WithAttributes(attribute.String("event_type", eventType)))
	if logger != nil && err != nil {
		logger.Error("audit emit failed — compliance trail gap",
			zap.String("event_type", eventType),
			zap.Error(err),
		)
	}
}

// RecordEventDropped logs a dropped (queue-full) audit event at Error and
// increments audit_events_dropped_total, dimensioned by event_type.
// logger may be nil — the counter is always incremented regardless.
func RecordEventDropped(ctx context.Context, logger *zap.Logger, eventType, tenant, eventID string) {
	auditEventsDropped.Add(ctx, 1, metric.WithAttributes(attribute.String("event_type", eventType)))
	if logger != nil {
		logger.Error("audit: upload queue full, event NOT persisted to MinIO",
			zap.String("event_type", eventType),
			zap.String("tenant_id", tenant),
			zap.String("event_id", eventID),
		)
	}
}

// RecordChainUnsigned increments audit_chain_unsigned_total, dimensioned by
// tenant. Call only when a VerifyChain report covers at least one event and
// none of them are signed (report.Total > 0 && !report.Signed) — an ops-alertable
// signal that this tenant's chain has no cryptographic signature at all (Vault
// audit-signing key never available), distinct from ChainReport.OK which stays
// true for a consistent-but-unsigned chain.
func RecordChainUnsigned(ctx context.Context, tenant string) {
	auditChainUnsigned.Add(ctx, 1, metric.WithAttributes(attribute.String("tenant_id", tenant)))
}
