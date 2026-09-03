package main

import (
	"context"
	"testing"

	"github.com/adamdekan/aikonos/broker/internal/connector"
)

func TestInitMeter_DisabledWhenNoEndpoint(t *testing.T) {
	ctx := context.Background()
	mp, err := initMeter(ctx, "")
	if err != nil {
		t.Fatalf("initMeter(empty) error: %v", err)
	}
	if mp == nil {
		t.Fatal("initMeter(empty) returned nil provider")
	}
	// A no-reader/no-exporter provider must flush cleanly with no network attempt.
	// If an exporter were accidentally wired on the empty-endpoint path, ForceFlush
	// would either error or block waiting for a connection that never comes.
	if err := mp.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush on disabled provider: %v", err)
	}
	_ = mp.Shutdown(ctx)
}

func TestInitMeter_ConstructsWithEndpoint(t *testing.T) {
	// Construction only — no live collector required. The OTLP gRPC exporter
	// connects lazily, so New() succeeds even when the endpoint is unreachable.
	ctx := context.Background()
	mp, err := initMeter(ctx, "localhost:4317")
	if err != nil {
		t.Fatalf("initMeter(endpoint) error: %v", err)
	}
	if mp == nil {
		t.Fatal("initMeter(endpoint) returned nil provider")
	}
	_ = mp.Shutdown(ctx)
}

// TestBuildConnectorCredentials_ReadsPerProviderKeys proves buildConnectorCredentials
// derives each registered provider's own "connectors.<key>.*" viper keys — pinning
// the existing deployed "connectors.google.*" / "connectors.microsoft.*" segments
// (F60) rather than a single shared/hardcoded pair. A fake get keyed by the exact
// viper string catches both a wrong key derivation and a missing provider: deleting
// either provider's population line (or misrouting its key) fails this test, unlike
// the pre-extraction inline loop in main(), which no test ever exercised.
func TestBuildConnectorCredentials_ReadsPerProviderKeys(t *testing.T) {
	values := map[string]string{
		"connectors.google.client_id":        "g-id",
		"connectors.google.client_secret":    "g-secret",
		"connectors.microsoft.client_id":     "m-id",
		"connectors.microsoft.client_secret": "m-secret",
	}
	get := func(key string) string { return values[key] }

	creds := buildConnectorCredentials(get)

	g, ok := creds[connector.ProviderGoogleDrive]
	if !ok {
		t.Fatal("missing ProviderGoogleDrive entry")
	}
	if g.ClientID != "g-id" || g.ClientSecret != "g-secret" {
		t.Fatalf("google creds = %+v, want g-id/g-secret", g)
	}

	m, ok := creds[connector.ProviderOneDrive]
	if !ok {
		t.Fatal("missing ProviderOneDrive entry")
	}
	if m.ClientID != "m-id" || m.ClientSecret != "m-secret" {
		t.Fatalf("microsoft creds = %+v, want m-id/m-secret", m)
	}

	if len(creds) != 2 {
		t.Fatalf("len(creds) = %d, want 2 (exactly the registered providers)", len(creds))
	}
}
