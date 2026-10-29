package main

import (
	"context"
	"errors"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/connector"
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

// healthServer serves the gRPC health service (whose Watch stream stays open
// until the client or server ends it) with serverLimits(maxStreams), and
// returns a client connection to it.
func healthServer(t *testing.T, maxStreams uint32) (*grpc.Server, healthpb.HealthClient) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(serverLimits(maxStreams)...)
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///"+lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return srv, healthpb.NewHealthClient(conn)
}

// The gateway sends every user's RPCs to a server over one connection, so the
// default stream cap must carry well over 100 concurrent long-lived calls on
// a single connection (the old hard-coded cap of 100 starved every tenant).
func TestServerLimits_DefaultStreamsFitSharedChannel(t *testing.T) {
	_, client := healthServer(t, defaultGRPCMaxStreams)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, 150)
	for i := 0; i < 150; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stream, err := client.Watch(ctx, &healthpb.HealthCheckRequest{})
			if err == nil {
				_, err = stream.Recv()
			}
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("stream %v: want every open stream served on one connection", err)
	}
}

// F200: a stream that never ends cannot hold shutdown past the grace period.
func TestStopGRPC_BoundedByGrace(t *testing.T) {
	srv, client := healthServer(t, defaultGRPCMaxStreams)
	stream, err := client.Watch(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	stopGRPC(srv, 100*time.Millisecond)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("stopGRPC took %v with an open stream, want about the 100ms grace", d)
	}
}

// F200: past maxConns open connections from one IP, the next one from that
// IP is closed at once; closing one gives its slot back.
func TestListenLimited(t *testing.T) {
	lis, err := listenLimited("127.0.0.1:0", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	accepted := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	dial := func() net.Conn {
		c, err := net.Dial("tcp", lis.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	dial()
	first := <-accepted
	over := dial()
	_ = over.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := over.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("second connection from the same IP: read err=%v, want it closed by the server", err)
	}
	select {
	case <-accepted:
		t.Fatal("second connection handed to the server past the cap of 1")
	default:
	}
	first.Close()
	dial()
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("a new connection was not accepted after the first closed")
	}
}
