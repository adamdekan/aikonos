package broker

// session_usage.go — GetSessionUsage: one chat session's billable LLM calls,
// rolled up by (provider, model), for the webui's per-session usage strip.
//
// Reads llm_usage_events (the analytics table, migration 045) rather than
// llm_spend_counters: the counters are a monthly per-subject rollup with no
// session dimension, and re-deriving a session total from them is impossible.
// Reading the same table the LLM Spend dashboard bills from also means the
// number a user sees in chat and the number an admin sees in Grafana cannot
// drift apart.
//
// Unlike GetSpendSummary this is deliberately NOT admin-gated — it is a
// self-service read. The user predicate is enforced in the handler from the
// call's verified identity, so authorization does not depend on the caller
// passing an honest field.

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
)

// sessionIDMaxChars bounds the id we will look up. The gateway caps the
// session_id it forwards at the same order of magnitude; a longer value is a
// hand-crafted request, so reject it rather than hand an unbounded string to
// the query.
const sessionIDMaxChars = 200

// GetSessionUsage returns the caller's own usage for one session. An unknown or
// unattributed session is an empty row set, not an error: a brand-new session
// legitimately has no usage yet, and the webui renders that as "no usage" —
// distinguishing "never existed" from "nothing spent" would also leak whether
// another user's session id exists.
func (s *BrokerService) GetSessionUsage(ctx context.Context, req *brokerv1.GetSessionUsageRequest) (*brokerv1.GetSessionUsageResponse, error) {
	tenant, user, err := s.callerIdentity(ctx, req.TenantId, "")
	if err != nil {
		return nil, err
	}
	if len(req.SessionId) > sessionIDMaxChars {
		return nil, status.Errorf(codes.InvalidArgument, "session_id too long")
	}
	if s.deps.UsageEvents == nil {
		return &brokerv1.GetSessionUsageResponse{}, nil
	}

	rows, err := s.deps.UsageEvents.SessionTotals(ctx, tenant, user, req.SessionId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "session usage: %v", err)
	}

	out := make([]*brokerv1.SessionUsageRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, &brokerv1.SessionUsageRow{
			Provider:   r.Provider,
			Model:      r.Model,
			TokensIn:   r.TokensIn,
			TokensOut:  r.TokensOut,
			CacheRead:  r.CacheRead,
			CacheWrite: r.CacheWrite,
			CostMicros: r.CostMicros,
			Calls:      r.Calls,
		})
	}
	return &brokerv1.GetSessionUsageResponse{Rows: out}, nil
}
