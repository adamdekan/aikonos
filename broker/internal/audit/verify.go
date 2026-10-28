package audit

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	"google.golang.org/protobuf/proto"
)

// ChainBreak describes a single integrity failure (continuity or signature).
type ChainBreak struct {
	EventID string
	Detail  string
}

// ChainReport is the output of VerifyChain.
type ChainReport struct {
	Total             int
	OK                bool
	Signed            bool   // true when signingKey != ""
	HeadID            string // event_id of the first (oldest) event
	TailID            string // event_id of the last (newest) event
	Breaks            []ChainBreak
	SignatureFailures []ChainBreak
	// Truncated is true when the store had more than maxScanObjects objects;
	// the chain verdict covers only the scanned subset.
	Truncated bool
	// Skipped is the count of objects that failed to fetch/decode and were
	// silently skipped. ok=true with Skipped>0 means the scanned-and-decoded
	// subset was verified, not the full chain.
	Skipped int
}

// VerifyChain verifies the hash-chain continuity and (when signingKey != "")
// the HMAC-SHA256 signatures of the supplied events, using a single static
// key for every event (i.e. version 0 only). Legacy/non-versioned
// convenience, equivalent to VerifyChainVersioned with a staticKeySource —
// kept so callers that never adopted Vault-backed signing keys are unaffected.
func VerifyChain(events []*auditv1.AuditEvent, signingKey string) ChainReport {
	return VerifyChainVersioned(context.Background(), events, staticKeySource{key: signingKey})
}

// VerifyChainVersioned verifies hash-chain continuity and, per event, the
// HMAC-SHA256 signature keyed by that event's recorded signing_key_version
// (CP1.2) — so a chain spanning a Vault key rotation (older events signed
// under version N, newer ones under N+1) still verifies cleanly. It does not
// mutate the caller's slice or the events within it. Canonicalization matches
// emitter.go exactly: json.Marshal of the event with Signature="" and every
// other field (including signing_key_version and prior_event_hash) left as
// stored.
//
// Each distinct signing_key_version is resolved from keySource at most once
// per call — cached in a local map — so a long chain signed under a handful
// of rotations costs a handful of Vault reads, not one per event.
func VerifyChainVersioned(ctx context.Context, events []*auditv1.AuditEvent, keySource SigningKeySource) ChainReport {
	if len(events) == 0 {
		return ChainReport{OK: true}
	}

	// Work on a shallow copy of the slice so sorting doesn't affect the caller.
	// event_id order is only used for deterministic iteration: the chain order
	// is the prior_event_hash linkage, which the emitter assigns under its lock
	// AFTER the caller minted the (UUIDv7) event_id — so under concurrency
	// chain order and id order legitimately differ.
	sorted := make([]*auditv1.AuditEvent, len(events))
	copy(sorted, events)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].EventId < sorted[j].EventId
	})

	report := ChainReport{Total: len(sorted)}

	// Canonical bytes (for signatures) and chain hash of every event, once.
	canonBytes := make([][]byte, len(sorted))
	hashes := make([]string, len(sorted))
	byHash := make(map[string]int, len(sorted))
	children := map[string][]int{} // prior_event_hash → successors, event_id order
	var roots []int                // events whose predecessor is absent
	for i, e := range sorted {
		canonBytes[i] = canonicalBytes(e)
		h := eventHash(canonBytes[i])
		hashes[i] = h
		if j, dup := byHash[h]; dup {
			report.Breaks = append(report.Breaks, ChainBreak{
				EventID: e.EventId,
				Detail:  fmt.Sprintf("duplicate event: identical to %s", sorted[j].EventId),
			})
			continue // keep duplicates out of the linkage so they don't also read as a fork
		}
		byHash[h] = i
	}
	for i, e := range sorted {
		if byHash[hashes[i]] != i {
			continue // duplicate, reported above
		}
		children[e.PriorEventHash] = append(children[e.PriorEventHash], i)
	}
	for i, e := range sorted {
		if byHash[hashes[i]] != i {
			continue
		}
		if _, ok := byHash[e.PriorEventHash]; !ok {
			roots = append(roots, i)
		}
	}

	// Walk the linkage from the head. A healthy chain has exactly one root
	// (empty prior) and no event with more than one successor; every extra
	// root is a gap (missing predecessor) or a restarted chain, every extra
	// successor a fork.
	head := 0
	if len(roots) > 0 {
		head = roots[0]
	}
	for _, r := range roots {
		e := sorted[r]
		switch {
		case r == head && e.PriorEventHash != "":
			report.Breaks = append(report.Breaks, ChainBreak{
				EventID: e.EventId,
				Detail:  fmt.Sprintf("head event has non-empty prior_event_hash: %s", e.PriorEventHash),
			})
		case r == head:
		case e.PriorEventHash == "":
			report.Breaks = append(report.Breaks, ChainBreak{
				EventID: e.EventId,
				Detail:  fmt.Sprintf("second chain head (empty prior_event_hash) besides %s", sorted[head].EventId),
			})
		default:
			report.Breaks = append(report.Breaks, ChainBreak{
				EventID: e.EventId,
				Detail:  fmt.Sprintf("prior_event_hash mismatch: %s matches no event (missing predecessor)", e.PriorEventHash),
			})
		}
	}
	for prior, succ := range children {
		if prior == "" {
			continue // multiple empty-prior roots already reported above
		}
		for _, k := range succ[1:] {
			report.Breaks = append(report.Breaks, ChainBreak{
				EventID: sorted[k].EventId,
				Detail:  fmt.Sprintf("fork: shares prior_event_hash %s with %s", prior, sorted[succ[0]].EventId),
			})
		}
	}
	// Map iteration above is random; keep the report deterministic.
	sort.SliceStable(report.Breaks, func(i, j int) bool { return report.Breaks[i].EventID < report.Breaks[j].EventID })

	tail := head
	for {
		next := children[hashes[tail]]
		if len(next) == 0 {
			break
		}
		tail = next[0]
	}
	report.HeadID = sorted[head].EventId
	report.TailID = sorted[tail].EventId

	keyCache := map[int32]string{}
	keyErr := map[int32]error{}
	resolveKey := func(version int32) (string, error) {
		if k, ok := keyCache[version]; ok {
			return k, nil
		}
		if err, ok := keyErr[version]; ok {
			return "", err
		}
		k, err := keySource.ForVersion(ctx, version)
		if err != nil {
			keyErr[version] = err
			return "", err
		}
		keyCache[version] = k
		return k, nil
	}

	// Resolve every event's key up front (cached per version above) so we know,
	// before the per-event pass, whether ANY event in the chain resolves to a
	// non-empty key. That distinction matters: an empty resolved key is
	// "legitimately unsigned" only in a wholly-unsigned legacy chain. In a
	// chain where at least one event carries real signing (some version
	// resolves non-empty), an event whose recorded version resolves empty is
	// not a benign legacy event — it's indistinguishable from an attacker
	// rewriting signing_key_version to an unsigned value to dodge the
	// signature check, so it must be treated as a signature failure.
	resolvedKeys := make([]string, len(sorted))
	resolvedErrs := make([]error, len(sorted))
	anySigned := false
	for i, e := range sorted {
		key, err := resolveKey(e.SigningKeyVersion)
		resolvedKeys[i], resolvedErrs[i] = key, err
		if err == nil && key != "" {
			anySigned = true
		}
	}
	report.Signed = anySigned

	for i, e := range sorted {
		canon := canonBytes[i]
		// Signature check. An unresolvable version (e.g. a Vault historic
		// read failing) is itself a signature failure — the chain can't be
		// trusted end-to-end, not silently skipped.
		key, err := resolvedKeys[i], resolvedErrs[i]
		if err != nil {
			report.SignatureFailures = append(report.SignatureFailures, ChainBreak{
				EventID: e.EventId,
				Detail:  fmt.Sprintf("key resolution failed for signing_key_version %d: %v", e.SigningKeyVersion, err),
			})
			continue
		}
		if key == "" {
			if anySigned {
				// Mixed chain: an event whose version resolves unsigned while
				// others resolve signed is a downgrade, not a legacy pass.
				report.SignatureFailures = append(report.SignatureFailures, ChainBreak{
					EventID: e.EventId,
					Detail: fmt.Sprintf("signing_key_version %d resolves to an unsigned key in a chain that has signed events — treating as a signature downgrade",
						e.SigningKeyVersion),
				})
			}
			continue // wholly-unsigned chain: this version is unsigned by design
		}
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write(canon)
		want := hex.EncodeToString(mac.Sum(nil))
		if e.Signature != want {
			report.SignatureFailures = append(report.SignatureFailures, ChainBreak{
				EventID: e.EventId,
				Detail:  fmt.Sprintf("HMAC mismatch: got %s want %s", e.Signature, want),
			})
		}
	}

	report.OK = len(report.Breaks) == 0 && len(report.SignatureFailures) == 0
	return report
}

// canonicalBytes returns the JSON encoding of e with Signature="" and all other
// fields (including PriorEventHash) left as stored — matching emitter.go exactly.
// A clone is used so the original event is never mutated, regardless of caller context.
func canonicalBytes(e *auditv1.AuditEvent) []byte {
	clone := proto.Clone(e).(*auditv1.AuditEvent)
	clone.Signature = ""
	b, _ := json.Marshal(clone)
	return b
}

// eventHash returns the hex-encoded SHA-256 of canonical bytes.
func eventHash(canon []byte) string {
	sum := sha256.Sum256(canon)
	return hex.EncodeToString(sum[:])
}
