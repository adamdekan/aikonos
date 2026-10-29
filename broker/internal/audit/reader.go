package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"

	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

// maxScanObjects is the hard cap on objects scanned in one Query call.
const maxScanObjects = 5000

// maxVerifyObjects caps one VerifyChain walk. Verify streams the whole trail
// and keeps only each event's link (~250 bytes), so the cap bounds memory and
// run time rather than coverage of any realistic tenant; past it the report is
// Truncated and OK=false. A var so tests can lower it.
// ponytail: one walk per Verify call, cut here or verifyMargin before the
// caller's deadline (an "incomplete" verdict); verify from a persisted,
// signed checkpoint of the last verified head if trails outgrow a deadline.
var maxVerifyObjects = 1_000_000

// verifyWorkers bounds the concurrent object GETs of one VerifyChain walk.
const verifyWorkers = 16

// verifyMargin is how long before the caller's deadline VerifyChain stops
// walking, to leave time to build and return the report: a walk that runs out
// of time comes back Truncated ("incomplete"), never as an error. A var so
// tests can shorten it.
var verifyMargin = 2 * time.Second

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
	store  chainStore // nil in logging-only mode
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

	prefix := keySegment(tenant) + "/"
	metas, err := r.listNewest(ctx, prefix, prefix, f, nil)
	if err != nil {
		r.logger.Warn("audit reader: list objects failed", zap.String("tenant", tenant), zap.Error(err))
		return nil, "", true, err
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

// listNewest collects up to maxScanObjects object keys under prefix, walking
// the <tenant>/YYYY/MM/DD/ date directories newest-first and skipping those
// outside f's time range, so the scan cap drops the oldest events rather than
// the newest. Within a directory, keys (UUIDv7 event ids) go newest-first.
func (r *Reader) listNewest(ctx context.Context, root, prefix string, f QueryFilter, out []objMeta) ([]objMeta, error) {
	dirs, objs, err := r.store.listLevel(ctx, prefix)
	if err != nil {
		return out, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
	for _, d := range dirs {
		if len(out) >= maxScanObjects {
			return out, nil
		}
		if dateDirOutside(strings.TrimSuffix(strings.TrimPrefix(d, root), "/"), f) {
			continue
		}
		if out, err = r.listNewest(ctx, root, d, f, out); err != nil {
			return out, err
		}
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].key > objs[j].key })
	for _, o := range objs {
		if len(out) >= maxScanObjects {
			break
		}
		out = append(out, o)
	}
	return out, nil
}

// dateDirOutside reports whether the date directory rel ("YYYY", "YYYY/MM" or
// "YYYY/MM/DD", relative to the tenant) lies wholly outside f's time range.
// Anything not shaped like a date directory is never pruned.
func dateDirOutside(rel string, f QueryFilter) bool {
	const layout = "2006/01/02"
	if n := len(rel); n != 4 && n != 7 && n != 10 {
		return false
	}
	if _, err := time.Parse(layout[:len(rel)], rel); err != nil {
		return false
	}
	if !f.StartTime.IsZero() && rel < f.StartTime.UTC().Format(layout)[:len(rel)] {
		return true
	}
	return !f.EndTime.IsZero() && rel > f.EndTime.UTC().Format(layout)[:len(rel)]
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

// walkOldest visits every object under prefix oldest first — the
// <tenant>/YYYY/MM/DD/ date directories ascending, keys ascending within one —
// until visit returns false. Only one listing level is held at a time.
func (r *Reader) walkOldest(ctx context.Context, prefix string, visit func(objMeta) bool) (bool, error) {
	dirs, objs, err := r.store.listLevel(ctx, prefix)
	if err != nil {
		return false, err
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].key < objs[j].key })
	for _, o := range objs {
		if !visit(o) {
			return false, nil
		}
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		if more, err := r.walkOldest(ctx, d, visit); !more || err != nil {
			return false, err
		}
	}
	return true, nil
}

// VerifyChain streams every one of the tenant's events, oldest first, through
// the chain verifier with the Reader's configured signing key. Returns
// (ChainReport{}, false, nil) when the store is not configured, and a non-nil
// error when a listing fails — callers must propagate it rather than treat it
// as a clean report. An object that cannot be fetched or decoded counts in
// Skipped, and a walk cut at maxVerifyObjects, or stopped verifyMargin before
// ctx's deadline, sets Truncated; either leaves part of the chain unverified,
// so OK is false. Objects are fetched verifyWorkers at a time.
func (r *Reader) VerifyChain(ctx context.Context, tenant string) (ChainReport, bool, error) {
	if r.store == nil {
		return ChainReport{}, false, nil
	}
	keySource := r.cfg.SigningKeySource
	if keySource == nil {
		keySource = staticKeySource{key: r.cfg.SigningKey}
	}
	v := newChainVerifier(ctx, keySource)
	walkCtx := ctx
	if dl, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		walkCtx, cancel = context.WithDeadline(ctx, dl.Add(-verifyMargin))
		defer cancel()
	}
	var (
		mu      sync.Mutex // guards v and skipped
		wg      sync.WaitGroup
		slots   = make(chan struct{}, verifyWorkers)
		skipped int
	)
	seen, truncated := 0, false
	_, err := r.walkOldest(walkCtx, keySegment(tenant)+"/", func(m objMeta) bool {
		if seen == maxVerifyObjects || walkCtx.Err() != nil {
			truncated = true
			return false
		}
		seen++
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-slots; wg.Done() }()
			// ctx, not walkCtx: a started fetch completes, so a cut walk has
			// verified a gap-free prefix (a gap reads as a missing predecessor).
			data, err := r.store.get(ctx, m.key)
			var ev *auditv1.AuditEvent
			if err == nil {
				ev, err = unmarshalAuditEvent(data)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				r.logger.Warn("audit reader: unreadable object in VerifyChain, skipping", zap.String("key", m.key), zap.Error(err))
				skipped++
				return
			}
			v.add(ev)
		}()
		return true
	})
	wg.Wait()
	if err != nil && walkCtx.Err() != nil {
		err, truncated = nil, true // the listing ran out of time, not the store
	}
	if err != nil {
		r.logger.Warn("audit reader: list objects failed in VerifyChain", zap.String("tenant", tenant), zap.Error(err))
		return ChainReport{}, true, err
	}
	for version, kerr := range v.keyErrs {
		r.logger.Warn("audit reader: signing key unavailable in VerifyChain", zap.Int32("signing_key_version", version), zap.Error(kerr))
	}
	report := v.report()
	report.Truncated = truncated
	report.Skipped = skipped
	if truncated || skipped > 0 {
		report.OK = false
	}
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
