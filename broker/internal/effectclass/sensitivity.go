package effectclass

import planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

// ReadsSensitive reports whether a step routed at effect class ec should be
// treated as a sensitive read for the exfil-DLP rule (reads_sensitive AND
// {write_external, network_egress} AND NOT has_dlp_attestation).
//
// Only READ_ONLY counts. This is a deliberate over-approximation ceiling —
// READ_ONLY includes web.fetch and siem.* (reads of public/telemetry data
// count as sensitive reads too) — accepted because a curated per-tool
// "sensitive" list would recreate the parallel-table drift the C11 tool-
// registration consolidation removes. Single source of truth: this is the
// only place server-side sensitivity is derived (routeStep applies it; no
// per-tool table exists).
func ReadsSensitive(ec planv1.EffectClass) bool {
	return ec == planv1.EffectClass_READ_ONLY
}
