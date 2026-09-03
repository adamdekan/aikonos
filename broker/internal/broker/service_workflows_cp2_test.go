package broker

// service_workflows_cp2_test.go — unit tests for the fable-rpc-twins CP2
// preamble helpers: workflowCallerSouth (SandboxService) and workflowCallerNorth
// (BrokerService). Written before the wrapper rewrite (TDD): these assert the
// exact codes/messages the inline preambles returned pre-collapse.

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/gatewaygrant"
	"github.com/adamdekan/aikonos/broker/internal/policy"
)

// ── workflowCallerSouth ───────────────────────────────────────────────────────

func newSandboxSvcForCallerTest(t *testing.T, sp *fakeSkillPolicy) *SandboxService {
	t.Helper()
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatalf("audit.NewEmitter: %v", err)
	}
	svc := NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		Audit:           em,
		GatewaySpiffeID: testGWSpiffeID,
		GatewayGrantKey: testGrantKey,
		GatewayGrantTTL: testGrantTTL,
	})
	svc.skillPolicy = sp
	return svc
}

func TestWorkflowCallerSouth_NotGatewayPeer_PermissionDenied(t *testing.T) {
	svc := newSandboxSvcForCallerTest(t, &fakeSkillPolicy{enabled: true, granted: true})
	// No identity on context at all → requireGatewayPeer fails first.
	_, _, err := svc.workflowCallerSouth(context.Background(), testWFTenant, "irrelevant")
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v (err: %v)", got, err)
	}
	if err.Error() != status.Error(codes.PermissionDenied, "peer identity required").Error() {
		t.Errorf("message = %q, want %q", err.Error(), "peer identity required")
	}
}

func TestWorkflowCallerSouth_MissingGrant_PermissionDenied(t *testing.T) {
	svc := newSandboxSvcForCallerTest(t, &fakeSkillPolicy{enabled: true, granted: true})
	_, _, err := svc.workflowCallerSouth(gatewayCtxForWorkflow(), testWFTenant, "")
	wantErr := status.Error(codes.PermissionDenied, "owner grant required")
	if err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestWorkflowCallerSouth_InvalidGrant_PermissionDenied(t *testing.T) {
	svc := newSandboxSvcForCallerTest(t, &fakeSkillPolicy{enabled: true, granted: true})
	_, _, err := svc.workflowCallerSouth(gatewayCtxForWorkflow(), testWFTenant, "not-a-real-grant")
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("want PermissionDenied, got %v (err: %v)", got, err)
	}
	// Message shape: "invalid owner grant: %v" — verify prefix, not the wrapped
	// gatewaygrant error text (that's an implementation detail of gatewaygrant.Verify).
	const wantPrefix = "invalid owner grant: "
	st, _ := status.FromError(err)
	if len(st.Message()) <= len(wantPrefix) || st.Message()[:len(wantPrefix)] != wantPrefix {
		t.Errorf("message = %q, want prefix %q", st.Message(), wantPrefix)
	}
}

func TestWorkflowCallerSouth_TenantMismatch_PermissionDenied(t *testing.T) {
	svc := newSandboxSvcForCallerTest(t, &fakeSkillPolicy{enabled: true, granted: true})
	grant, err := gatewaygrant.Mint(testGrantKey, testWFTenant, testWFOwner, testGrantTTL)
	if err != nil {
		t.Fatalf("gatewaygrant.Mint: %v", err)
	}
	const otherTenant = "ffffffff-ffff-ffff-ffff-ffffffffffff"
	_, _, err = svc.workflowCallerSouth(gatewayCtxForWorkflow(), otherTenant, grant)
	wantErr := status.Error(codes.PermissionDenied, "owner grant tenant mismatch")
	if err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestWorkflowCallerSouth_SkillDenied_PermissionDenied(t *testing.T) {
	svc := newSandboxSvcForCallerTest(t, &fakeSkillPolicy{enabled: true, granted: false})
	grant, err := gatewaygrant.Mint(testGrantKey, testWFTenant, testWFOwner, testGrantTTL)
	if err != nil {
		t.Fatalf("gatewaygrant.Mint: %v", err)
	}
	_, _, err = svc.workflowCallerSouth(gatewayCtxForWorkflow(), testWFTenant, grant)
	wantErr := status.Error(codes.PermissionDenied, "skill:workflows not granted")
	if err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestWorkflowCallerSouth_Success(t *testing.T) {
	svc := newSandboxSvcForCallerTest(t, &fakeSkillPolicy{enabled: true, granted: true})
	grant, err := gatewaygrant.Mint(testGrantKey, testWFTenant, testWFOwner, testGrantTTL)
	if err != nil {
		t.Fatalf("gatewaygrant.Mint: %v", err)
	}
	tenant, user, err := svc.workflowCallerSouth(gatewayCtxForWorkflow(), testWFTenant, grant)
	if err != nil {
		t.Fatalf("workflowCallerSouth: %v", err)
	}
	if tenant != testWFTenant || user != testWFOwner {
		t.Errorf("got (tenant=%q, user=%q), want (%q, %q)", tenant, user, testWFTenant, testWFOwner)
	}
}

// ── workflowCallerNorth ───────────────────────────────────────────────────────

func TestWorkflowCallerNorth_IdentityFailure_PermissionDenied(t *testing.T) {
	// A verified subject that mismatches the request's asserted owner_user_id
	// must fail inside callerIdentity before the skill gate ever runs.
	svc := NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		TenantID: "aikonos-dev",
	})
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{
		TenantID: testWFTenant,
		Subject:  "alice@example.com",
	})
	_, _, err := svc.workflowCallerNorth(ctx, testWFTenant, "eve@example.com")
	wantErr := status.Error(codes.PermissionDenied, "user_id does not match the authenticated identity")
	if err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestWorkflowCallerNorth_SkillDenied_PermissionDenied(t *testing.T) {
	fga := &fakeFGA{checks: map[string]bool{}}
	srv := fga.server(t)
	t.Cleanup(srv.Close)

	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	svc := NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		Policy:   eng,
		TenantID: "aikonos-dev",
	})
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")
	_, _, err = svc.workflowCallerNorth(ctx, testWFTenant, testWFOwner)
	wantErr := status.Error(codes.PermissionDenied, "skill:workflows not granted")
	if err == nil || err.Error() != wantErr.Error() {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

func TestWorkflowCallerNorth_Success(t *testing.T) {
	fga := &fakeFGA{checks: map[string]bool{
		"user:" + testWFOwner + "|can_invoke|skill:workflows": true,
	}}
	srv := fga.server(t)
	t.Cleanup(srv.Close)

	eng, err := policy.NewEngine(context.Background(), policy.Config{
		OPAEndpoint:     "http://unused",
		OpenFGAEndpoint: srv.URL,
		OpenFGAStoreID:  "store-1",
	})
	if err != nil {
		t.Fatalf("policy.NewEngine: %v", err)
	}
	svc := NewBrokerService(Deps{
		Logger:   zap.NewNop(),
		Policy:   eng,
		TenantID: "aikonos-dev",
	})
	ctx := ctxWithSubject(testWFTenant, testWFOwner, "")
	tenant, user, err := svc.workflowCallerNorth(ctx, testWFTenant, testWFOwner)
	if err != nil {
		t.Fatalf("workflowCallerNorth: %v", err)
	}
	if tenant != testWFTenant || user != testWFOwner {
		t.Errorf("got (tenant=%q, user=%q), want (%q, %q)", tenant, user, testWFTenant, testWFOwner)
	}
}
