package audit

// policy_archive.go — archived policy snapshots and single-event reads for
// decision evidence (docs/15-decision-replay.md).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// PolicyBundlePrefix holds archived policy snapshots in the audit bucket. The
// leading underscore keeps it apart from tenant prefixes, which are UUIDs.
const PolicyBundlePrefix = "_policy/bundles/"

// ErrNotFound means the requested event or snapshot is not in the store.
var ErrNotFound = errors.New("not found in the audit store")

// PolicyBundleKey is the object key of an archived policy revision
// ("sha256:<64 hex>").
func PolicyBundleKey(revision string) (string, error) {
	h, ok := strings.CutPrefix(revision, "sha256:")
	if !ok || len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
		return "", fmt.Errorf("invalid policy revision %q", revision)
	}
	return PolicyBundlePrefix + h + ".json", nil
}

// Persistent reports whether the emitter writes to an object store (false in
// logging-only mode).
func (e *Emitter) Persistent() bool { return e.client != nil }

// ArchivePolicyBundle writes a policy snapshot under its revision,
// synchronously: the caller serves that policy only once this returns nil.
// The key is the content digest, so an existing object is the same snapshot
// and is left alone; under the bucket's object lock it could not be replaced.
func (e *Emitter) ArchivePolicyBundle(ctx context.Context, revision string, snapshot []byte) error {
	if e.client == nil {
		return errors.New("audit store not configured")
	}
	key, err := PolicyBundleKey(revision)
	if err != nil {
		return err
	}
	if _, err := e.client.StatObject(ctx, e.cfg.Bucket, key, minio.StatObjectOptions{}); err == nil {
		return nil
	} else if minio.ToErrorResponse(err).Code != "NoSuchKey" {
		return fmt.Errorf("stat %s: %w", key, err)
	}
	if _, err := e.client.PutObject(ctx, e.cfg.Bucket, key, bytes.NewReader(snapshot), int64(len(snapshot)),
		minio.PutObjectOptions{ContentType: "application/json"}); err != nil {
		return fmt.Errorf("put %s: %w", key, err)
	}
	return nil
}

// GetPolicyBundle returns the archived snapshot for a revision. ok=false when
// the store is not configured.
func (r *Reader) GetPolicyBundle(ctx context.Context, revision string) ([]byte, bool, error) {
	if r.store == nil {
		return nil, false, nil
	}
	key, err := PolicyBundleKey(revision)
	if err != nil {
		return nil, true, err
	}
	data, err := r.store.get(ctx, key)
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, true, ErrNotFound
		}
		return nil, true, err
	}
	return data, true, nil
}

// GetEvent returns one of a tenant's events, decoded and as stored. Events are
// stored under the day they occurred; event ids are UUIDv7, so the id's own
// timestamp names the day, give or take one around midnight. Only when none of
// those keys exist does it fall back to listing the tenant's events.
func (r *Reader) GetEvent(ctx context.Context, tenant, eventID string) (*auditv1.AuditEvent, []byte, bool, error) {
	if r.store == nil {
		return nil, nil, false, nil
	}
	id, err := uuid.Parse(eventID)
	if err != nil || id.String() != eventID {
		return nil, nil, true, fmt.Errorf("invalid event id %q", eventID)
	}
	var candidates []string
	if id.Version() == 7 {
		sec, nsec := id.Time().UnixTime()
		day := time.Unix(sec, nsec).UTC()
		for _, d := range []time.Time{day, day.AddDate(0, 0, -1), day.AddDate(0, 0, 1)} {
			candidates = append(candidates, fmt.Sprintf("%s/%04d/%02d/%02d/%s.json", tenant, d.Year(), int(d.Month()), d.Day(), eventID))
		}
	}
	for _, key := range candidates {
		if data, err := r.store.get(ctx, key); err == nil {
			return decodeStoredEvent(tenant, eventID, data)
		}
	}
	metas, err := r.store.list(ctx, tenant+"/")
	if err != nil {
		return nil, nil, true, err
	}
	for _, m := range metas {
		if strings.HasSuffix(m.key, "/"+eventID+".json") {
			data, err := r.store.get(ctx, m.key)
			if err != nil {
				return nil, nil, true, err
			}
			return decodeStoredEvent(tenant, eventID, data)
		}
	}
	return nil, nil, true, ErrNotFound
}

func decodeStoredEvent(tenant, eventID string, data []byte) (*auditv1.AuditEvent, []byte, bool, error) {
	ev, err := unmarshalAuditEvent(data)
	if err != nil {
		return nil, nil, true, fmt.Errorf("decode event %s: %w", eventID, err)
	}
	if ev.EventId != eventID || ev.TenantId != tenant {
		return nil, nil, true, fmt.Errorf("stored object for %s holds event %s of tenant %s", eventID, ev.EventId, ev.TenantId)
	}
	return ev, data, true, nil
}

// EventHash is the chain hash of an event: the value the next event in its
// tenant's chain records as prior_event_hash.
func EventHash(e *auditv1.AuditEvent) string { return eventHash(canonicalBytes(e)) }

// SignatureStatus is the outcome of checking one event's HMAC signature.
type SignatureStatus string

const (
	SignatureValid    SignatureStatus = "valid"
	SignatureInvalid  SignatureStatus = "invalid"
	SignatureUnsigned SignatureStatus = "unsigned"   // signed with no key (legacy or dev)
	SignatureUnknown  SignatureStatus = "unverified" // the key for its version is unavailable
)

// VerifyEventSignature checks one event's signature under the key of its
// recorded signing_key_version. Continuity with its neighbours is the chain
// verifier's job (VerifyAuditChain).
func (r *Reader) VerifyEventSignature(ctx context.Context, e *auditv1.AuditEvent) SignatureStatus {
	keySource := r.cfg.SigningKeySource
	if keySource == nil {
		keySource = staticKeySource{key: r.cfg.SigningKey}
	}
	key, err := keySource.ForVersion(ctx, e.SigningKeyVersion)
	if err != nil {
		return SignatureUnknown
	}
	if key == "" {
		if e.Signature == "" {
			return SignatureUnsigned
		}
		return SignatureUnknown
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write(canonicalBytes(e))
	if hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(e.Signature)) {
		return SignatureValid
	}
	return SignatureInvalid
}
