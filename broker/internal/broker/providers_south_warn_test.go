package broker

import (
	"context"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// TestGetLlmProviders_VaultKeyMissingWarns pins the Vault-wipe signature:
// HasKey is true (a key was registered) but ReadProviderKey reports a miss
// (ok=false, err=nil) — the secret is gone from Vault, e.g. after an inmem
// Vault restart. That must produce a named warning instead of silently
// returning a provider with an empty api_key.
func TestGetLlmProviders_VaultKeyMissingWarns(t *testing.T) {
	core, recorded := observer.New(zapcore.DebugLevel)
	obsLogger := zap.New(core)

	store := newFakeLlmProviderStore()
	store.rows[testTenant+"/openrouter"] = fakeLlmProviderRow{
		id: "openrouter", name: "OR", endpoint: "https://x.com", api: "openai-completions", hasKey: true,
	}

	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc := NewSandboxService(Deps{
		Logger:          obsLogger,
		Audit:           em,
		GatewaySpiffeID: testGateway,
		TenantID:        testTenant,
		Secrets:         newCapturingSecrets(), // ReadProviderKey returns "", false, nil — Vault-wipe miss
		Providers:       store,
	})

	ctx := gatewayCtx(testGateway)
	resp, err := svc.GetLlmProviders(ctx, &brokerv1.GetLlmProvidersRequest{TenantId: testTenant})
	if err != nil {
		t.Fatalf("GetLlmProviders: %v", err)
	}

	var found *brokerv1.LlmProvider
	for _, p := range resp.Providers {
		if p.Id == "openrouter" {
			found = p
		}
	}
	if found == nil {
		t.Fatal("openrouter provider not in response")
	}
	if found.ApiKey != "" {
		t.Errorf("api_key = %q, want empty on a Vault-miss", found.ApiKey)
	}

	warns := recorded.FilterLevelExact(zapcore.WarnLevel).FilterMessageSnippet("provider key missing from Vault")
	if warns.Len() != 1 {
		t.Fatalf("want 1 warn naming the provider-key-missing condition, got %d entries: %+v", warns.Len(), recorded.All())
	}
	entry := warns.All()[0]
	found2 := false
	for _, f := range entry.Context {
		if f.Key == "provider" && f.String == "openrouter" {
			found2 = true
		}
	}
	if !found2 {
		t.Errorf("warn entry missing provider=openrouter field: %+v", entry.Context)
	}
}
