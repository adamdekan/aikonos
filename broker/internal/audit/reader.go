package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// maxScanObjects is the hard cap on objects scanned in one Query call.
const maxScanObjects = 5000

// maxLimit is the maximum page size a caller may request.
const maxLimit = 200

// defaultLimit is used when limit is 0.
const defaultLimit = 50

// QueryFilter holds optional predicates for a Query call.
type QueryFilter struct {
	StartTime   time.Time
	EndTime     time.Time
	ActorUserID string
	EventType   string
	Decision    auditv1.PolicyDecision // UNSPECIFIED = any
	Limit       int
	Cursor      string // opaque; empty = first page; continuation uses event_id < cursor
}

// ReaderIface is the interface the broker handler calls.
type ReaderIface interface {
	// Query returns a page of audit events. ok=false when the store is not
	// configured. A non-nil error means the store is configured but the list
	// call failed — callers must not treat this as an empty result.
	Query(ctx context.Context, tenant string, f QueryFilter) (events []*auditv1.AuditEvent, nextCursor string, ok bool, err error)
	// VerifyChain reads all events for the tenant and verifies the hash chain +
	// HMAC signatures. ok=false when the object store is not configured (no
	// error). A non-nil error indicates a store I/O failure that prevented
	// reading — callers must not treat this as a clean chain result.
	VerifyChain(ctx context.Context, tenant string) (ChainReport, bool, error)
}

// objMeta carries the key and last-modified time of a listed object.
type objMeta struct {
	key          string
	lastModified time.Time
}

// objectStore abstracts MinIO list/get operations so Reader is testable without
// a real MinIO server.
type objectStore interface {
	list(ctx context.Context, prefix string) ([]objMeta, error)
	get(ctx context.Context, key string) ([]byte, error)
}

// minioStore is the thin production wrapper around minio-go.
type minioStore struct {
	client *minio.Client
	bucket string
}

func (m *minioStore) list(ctx context.Context, prefix string) ([]objMeta, error) {
	var out []objMeta
	for obj := range m.client.ListObjects(ctx, m.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		if strings.HasSuffix(obj.Key, "/") {
			continue
		}
		out = append(out, objMeta{key: obj.Key, lastModified: obj.LastModified})
	}
	return out, nil
}

func (m *minioStore) get(ctx context.Context, key string) ([]byte, error) {
	obj, err := m.client.GetObject(ctx, m.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(obj)
}

// Reader reads audit events from the MinIO object store.  When the store is
// nil (logging-only mode) every Query call returns ok=false.
type Reader struct {
	store  objectStore // nil in logging-only mode
	cfg    Config
	logger *zap.Logger
}

// NewReader constructs a Reader from an audit Config.  Logging-only (no MinIO
// config) → store is nil; Query returns ok=false.
func NewReader(cfg Config) *Reader {
	r := &Reader{cfg: cfg, logger: zap.NewNop()}
	if cfg.MinioEndpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		return r
	}
	client, err := minio.New(cfg.MinioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		// Fail-safe: return a logging-only reader rather than a hard error.
		return r
	}
	r.store = &minioStore{client: client, bucket: cfg.Bucket}
	return r
}

// WithLogger attaches a logger to the Reader (mirrors Emitter convention).
// Safe to call after NewReader; not concurrency-safe after first Query call.
func (r *Reader) WithLogger(l *zap.Logger) *Reader {
	r.logger = l
	return r
}

// Query returns a page of audit events for tenant matching the filter.
// ok=false means the object store is not configured (nil, "", false, nil).
// A non-nil error means the store is configured but the list call failed —
// callers must not treat this as an empty result.
func (r *Reader) Query(ctx context.Context, tenant string, f QueryFilter) ([]*auditv1.AuditEvent, string, bool, error) {
	if r.store == nil {
		return nil, "", false, nil
	}

	prefix := tenant + "/"
	metas, err := r.store.list(ctx, prefix)
	if err != nil {
		r.logger.Warn("audit reader: list objects failed", zap.String("tenant", tenant), zap.Error(err))
		return nil, "", true, err
	}

	// Limit the total number of objects scanned.
	if len(metas) > maxScanObjects {
		metas = metas[:maxScanObjects]
	}

	var events []*auditv1.AuditEvent
	for _, m := range metas {
		data, err := r.store.get(ctx, m.key)
		if err != nil {
			r.logger.Warn("audit reader: get object failed, skipping", zap.String("key", m.key), zap.Error(err))
			continue
		}
		ev, err := unmarshalAuditEvent(data)
		if err != nil {
			continue
		}
		events = append(events, ev)
	}

	page, next := filterAndPage(events, f)
	return page, next, true, nil
}

// filterAndPage is the pure, unit-tested helper that applies all QueryFilter
// predicates, sorts newest-first by event_id, applies the cursor, pages by
// limit, and returns the page + next-page cursor.
func filterAndPage(events []*auditv1.AuditEvent, f QueryFilter) ([]*auditv1.AuditEvent, string) {
	lim := f.Limit
	if lim <= 0 {
		lim = defaultLimit
	}
	if lim > maxLimit {
		lim = maxLimit
	}

	// Filter pass.
	var filtered []*auditv1.AuditEvent
	for _, e := range events {
		if f.ActorUserID != "" && e.ActorUserId != f.ActorUserID {
			continue
		}
		if f.EventType != "" && e.EventType != f.EventType {
			continue
		}
		if f.Decision != auditv1.PolicyDecision_POLICY_DECISION_UNSPECIFIED && e.Decision != f.Decision {
			continue
		}
		if !f.StartTime.IsZero() && e.OccurredAt != nil {
			if e.OccurredAt.AsTime().Before(f.StartTime) {
				continue
			}
		}
		if !f.EndTime.IsZero() && e.OccurredAt != nil {
			if e.OccurredAt.AsTime().After(f.EndTime) {
				continue
			}
		}
		filtered = append(filtered, e)
	}

	// Sort by event_id descending (UUIDv7 → lexical = time-order).
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].EventId > filtered[j].EventId
	})

	// Cursor: skip events with event_id >= cursor (return only those strictly less).
	if f.Cursor != "" {
		var after []*auditv1.AuditEvent
		for _, e := range filtered {
			if e.EventId < f.Cursor {
				after = append(after, e)
			}
		}
		filtered = after
	}

	// Page.
	if len(filtered) <= lim {
		return filtered, ""
	}
	page := filtered[:lim]
	next := page[len(page)-1].EventId
	return page, next
}

// scanResult carries the output of readAll including incompleteness signals.
type scanResult struct {
	events    []*auditv1.AuditEvent
	truncated bool // true when total objects > maxScanObjects
	skipped   int  // count of objects that failed to fetch/decode
}

// readAll fetches and decodes every event for tenant (up to maxScanObjects).
// Returns (scanResult{}, false, nil) when the store is not configured.
// Returns (scanResult{}, true, err) when the list call fails — a fatal I/O
// error that must not be silently treated as an empty (intact) chain.
// Individual get/unmarshal failures increment skipped and continue.
func (r *Reader) readAll(ctx context.Context, tenant string) (scanResult, bool, error) {
	if r.store == nil {
		return scanResult{}, false, nil
	}
	prefix := tenant + "/"
	metas, err := r.store.list(ctx, prefix)
	if err != nil {
		r.logger.Warn("audit reader: list objects failed in readAll", zap.String("tenant", tenant), zap.Error(err))
		return scanResult{}, true, err
	}
	var res scanResult
	if len(metas) > maxScanObjects {
		metas = metas[:maxScanObjects]
		res.truncated = true
	}
	for _, m := range metas {
		data, err := r.store.get(ctx, m.key)
		if err != nil {
			r.logger.Warn("audit reader: get object failed in readAll, skipping", zap.String("key", m.key), zap.Error(err))
			res.skipped++
			continue
		}
		ev, err := unmarshalAuditEvent(data)
		if err != nil {
			res.skipped++
			continue
		}
		res.events = append(res.events, ev)
	}
	return res, true, nil
}

// VerifyChain reads all tenant events and runs the pure chain verifier with the
// Reader's configured signing key. Returns (ChainReport{}, false, nil) when the
// store is not configured. Returns a non-nil error when the underlying list
// fails — callers must propagate this rather than treating it as a clean report.
// The returned ChainReport carries Truncated and Skipped to signal that the
// verdict covers only the scanned subset when either field is non-zero.
func (r *Reader) VerifyChain(ctx context.Context, tenant string) (ChainReport, bool, error) {
	res, configured, err := r.readAll(ctx, tenant)
	if !configured {
		return ChainReport{}, false, nil
	}
	if err != nil {
		return ChainReport{}, true, err
	}
	keySource := r.cfg.SigningKeySource
	if keySource == nil {
		keySource = staticKeySource{key: r.cfg.SigningKey}
	}
	report := VerifyChainVersioned(ctx, res.events, keySource)
	report.Truncated = res.truncated
	report.Skipped = res.skipped
	// Surface the "entire chain unsigned" case beyond a boot-time log line: OK
	// stays true for a consistent-but-unsigned chain, so an ops alert needs its
	// own signal — this fires every time such a chain is verified, not just once.
	if report.Total > 0 && !report.Signed {
		RecordChainUnsigned(ctx, tenant)
	}
	return report, true, nil
}

// Configured reports whether the reader has a live object store. Use this to
// decide whether to assign the reader to the ReaderIface slot in Deps (a nil
// *Reader satisfies ReaderIface but is a non-nil interface, which breaks the
// nil check in the handler — so callers assign nil when unconfigured).
func (r *Reader) Configured() bool { return r.store != nil }

// Ensure Reader implements ReaderIface at compile time.
var _ ReaderIface = (*Reader)(nil)

// unmarshalAuditEvent decodes a JSON blob written by the emitter. Plain
// encoding/json (emitter.go) is the canonical format — protojson is accepted
// here defensively only, in case any object was ever written by a protojson
// emitter. Dual-parse order (protojson first, then plain JSON) is unchanged.
func unmarshalAuditEvent(data []byte) (*auditv1.AuditEvent, error) {
	ev := &auditv1.AuditEvent{}
	// protojson handles Timestamp/Struct correctly.
	if err := protojson.Unmarshal(data, ev); err == nil {
		return ev, nil
	}
	// Fall back to plain JSON for events written by older emitters.
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(ev); err != nil {
		return nil, err
	}
	return ev, nil
}
