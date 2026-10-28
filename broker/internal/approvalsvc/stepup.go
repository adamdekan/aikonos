package approvalsvc

import (
	"context"
	"slices"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/adamdekan/aikonos/broker/internal/auth"
)

// StepUpRequired is the status message of the refusal a step-up approval gets
// without a recent multi-factor sign-in (AG-13). The web UI keys on it.
const StepUpRequired = "step_up_required"

// StepUpACRTrailer carries the tenant's approvals.step_up_acr on that refusal,
// so the web UI can ask the identity provider for it.
const StepUpACRTrailer = "aikonos-step-up-acr"

// StepUp is a tenant's rule for approvals OPA marks require_step_up.
type StepUp struct {
	ACR    string        // approvals.step_up_acr: an acr / Entra acrs value that counts as MFA ("" = amr only)
	MaxAge time.Duration // approvals.step_up_max_age_minutes
}

// mfaAMR are the RFC 8176 amr values that prove a second factor.
var mfaAMR = map[string]bool{"mfa": true, "otp": true, "hwk": true, "sms": true, "tel": true, "fpt": true, "face": true, "sc": true}

// SteppedUp reports whether id carries a recent multi-factor sign-in: amr names
// a second factor or acr/acrs equals the configured value, and auth_time lies
// within MaxAge. No identity (a south caller) or no auth_time fails closed.
func (r StepUp) SteppedUp(id *auth.Identity, now time.Time) bool {
	if id == nil || id.AuthTime.IsZero() || now.Sub(id.AuthTime) > r.MaxAge {
		return false
	}
	if r.ACR != "" && (id.ACR == r.ACR || slices.Contains(id.ACRS, r.ACR)) {
		return true
	}
	return slices.ContainsFunc(id.AMR, func(m string) bool { return mfaAMR[m] })
}

// requireStepUp refuses with StepUpRequired unless the caller on ctx has
// stepped up. The trailer is best-effort: outside a gRPC server it is dropped.
func (r StepUp) requireStepUp(ctx context.Context) error {
	id, _ := auth.IdentityFromContext(ctx)
	if r.SteppedUp(id, time.Now()) {
		return nil
	}
	if r.ACR != "" {
		_ = grpc.SetTrailer(ctx, metadata.Pairs(StepUpACRTrailer, r.ACR))
	}
	return status.Error(codes.PermissionDenied, StepUpRequired)
}

// IsStepUpRequired reports whether err is the step-up refusal.
func IsStepUpRequired(err error) bool {
	s, ok := status.FromError(err)
	return ok && s.Code() == codes.PermissionDenied && s.Message() == StepUpRequired
}
