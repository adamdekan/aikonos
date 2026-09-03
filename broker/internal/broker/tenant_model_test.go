package broker

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

const testGateway = "spiffe://aikonos.com/agent-gateway"
const testTenant = "11111111-1111-1111-1111-111111111111"

func newModelSvc(kvs map[string]string) *SandboxService {
	cfg := newFakeConfigStore()
	for k, v := range kvs {
		cfg.values[testTenant+":"+k] = v
	}
	return NewSandboxService(Deps{
		Logger:          zap.NewNop(),
		GatewaySpiffeID: testGateway,
		Config:          cfg,
	})
}

func TestGetTenantModel_RequiresGatewayPeer(t *testing.T) {
	svc := newModelSvc(nil)
	_, err := svc.GetTenantModel(context.Background(), &brokerv1.GetTenantModelRequest{
		TenantId: testTenant,
	})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("missing peer: want PermissionDenied, got %v", err)
	}
}

func TestGetTenantModel_WrongPeerDenied(t *testing.T) {
	svc := newModelSvc(nil)
	ctx := gatewayCtx("spiffe://aikonos.com/some-sandbox")
	_, err := svc.GetTenantModel(ctx, &brokerv1.GetTenantModelRequest{TenantId: testTenant})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wrong peer: want PermissionDenied, got %v", err)
	}
}

func TestGetTenantModel_MissingTenantId(t *testing.T) {
	svc := newModelSvc(nil)
	_, err := svc.GetTenantModel(gatewayCtx(testGateway), &brokerv1.GetTenantModelRequest{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty tenant_id: want InvalidArgument, got %v", err)
	}
}

func TestGetTenantModel_ReturnsConfiguredModel(t *testing.T) {
	svc := newModelSvc(map[string]string{"llm_model": "openai/gpt-4o"})
	resp, err := svc.GetTenantModel(gatewayCtx(testGateway), &brokerv1.GetTenantModelRequest{TenantId: testTenant})
	if err != nil {
		t.Fatalf("want nil error, got %v", err)
	}
	if resp.Model != "openai/gpt-4o" {
		t.Fatalf("want openai/gpt-4o, got %q", resp.Model)
	}
}

func TestGetTenantModel_EmptyWhenUnset(t *testing.T) {
	svc := newModelSvc(nil)
	resp, err := svc.GetTenantModel(gatewayCtx(testGateway), &brokerv1.GetTenantModelRequest{TenantId: testTenant})
	if err != nil {
		t.Fatalf("want nil error, got %v", err)
	}
	if resp.Model != "" {
		t.Fatalf("want empty model when unset, got %q", resp.Model)
	}
}
