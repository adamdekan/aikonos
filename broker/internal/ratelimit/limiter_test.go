package ratelimit_test

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/ratelimit"
)

const (
	tenant = "tenant-1"
	agent1 = "agent-1"
	agent2 = "agent-2"
	prov1  = "anthropic"
)

func TestNoPolicy(t *testing.T) {
	l := ratelimit.New(nil)
	if err := l.CheckToolInvocation(context.Background(), tenant, agent1); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	allowed, _ := l.CheckLlmRequest(context.Background(), tenant, agent1, prov1)
	if !allowed {
		t.Fatal("expected allowed")
	}
}

func TestRPMDenyAll(t *testing.T) {
	l := ratelimit.New(nil)
	l.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: agent1, RPM: 0, TPM: -1}})
	err := l.CheckToolInvocation(context.Background(), tenant, agent1)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected ResourceExhausted, got %v", err)
	}
}

func TestRPMLimit(t *testing.T) {
	l := ratelimit.New(nil)
	l.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: agent1, RPM: 3, TPM: -1}})
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := l.CheckToolInvocation(ctx, tenant, agent1); err != nil {
			t.Fatalf("call %d expected nil, got %v", i+1, err)
		}
	}
	if err := l.CheckToolInvocation(ctx, tenant, agent1); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("4th call expected ResourceExhausted, got %v", err)
	}
	// different agent unaffected
	if err := l.CheckToolInvocation(ctx, tenant, agent2); err != nil {
		t.Fatalf("different agent should not be limited: %v", err)
	}
}

func TestTPMLimit(t *testing.T) {
	l := ratelimit.New(nil)
	l.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: agent1, RPM: -1, TPM: 500}})
	ctx := context.Background()
	l.RecordLlmTokens(ctx, tenant, agent1, prov1, 600)
	allowed, lt := l.CheckLlmRequest(ctx, tenant, agent1, prov1)
	if allowed {
		t.Fatal("expected not allowed after exceeding TPM")
	}
	if lt != "tpm_agent" {
		t.Fatalf("expected tpm_agent, got %q", lt)
	}
}

func TestSetPoliciesHotReload(t *testing.T) {
	l := ratelimit.New(nil)
	l.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: agent1, RPM: 1, TPM: -1}})
	ctx := context.Background()
	l.CheckToolInvocation(ctx, tenant, agent1) // use the 1 allowed
	if err := l.CheckToolInvocation(ctx, tenant, agent1); status.Code(err) != codes.ResourceExhausted {
		t.Fatal("expected rate limit before reload")
	}
	l.SetPolicies(nil) // clear all policies
	if err := l.CheckToolInvocation(ctx, tenant, agent1); err != nil {
		t.Fatalf("after clearing policies expected nil, got %v", err)
	}
}

func TestWindowRollover(t *testing.T) {
	// Simulate a new minute window by creating a fresh Limiter with the same policies.
	// This proves that rate counters are not globally shared and a new window
	// (represented here by a new Limiter) resets the count.
	l := ratelimit.New(nil)
	l.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: agent1, RPM: 1, TPM: -1}})
	ctx := context.Background()
	if err := l.CheckToolInvocation(ctx, tenant, agent1); err != nil {
		t.Fatalf("first call should pass: %v", err)
	}
	if err := l.CheckToolInvocation(ctx, tenant, agent1); status.Code(err) != codes.ResourceExhausted {
		t.Fatal("second call in same window should be limited")
	}
	// New Limiter = new window counters
	l2 := ratelimit.New(nil)
	l2.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: agent1, RPM: 1, TPM: -1}})
	if err := l2.CheckToolInvocation(ctx, tenant, agent1); err != nil {
		t.Fatalf("fresh limiter (new window) should allow: %v", err)
	}
}

func TestTPMProviderLimit(t *testing.T) {
	l := ratelimit.New(nil)
	l.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: "", Provider: prov1, RPM: -1, TPM: 100}})
	ctx := context.Background()
	l.RecordLlmTokens(ctx, tenant, agent1, prov1, 200)
	allowed, lt := l.CheckLlmRequest(ctx, tenant, agent1, prov1)
	if allowed {
		t.Fatal("expected not allowed")
	}
	if lt != "tpm_provider" {
		t.Fatalf("expected tpm_provider, got %q", lt)
	}
}

func TestTPMGlobalLimit(t *testing.T) {
	l := ratelimit.New(nil)
	l.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: "", Provider: "", RPM: -1, TPM: 100}})
	ctx := context.Background()
	l.RecordLlmTokens(ctx, tenant, agent1, prov1, 200)
	allowed, lt := l.CheckLlmRequest(ctx, tenant, agent1, prov1)
	if allowed {
		t.Fatal("expected not allowed")
	}
	if lt != "tpm_global" {
		t.Fatalf("expected tpm_global, got %q", lt)
	}
}

// newTestMeter builds a real Int64Counter backed by an in-process SDK
// MeterProvider + ManualReader, so recorded points can be collected and
// asserted without any network/OTLP dependency.
func newTestMeter(t *testing.T) (metric.Int64Counter, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	counter, err := provider.Meter("ratelimit-test").Int64Counter("rate_limit_hits_total")
	if err != nil {
		t.Fatalf("failed to create counter: %v", err)
	}
	return counter, reader
}

// sumByResult collects the reader and sums data point values whose "result"
// attribute matches want.
func sumByResult(t *testing.T, reader *sdkmetric.ManualReader, want string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect failed: %v", err)
	}
	var sum int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum2, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum2.DataPoints {
				if v, ok := dp.Attributes.Value(attribute.Key("result")); ok && v.AsString() == want {
					sum += dp.Value
				}
			}
		}
	}
	return sum
}

// TestDeniedCallDoesNotConsumeQuota pins the check-then-increment-on-allow
// ordering: calls denied while a policy is at RPM=1 must not have advanced
// the counter, proven by raising the policy to RPM=2 via hot-reload and
// observing the next call is still allowed (count was 1, not 3).
func TestDeniedCallDoesNotConsumeQuota(t *testing.T) {
	l := ratelimit.New(nil)
	l.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: agent1, RPM: 1, TPM: -1}})
	ctx := context.Background()

	if err := l.CheckToolInvocation(ctx, tenant, agent1); err != nil {
		t.Fatalf("1st call expected nil, got %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := l.CheckToolInvocation(ctx, tenant, agent1); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("denied call %d expected ResourceExhausted, got %v", i, err)
		}
	}

	// If the denied calls above had still incremented the counter (the old
	// buggy behavior), the counter would now be at 4 and this raised-to-2
	// policy would still deny. Because denies don't consume quota, the
	// counter is still at 1 and this call is allowed.
	l.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: agent1, RPM: 2, TPM: -1}})
	if err := l.CheckToolInvocation(ctx, tenant, agent1); err != nil {
		t.Fatalf("expected allow after raising RPM to 2, got %v", err)
	}
}

// TestAllowIncrementsCounter pins the complementary half: allowed calls still
// advance the counter (denying quota-skipping in the other direction).
func TestAllowIncrementsCounter(t *testing.T) {
	l := ratelimit.New(nil)
	l.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: agent1, RPM: 2, TPM: -1}})
	ctx := context.Background()

	if err := l.CheckToolInvocation(ctx, tenant, agent1); err != nil {
		t.Fatalf("1st call expected nil, got %v", err)
	}
	if err := l.CheckToolInvocation(ctx, tenant, agent1); err != nil {
		t.Fatalf("2nd call expected nil, got %v", err)
	}
	if err := l.CheckToolInvocation(ctx, tenant, agent1); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("3rd call expected ResourceExhausted (counter must have advanced to 2), got %v", err)
	}
}

// TestMeterRecordsAllowAndDeny wires a real SDK counter into New() and
// asserts allow/deny hits are recorded with the expected "result" label.
func TestMeterRecordsAllowAndDeny(t *testing.T) {
	counter, reader := newTestMeter(t)
	l := ratelimit.New(counter)
	l.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: agent1, RPM: 1, TPM: -1}})
	ctx := context.Background()

	if err := l.CheckToolInvocation(ctx, tenant, agent1); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
	if err := l.CheckToolInvocation(ctx, tenant, agent1); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected deny, got %v", err)
	}

	if got := sumByResult(t, reader, "allowed"); got != 1 {
		t.Fatalf("expected 1 allowed hit recorded, got %d", got)
	}
	if got := sumByResult(t, reader, "denied"); got != 1 {
		t.Fatalf("expected 1 denied hit recorded, got %d", got)
	}
}

// TestMeterNilIsSafe proves New(nil) — the path every other test in this file
// uses — never panics when a call is allowed or denied.
func TestMeterNilIsSafe(t *testing.T) {
	l := ratelimit.New(nil)
	l.SetPolicies([]ratelimit.Policy{{TenantID: tenant, AgentID: agent1, RPM: 0, TPM: -1}})
	ctx := context.Background()
	if err := l.CheckToolInvocation(ctx, tenant, agent1); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("expected deny, got %v", err)
	}
}

func TestTenantIsolation(t *testing.T) {
	l := ratelimit.New(nil)
	l.SetPolicies([]ratelimit.Policy{{TenantID: "tenant-A", AgentID: agent1, RPM: 1, TPM: -1}})
	ctx := context.Background()
	l.CheckToolInvocation(ctx, "tenant-A", agent1) // use tenant-A's limit
	// tenant-B with same agent not affected
	if err := l.CheckToolInvocation(ctx, "tenant-B", agent1); err != nil {
		t.Fatalf("different tenant should not be limited: %v", err)
	}
}
