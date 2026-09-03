// broker/internal/provisioning/interceptor.go
// gRPC unary server interceptor that fires Seen for every authenticated north
// caller. Never blocks or fails the RPC.
package provisioning

import (
	"context"

	"google.golang.org/grpc"

	"github.com/adamdekan/aikonos/broker/internal/auth"
)

// Interceptor returns a gRPC unary interceptor that reads the verified identity
// from the context (placed there by the OIDC interceptor) and fires Seen in a
// goroutine. The handler is always called immediately; provisioning is
// best-effort and must not affect RPC latency or success.
func (p *Provisioner) Interceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if id, ok := auth.IdentityFromContext(ctx); ok {
			tenantID := id.TenantID
			// Seen detaches from ctx intentionally: it spawns a goroutine with a
			// fresh background context so the RPC cancellation cannot abort the
			// provisioning flow mid-write.
			p.Seen(ctx, tenantID, id)
		}
		return handler(ctx, req)
	}
}
