// broker/internal/provisioning/interceptor.go
// gRPC unary and stream server interceptors that fire Seen for every
// authenticated north caller. Never block or fail the RPC.
package provisioning

import (
	"context"

	"google.golang.org/grpc"

	"github.com/adamdekan/aikonos/broker/internal/auth"
)

// Interceptor returns a gRPC unary interceptor that reads the verified identity
// from the context (placed there by the OIDC interceptor) and fires Seen in a
// goroutine. The handler is always called immediately; provisioning is
// best-effort and must not affect RPC latency or success. It must run after
// the tenant and revocation interceptors, so a blocked, session-revoked or
// tenant-less caller is never provisioned into groups.
func (p *Provisioner) Interceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		p.seen(ctx)
		return handler(ctx, req)
	}
}

// StreamInterceptor is Interceptor for streaming RPCs, with the same ordering
// requirement.
func (p *Provisioner) StreamInterceptor() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		p.seen(ss.Context())
		return handler(srv, ss)
	}
}

func (p *Provisioner) seen(ctx context.Context) {
	if id, ok := auth.IdentityFromContext(ctx); ok && id.TenantID != "" {
		// Seen detaches from ctx intentionally: it spawns a goroutine with a
		// fresh background context so the RPC cancellation cannot abort the
		// provisioning flow mid-write.
		p.Seen(ctx, id.TenantID, id)
	}
}
