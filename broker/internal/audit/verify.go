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
	// Truncated is true when the store had more than maxVerifyObjects objects;
	// the newest ones were not verified, so OK is false (an "incomplete"
	// verdict when there are no Breaks or SignatureFailures).
	Truncated bool
	// Skipped is the count of objects that failed to fetch/decode. Any skip
	// leaves the chain unverified there, so OK is false ("incomplete").
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
func VerifyChainVersioned(ctx context.Context, events []*auditv1.AuditEvent, keySource SigningKeySource) ChainReport {
	v := newChainVerifier(ctx, keySource)
	for _, e := range events {
		v.add(e)
	}
	return v.report()
}

// chainLink is what the continuity check keeps of an event: its id, its own
// chain hash and the hash it names as its predecessor.
type chainLink struct{ id, hash, prior string }

// chainVerifier checks a chain one event at a time, so a caller can stream a
// tenant's whole trail through it while holding only the links, never the
// events. Signatures are checked as events arrive; continuity is decided in
// report, once every link is known.
//
// Each distinct signing_key_version is resolved from keySource at most once —
// cached in a local map — so a long chain signed under a handful of rotations
// costs a handful of Vault reads, not one per event.
type chainVerifier struct {
	ctx        context.Context
	keySource  SigningKeySource
	keys       map[int32]string
	keyErrs    map[int32]error
	links      []chainLink
	sigFails   []ChainBreak
	downgrades []ChainBreak // unsigned events: failures only if anySigned
	anySigned  bool
}

func newChainVerifier(ctx context.Context, keySource SigningKeySource) *chainVerifier {
	return &chainVerifier{ctx: ctx, keySource: keySource, keys: map[int32]string{}, keyErrs: map[int32]error{}}
}

func (v *chainVerifier) resolveKey(version int32) (string, error) {
	if k, ok := v.keys[version]; ok {
		return k, nil
	}
	if err, ok := v.keyErrs[version]; ok {
		return "", err
	}
	k, err := v.keySource.ForVersion(v.ctx, version)
	if err != nil {
		v.keyErrs[version] = err
		return "", err
	}
	v.keys[version] = k
	return k, nil
}

// add records e's link and checks its signature.
func (v *chainVerifier) add(e *auditv1.AuditEvent) {
	canon := canonicalBytes(e)
	v.links = append(v.links, chainLink{id: e.EventId, hash: eventHash(canon), prior: e.PriorEventHash})

	// An unresolvable version (e.g. a Vault historic read failing) is itself
	// a signature failure — the chain can't be trusted end-to-end, not
	// silently skipped.
	key, err := v.resolveKey(e.SigningKeyVersion)
	if err != nil {
		v.sigFails = append(v.sigFails, ChainBreak{
			EventID: e.EventId,
			Detail:  fmt.Sprintf("key resolution failed for signing_key_version %d: %v", e.SigningKeyVersion, err),
		})
		return
	}
	if key == "" {
		// Judged in report: legitimately unsigned only if no event in the
		// whole chain resolves to a real key.
		v.downgrades = append(v.downgrades, ChainBreak{
			EventID: e.EventId,
			Detail: fmt.Sprintf("signing_key_version %d resolves to an unsigned key in a chain that has signed events — treating as a signature downgrade",
				e.SigningKeyVersion),
		})
		return
	}
	v.anySigned = true
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(canon)
	// Constant-time compare on the decoded bytes. The detail names neither
	// value: the expected MAC is a valid signature for the stored content, so
	// returning it would let anyone who can write the bucket forge events.
	if got, derr := hex.DecodeString(e.Signature); derr != nil || !hmac.Equal(got, mac.Sum(nil)) {
		v.sigFails = append(v.sigFails, ChainBreak{EventID: e.EventId, Detail: "HMAC mismatch"})
	}
}

// report decides continuity over every link added and returns the verdict.
func (v *chainVerifier) report() ChainReport {
	if len(v.links) == 0 {
		return ChainReport{OK: true}
	}

	// event_id order is only used for deterministic iteration: the chain order
	// is the prior_event_hash linkage, which older brokers assigned AFTER the
	// caller minted the (UUIDv7) event_id — so chain order and id order
	// legitimately differ.
	sorted := v.links
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].id < sorted[j].id })

	report := ChainReport{Total: len(sorted)}

	byHash := make(map[string]int, len(sorted))
	children := map[string][]int{} // prior_event_hash → successors, event_id order
	var roots []int                // events whose predecessor is absent
	for i, l := range sorted {
		if j, dup := byHash[l.hash]; dup {
			report.Breaks = append(report.Breaks, ChainBreak{
				EventID: l.id,
				Detail:  fmt.Sprintf("duplicate event: identical to %s", sorted[j].id),
			})
			continue // keep duplicates out of the linkage so they don't also read as a fork
		}
		byHash[l.hash] = i
	}
	for i, l := range sorted {
		if byHash[l.hash] != i {
			continue // duplicate, reported above
		}
		children[l.prior] = append(children[l.prior], i)
	}
	for i, l := range sorted {
		if byHash[l.hash] != i {
			continue
		}
		if _, ok := byHash[l.prior]; !ok {
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
		l := sorted[r]
		switch {
		case r == head && l.prior != "":
			report.Breaks = append(report.Breaks, ChainBreak{
				EventID: l.id,
				Detail:  fmt.Sprintf("head event has non-empty prior_event_hash: %s", l.prior),
			})
		case r == head:
		case l.prior == "":
			report.Breaks = append(report.Breaks, ChainBreak{
				EventID: l.id,
				Detail:  fmt.Sprintf("second chain head (empty prior_event_hash) besides %s", sorted[head].id),
			})
		default:
			report.Breaks = append(report.Breaks, ChainBreak{
				EventID: l.id,
				Detail:  fmt.Sprintf("prior_event_hash mismatch: %s matches no event (missing predecessor)", l.prior),
			})
		}
	}
	for prior, succ := range children {
		if prior == "" {
			continue // multiple empty-prior roots already reported above
		}
		for _, k := range succ[1:] {
			report.Breaks = append(report.Breaks, ChainBreak{
				EventID: sorted[k].id,
				Detail:  fmt.Sprintf("fork: shares prior_event_hash %s with %s", prior, sorted[succ[0]].id),
			})
		}
	}
	// Map iteration above is random; keep the report deterministic.
	sort.SliceStable(report.Breaks, func(i, j int) bool { return report.Breaks[i].EventID < report.Breaks[j].EventID })

	tail := head
	for {
		next := children[sorted[tail].hash]
		if len(next) == 0 {
			break
		}
		tail = next[0]
	}
	report.HeadID = sorted[head].id
	report.TailID = sorted[tail].id

	// An empty resolved key is "legitimately unsigned" only in a
	// wholly-unsigned legacy chain. In a chain where at least one event
	// resolves to a real key, an event whose recorded version resolves empty
	// is indistinguishable from an attacker rewriting signing_key_version to
	// an unsigned value to dodge the signature check — a signature failure.
	report.Signed = v.anySigned
	report.SignatureFailures = v.sigFails
	if v.anySigned {
		report.SignatureFailures = append(report.SignatureFailures, v.downgrades...)
	}
	sort.SliceStable(report.SignatureFailures, func(i, j int) bool {
		return report.SignatureFailures[i].EventID < report.SignatureFailures[j].EventID
	})

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
