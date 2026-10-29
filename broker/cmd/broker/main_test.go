package main

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/revocation"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
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

// F35: only the host of the alert webhook URL is logged, never its path.
func TestWebhookHost_LogsHostOnly(t *testing.T) {
	if got := webhookHost("https://hooks.slack.com/services/T000/B000/XXXX?token=y"); got != "hooks.slack.com" {
		t.Fatalf("got %q", got)
	}
	if got := webhookHost("://bad/XXXX"); strings.Contains(got, "XXXX") {
		t.Fatalf("unparseable URL leaked: %q", got)
	}
}

// F37: the unauthenticated bundle listener binds every interface only in dev
// mode or when configured to.
func TestBundleHTTPAddr(t *testing.T) {
	log := zap.NewNop()
	if got := bundleHTTPAddr("", false, log); got != "127.0.0.1:9092" {
		t.Errorf("non-dev default = %q, want loopback", got)
	}
	if got := bundleHTTPAddr("", true, log); got != ":9092" {
		t.Errorf("dev default = %q, want :9092", got)
	}
	if got := bundleHTTPAddr("10.0.0.5:9092", false, log); got != "10.0.0.5:9092" {
		t.Errorf("configured = %q", got)
	}
}

// G48/F200: requests over the cap are refused before the handler runs, except
// on the exempt file-upload methods.
func TestRequestSizeInterceptor(t *testing.T) {
	big := &brokerv1.CreateTaskRequest{Prompt: strings.Repeat("a", 100)}
	called := false
	handler := func(context.Context, any) (any, error) { called = true; return nil, nil }
	ic := requestSizeInterceptor(50, map[string]bool{"/exempt": true})

	if _, err := ic(context.Background(), big, &grpc.UnaryServerInfo{FullMethod: "/other"}, handler); status.Code(err) != codes.ResourceExhausted || called {
		t.Fatalf("oversized: err=%v called=%v, want ResourceExhausted and no handler", err, called)
	}
	if _, err := ic(context.Background(), big, &grpc.UnaryServerInfo{FullMethod: "/exempt"}, handler); err != nil || !called {
		t.Fatalf("exempt: err=%v called=%v", err, called)
	}
}

// blockedSubjects is a revocation view that blocks the listed subjects.
type blockedSubjects map[string]bool

func (b blockedSubjects) User(_ context.Context, _, subject string) (revocation.State, error) {
	return revocation.State{Blocked: b[subject]}, nil
}

type identityStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s identityStream) Context() context.Context { return s.ctx }

// TestNorthInterceptors runs the chain main.go registers (F173/G47, F136): a
// blocked caller or one naming another tenant is refused before the
// provisioner runs, and the stream chain refuses a blocked caller too.
func TestNorthInterceptors(t *testing.T) {
	const tenant = "11111111-1111-1111-1111-111111111111"
	v, err := auth.NewValidator(auth.OIDCConfig{}) // disabled: the context identity stands in for a verified token
	if err != nil {
		t.Fatal(err)
	}
	em, err := audit.NewEmitter(context.Background(), audit.Config{})
	if err != nil {
		t.Fatal(err)
	}
	provisioned := 0
	prov := func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
		provisioned++
		return h(ctx, req)
	}
	unary, stream := northInterceptors(v, em, "aikonos-dev", tenant, blockedSubjects{"mallory": true}, prov, zap.NewNop())

	info := &grpc.UnaryServerInfo{FullMethod: "/aikonos.broker.v1.BrokerService/CreateTask"}
	call := func(id *auth.Identity) error {
		h := grpc.UnaryHandler(func(context.Context, any) (any, error) { return "ok", nil })
		for i := len(unary) - 1; i >= 0; i-- {
			ic, next := unary[i], h
			h = func(ctx context.Context, req any) (any, error) { return ic(ctx, req, info, next) }
		}
		_, err := h(auth.WithIdentity(context.Background(), id), &brokerv1.CreateTaskRequest{})
		return err
	}
	if err := call(&auth.Identity{Subject: "mallory", TenantID: tenant}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("blocked caller: %v", err)
	}
	if err := call(&auth.Identity{Subject: "alice", TenantID: "22222222-2222-2222-2222-222222222222"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("foreign tenant: %v", err)
	}
	if provisioned != 0 {
		t.Fatal("the provisioner ran for a refused caller")
	}
	if err := call(&auth.Identity{Subject: "alice", TenantID: tenant}); err != nil || provisioned != 1 {
		t.Fatalf("allowed caller: err=%v provisioned=%d", err, provisioned)
	}

	handled := false
	sh := grpc.StreamHandler(func(any, grpc.ServerStream) error { handled = true; return nil })
	sinfo := &grpc.StreamServerInfo{FullMethod: "/aikonos.broker.v1.BrokerService/StreamTaskEvents"}
	for i := len(stream) - 1; i >= 0; i-- {
		ic, next := stream[i], sh
		sh = func(srv any, ss grpc.ServerStream) error { return ic(srv, ss, sinfo, next) }
	}
	ctx := auth.WithIdentity(context.Background(), &auth.Identity{Subject: "mallory", TenantID: tenant})
	if err := sh(nil, identityStream{ctx: ctx}); status.Code(err) != codes.PermissionDenied || handled {
		t.Fatalf("blocked stream caller: err=%v handled=%v", err, handled)
	}
}
