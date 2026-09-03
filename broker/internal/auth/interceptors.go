// broker/internal/auth/interceptors.go
// gRPC unary interceptors that establish and audit caller identity.
//
//	North-bound chain:  OIDC → tenant → audit
//	South-bound chain:  SPIFFE → audit
//
// Transport-level mTLS (and SPIFFE trust-domain membership) is enforced by the
// server credentials before these run; the interceptors layer application
// identity on top: a verified OIDC subject (north) or peer SPIFFE ID (south),
// both stashed on the context for handlers and the audit trail.
package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffegrpc/grpccredentials"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/ids"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// auditEmitter is the minimal audit surface the auth interceptors need to record
// credential rejections. *audit.Emitter satisfies it; tests pass a fake recorder.
type auditEmitter interface {
	Emit(ctx context.Context, ev *auditv1.AuditEvent) error
}

// OIDCInterceptor validates the bearer token on north-bound calls and puts the
// resulting Identity on the context. When the validator is disabled (no issuer
// configured) it passes calls through unauthenticated — preserving local-dev
// behaviour where callers carry their user id in the request body.
func OIDCInterceptor(v *Validator, emitter auditEmitter, defaultTenant string, log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if !v.Enabled() {
			log.Debug("OIDC validation disabled (dev mode)", zap.String("method", info.FullMethod))
			return handler(ctx, req)
		}

		raw, err := BearerFromContext(ctx)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, err.Error())
		}
		id, err := v.Validate(ctx, raw)
		if err != nil {
			var svcErr *SvcSubjectError
			if errors.As(err, &svcErr) {
				log.Warn("north-bound call with svc- subject rejected", zap.String("method", info.FullMethod), zap.String("subject", svcErr.Subject))
				emitAuthDenied(ctx, emitter, defaultTenant, "auth.subject.rejected", info.FullMethod, svcErr.Subject, svcErr.Email, "svc_on_north")
				return nil, status.Error(codes.PermissionDenied, "svc- subjects may not use the north-bound API")
			}
			log.Warn("OIDC token rejected", zap.String("method", info.FullMethod), zap.Error(err))
			emitAuthDenied(ctx, emitter, defaultTenant, "auth.token.rejected", info.FullMethod, "", "", "invalid_token")
			return nil, status.Error(codes.Unauthenticated, "invalid bearer token")
		}
		// Carry the peer SPIFFE ID too when the north-bound caller is itself a
		// known workload (e.g. a control-plane service rather than a browser).
		if pid, ok := grpccredentials.PeerIDFromContext(ctx); ok {
			id.SpiffeID = pid.String()
		}
		log.Debug("OIDC token accepted",
			zap.String("method", info.FullMethod),
			zap.String("subject", id.Subject),
			zap.String("tenant", id.TenantID),
		)
		return handler(WithIdentity(ctx, id), req)
	}
}

// TenantInterceptor enforces that an authenticated north-bound caller carries a
// tenant claim. It runs after OIDCInterceptor; in dev mode (no identity on the
// context) it is a no-op so request-body tenant ids still flow through.
func TenantInterceptor(emitter auditEmitter, defaultTenant string, log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		id, ok := IdentityFromContext(ctx)
		if !ok {
			return handler(ctx, req)
		}
		if id.TenantID == "" {
			log.Warn("authenticated caller has no tenant claim", zap.String("subject", id.Subject))
			emitAuthDenied(ctx, emitter, defaultTenant, "auth.tenant.missing", info.FullMethod, id.Subject, id.Email, "no_tenant_claim")
			return nil, status.Error(codes.PermissionDenied, "token carries no tenant")
		}
		return handler(ctx, req)
	}
}

// SPIFFEInterceptor extracts the verified peer SPIFFE ID on south-bound calls
// and puts it on the context as the caller Identity. The server credentials
// already pin the trust domain; allowedPathPrefix (when non-empty) additionally
// constrains which workloads may call (e.g. "/sandbox/").
func SPIFFEInterceptor(allowedPathPrefix string, emitter auditEmitter, defaultTenant string, log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		pid, ok := grpccredentials.PeerIDFromContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "no SPIFFE peer identity")
		}
		if allowedPathPrefix != "" && !strings.HasPrefix(pid.Path(), allowedPathPrefix) {
			log.Warn("south-bound caller rejected by path prefix",
				zap.String("spiffe_id", pid.String()),
				zap.String("required_prefix", allowedPathPrefix),
			)
			emitAuthDenied(ctx, emitter, defaultTenant, "auth.peer.rejected", info.FullMethod, pid.String(), "", "path_not_permitted")
			return nil, status.Errorf(codes.PermissionDenied, "workload %s not permitted", pid.String())
		}
		log.Debug("SPIFFE peer accepted", zap.String("method", info.FullMethod), zap.String("spiffe_id", pid.String()))
		return handler(WithIdentity(ctx, &Identity{SpiffeID: pid.String()}), req)
	}
}

// AuditInterceptor emits one audit event per call, recording the method, trace
// id, caller identity, and outcome (ALLOW on success, DENY on error).
//
// A panicking handler is recovered here rather than left to crash the audit
// trail along with the RPC: without this, a panic unwinds straight past the
// Emit call below and a mutation that blew up leaves no record at all. The
// recovered panic is converted to codes.Internal (never re-panicked — the
// server must stay up) and still gets its DENY audit event via the same emit
// path a normal error takes.
func AuditInterceptor(emitter auditEmitter, defaultTenant string, log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("recovered panic in RPC handler",
					zap.String("method", info.FullMethod), zap.Any("panic", r))
				resp, err = nil, status.Errorf(codes.Internal, "internal error")
			}
			emitRPCAudit(ctx, emitter, defaultTenant, info.FullMethod, err)
		}()
		return handler(ctx, req)
	}
}

// emitRPCAudit is AuditInterceptor's shared emit path — used for both the
// normal return and the recovered-panic path so the two produce identical
// event shapes (only Decision and the underlying err differ).
func emitRPCAudit(ctx context.Context, emitter auditEmitter, defaultTenant, method string, err error) {
	if emitter == nil {
		return
	}
	id, _ := IdentityFromContext(ctx)
	tenant := defaultTenant
	actor, email, spiffe := "", "", ""
	if id != nil {
		// actor_user_id is the STABLE principal (oid), never email-first; the
		// human label goes in actor_email. PrincipalID() = Subject (oid) first.
		actor, email, spiffe = id.PrincipalID(), id.Email, id.SpiffeID
		if id.TenantID != "" {
			tenant = id.TenantID
		}
	}
	if tenant == "" {
		tenant = "unknown"
	}

	decision := auditv1.PolicyDecision_ALLOW
	if err != nil {
		decision = auditv1.PolicyDecision_DENY
	}

	traceID := ""
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		traceID = sc.TraceID().String()
	}

	_ = emitter.Emit(ctx, &auditv1.AuditEvent{
		EventId:       ids.EventID(),
		TraceId:       traceID,
		TenantId:      tenant,
		OccurredAt:    timestamppb.New(time.Now().UTC()),
		ActorUserId:   actor,
		ActorEmail:    email,
		ActorSpiffeId: spiffe,
		EventType:     "aikonos.broker.rpc",
		ResourceRef:   method,
		Decision:      decision,
	})
}

// emitAuthDenied records a credential-rejection as a structured audit event so
// auth failures flow through the signed audit trail (NATS → MinIO + observability)
// instead of only stderr. Emitted for rejected *presented* credentials
// (invalid token, svc- on north, missing tenant claim, SPIFFE path) — not for
// pre-credential cases (no bearer / no peer), which are transport-level noise.
// nil emitter is a no-op; the subject is "" when no identity was established.
func emitAuthDenied(ctx context.Context, emitter auditEmitter, defaultTenant, eventType, method, subject, email, reason string) {
	if emitter == nil {
		return
	}
	tenant := defaultTenant
	if tenant == "" {
		tenant = "unknown"
	}
	traceID := ""
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() {
		traceID = sc.TraceID().String()
	}
	ctxStruct, _ := structpb.NewStruct(map[string]any{"reason": reason, "method": method})
	_ = emitter.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TraceId:     traceID,
		TenantId:    tenant,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		ActorUserId: subject,
		ActorEmail:  email,
		EventType:   eventType,
		ResourceRef: method,
		Decision:    auditv1.PolicyDecision_DENY,
		Context:     ctxStruct,
	})
}

// BearerFromContext pulls the "authorization: Bearer <token>" value from the
// incoming gRPC metadata. Exported so RPC handlers that need to forward the
// caller's own bearer downstream (e.g. an OBO exchange) can read it without
// re-parsing metadata themselves.
func BearerFromContext(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", errors.New("no request metadata")
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return "", errors.New("missing authorization metadata")
	}
	const prefix = "Bearer "
	h := vals[0]
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", errors.New("authorization header is not a Bearer token")
	}
	return h[len(prefix):], nil
}
