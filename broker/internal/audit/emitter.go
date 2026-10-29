// broker/internal/audit/emitter.go
// Audit emitter — hash-chained, signed AuditEvents written to MinIO (WORM via
// object lock). Falls back to logging-only when MinIO isn't configured, so the
// broker still runs (and still publishes to the live observability stream)
// without object storage. With MinIO, one worker chains, signs and writes the
// events in queue order and advances a tenant's chain head only once the write
// has landed, so a dropped or failed event never leaves a hole in the chain.
// Emit waits for that write and returns its error, except for the per-RPC
// envelope of an allowed call (RPCEventType), which stays fire-and-forget.
package audit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	"github.com/adamdekan/aikonos/broker/internal/notify"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

type Config struct {
	MinioEndpoint string
	Bucket        string
	TenantID      string
	AccessKey     string
	SecretKey     string
	UseSSL        bool
	// SigningKey is the HMAC-SHA256 key for event signatures. Empty → events are
	// chained but unsigned. Ignored when SigningKeySource is set.
	SigningKey string
	// SigningKeySource, when set, supersedes SigningKey and additionally
	// records each event's signing_key_version (CP1.2 — Vault KV-v2 versioned
	// audit key). Nil → falls back to a static source wrapping SigningKey,
	// version 0.
	SigningKeySource SigningKeySource
	// RetentionDays sets the bucket's default object-lock retention when this
	// emitter creates the bucket. 0 → lock-enabled bucket with no default
	// retention (a default makes WORM automatic for every written object).
	RetentionDays int
	// DropTimeout is how long Emit blocks on a full upload queue before
	// dropping the event. 0 → default of 100ms.
	DropTimeout time.Duration
}

// chainHashMeta is the S3 user-metadata key carrying each event's own hash (the
// value the next event chains to). Stored on write, read back on startup to
// resume the per-tenant chain across restarts.
const chainHashMeta = "chain-hash"

// RPCEventType is the per-RPC envelope event the auth interceptor emits for
// every call. Its ALLOW form is the one event Emit does not wait to persist:
// it is the highest-volume event and records no mutation of its own.
const RPCEventType = "aikonos.broker.rpc"

// putBackoff is the wait before the first retry of a failed write; it grows
// linearly per attempt. A var so tests can shorten it.
var putBackoff = 500 * time.Millisecond

type uploadJob struct {
	event *auditv1.AuditEvent // not yet chained: the worker chains and signs it
	done  chan error          // buffered(1): the persistence outcome
}

// EmailResolver maps a (tenantID, subject) pair to a human-readable email
// address for audit display. Implementations must be safe for concurrent use.
// A miss or error returns "" — never fails the audited operation.
type EmailResolver interface {
	Email(ctx context.Context, tenantID, subject string) string
}

type Emitter struct {
	cfg    Config
	logger *zap.Logger
	bus    notify.Bus // optional: publishes audit events for live observability

	client *minio.Client // nil → logging-only
	store  chainStore    // list/get view over client, used for chain resume

	keySource SigningKeySource // never nil after NewEmitter

	emailResolver EmailResolver // optional: enriches actor_email before chain/sign

	dropTimeout time.Duration

	mu       sync.Mutex
	lastHash map[string]string // keySegment(tenant) → hash of its last persisted event (chain head)

	queue     chan uploadJob
	workerCtx context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
}

// AttachBus wires a NATS bus so Emit also publishes each event to the per-tenant
// audit subject (aikonos.audit.<tenant>) for the observability stream. Best-effort.
func (e *Emitter) AttachBus(bus notify.Bus) { e.bus = bus }

// AttachEmailResolver wires a resolver that enriches actor_email before the
// chain/sign step. Best-effort: a nil resolver or a resolver returning "" leaves
// actor_email empty — the view falls back to actor_user_id. Mirrors AttachBus.
func (e *Emitter) AttachEmailResolver(r EmailResolver) { e.emailResolver = r }

func NewEmitter(ctx context.Context, cfg Config) (*Emitter, error) {
	logger, _ := zap.NewProduction()
	dt := cfg.DropTimeout
	if dt <= 0 {
		dt = 100 * time.Millisecond
	}
	keySource := cfg.SigningKeySource
	if keySource == nil {
		keySource = staticKeySource{key: cfg.SigningKey}
	}
	e := &Emitter{cfg: cfg, logger: logger, lastHash: make(map[string]string), dropTimeout: dt, keySource: keySource}

	// Opt-in: only enable the MinIO sink when fully configured. Otherwise stay
	// logging-only (same posture as OIDC/OpenFGA/NATS).
	if cfg.MinioEndpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" || cfg.SecretKey == "" {
		logger.Info("Audit emitter: MinIO sink disabled (logging only) — set audit.minio_endpoint/bucket/access_key/secret_key to enable",
			zap.String("endpoint", cfg.MinioEndpoint), zap.String("bucket", cfg.Bucket))
		return e, nil
	}

	client, err := minio.New(cfg.MinioEndpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("audit minio client: %w", err)
	}
	if err := ensureBucket(ctx, client, cfg, logger); err != nil {
		return nil, fmt.Errorf("audit bucket: %w", err)
	}

	e.client = client
	e.startSink(ctx, &minioStore{client: client, bucket: cfg.Bucket}, 1024)

	currentKey, currentVersion := keySource.Current()
	logger.Info("Audit emitter initialized (MinIO sink active)",
		zap.String("endpoint", cfg.MinioEndpoint),
		zap.String("bucket", cfg.Bucket),
		zap.Bool("signed", currentKey != ""),
		zap.Int32("signing_key_version", currentVersion),
		zap.Int("retention_days", cfg.RetentionDays),
	)
	return e, nil
}

// Durable reports whether events are persisted to the object store (false in
// logging-only mode).
func (e *Emitter) Durable() bool { return e.queue != nil }

// startSink attaches the object store, resumes each tenant's hash chain from
// its newest persisted event (so a restart continues the chain instead of
// starting a fresh one) and starts the upload worker.
func (e *Emitter) startSink(ctx context.Context, store chainStore, queueSize int) {
	e.store = store
	e.loadChainHeads(ctx, e.logger)
	e.queue = make(chan uploadJob, queueSize)
	e.workerCtx, e.cancel = context.WithCancel(context.Background())
	e.wg.Add(1)
	go e.worker()
}

// ensureBucket creates the audit bucket with object locking + the default
// retention when it doesn't exist. An existing bucket is left as-is (warn if
// object lock isn't on — it can't be enabled retroactively).
func ensureBucket(ctx context.Context, client *minio.Client, cfg Config, logger *zap.Logger) error {
	exists, err := client.BucketExists(ctx, cfg.Bucket)
	if err != nil {
		return fmt.Errorf("bucket exists check: %w", err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.Bucket, minio.MakeBucketOptions{ObjectLocking: true}); err != nil {
			return fmt.Errorf("make bucket: %w", err)
		}
		logger.Info("audit: created bucket with object lock", zap.String("bucket", cfg.Bucket))
	}

	// Apply the default object-lock retention every startup (idempotent), so the
	// WORM policy self-heals and a pre-existing lock-capable bucket gets it too.
	if cfg.RetentionDays > 0 {
		days := uint(cfg.RetentionDays)
		mode := minio.Governance
		unit := minio.Days
		if err := client.SetObjectLockConfig(ctx, cfg.Bucket, &mode, &days, &unit); err != nil {
			logger.Warn("audit: could not set default object-lock retention (bucket may lack object locking — recreate with --with-lock for WORM)",
				zap.String("bucket", cfg.Bucket), zap.Error(err))
		}
	}
	return nil
}

// Emit chains, signs, logs, publishes and persists an audit event. With an
// object store the event is handed to the upload worker (see the package
// comment); a full queue or a failed write is returned as an error so the
// caller can record or refuse the gap, and the chain head never moves past an
// event that was not persisted. The caller's event is chained in place only in
// logging-only mode; with a store the worker chains its own copy.
func (e *Emitter) Emit(ctx context.Context, event *auditv1.AuditEvent) error {
	if event == nil {
		return nil
	}
	if event.EventId == "" || event.TenantId == "" || event.EventType == "" {
		return fmt.Errorf("audit event missing required fields: event_id=%q tenant_id=%q event_type=%q",
			event.EventId, event.TenantId, event.EventType)
	}

	// Enrich actor_email before chaining so the email is covered by the
	// signature. Only when currently empty and actor_user_id is set — never
	// overwrite an email the interceptor already supplied.
	if event.ActorEmail == "" && event.ActorUserId != "" && e.emailResolver != nil {
		event.ActorEmail = e.emailResolver.Email(ctx, event.TenantId, event.ActorUserId)
	}

	if e.queue == nil {
		// Logging-only: nothing to persist, so the head advances at once.
		e.mu.Lock()
		hash := e.chainLocked(event)
		e.lastHash[keySegment(event.TenantId)] = hash
		e.mu.Unlock()
		_, err := e.record(ctx, event)
		return err
	}

	job := uploadJob{event: proto.Clone(event).(*auditv1.AuditEvent), done: make(chan error, 1)}
	if !e.enqueue(ctx, job) {
		return fmt.Errorf("audit: upload queue full, event %s not persisted", event.EventId)
	}
	if event.EventType == RPCEventType && event.Decision != auditv1.PolicyDecision_DENY {
		return nil
	}
	select {
	case err := <-job.done:
		return err
	case <-ctx.Done():
		return fmt.Errorf("audit: event %s not confirmed persisted: %w", event.EventId, ctx.Err())
	case <-e.workerCtx.Done():
		select {
		case err := <-job.done:
			return err
		default:
			return fmt.Errorf("audit: emitter closed, event %s not persisted", event.EventId)
		}
	}
}

// chainLocked links event to its tenant's chain head and signs it, returning
// its chain hash. The caller holds e.mu and decides when the head advances.
// The hash covers prior_event_hash, so any tamper breaks every downstream link.
//
// canonicalBytes (verify.go) is the single canonical form used by both the
// emitter and the verifier — keeping them in sync is the entire point.
func (e *Emitter) chainLocked(event *auditv1.AuditEvent) string {
	event.PriorEventHash = e.lastHash[keySegment(event.TenantId)]
	// SigningKeyVersion must be set before canonicalBytes so it's covered by
	// both the chain hash and the signature — recording it after signing would
	// let the version field itself go untamper-evident.
	key, version := e.keySource.Current()
	event.SigningKeyVersion = version
	canonical := canonicalBytes(event)
	sum := sha256.Sum256(canonical)
	if key != "" {
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write(canonical)
		event.Signature = hex.EncodeToString(mac.Sum(nil))
	}
	return hex.EncodeToString(sum[:])
}

// record serializes a chained event, logs it and publishes it to the live
// observability stream, returning the stored form.
func (e *Emitter) record(ctx context.Context, event *auditv1.AuditEvent) ([]byte, error) {
	// Plain encoding/json is the canonical wire+storage serialization for
	// AuditEvent (snake_case field names, numeric enums, {seconds,nanos}
	// timestamps) — pinned by TestGoldenJSON_CanonicalShape. protojson is
	// accepted defensively on read only (unmarshalAuditEvent); never write it.
	data, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("audit marshal: %w", err)
	}

	e.logger.Info("AUDIT",
		zap.String("event_id", event.EventId),
		zap.String("event_type", event.EventType),
		zap.String("tenant_id", event.TenantId),
		zap.String("actor_user_id", event.ActorUserId),
		zap.String("resource_ref", event.ResourceRef),
		zap.String("raw", string(data)),
	)

	// Live observability stream. Best-effort — never fail the audited operation.
	if e.bus != nil && e.bus.Enabled() {
		if perr := e.bus.Publish(ctx, notify.AuditSubject(event.TenantId), data); perr != nil {
			e.logger.Debug("audit NATS publish failed", zap.Error(perr))
		}
	}
	return data, nil
}

// persist is the worker's half of Emit: chain, sign, record and write one
// event, advancing the tenant's chain head only once the write has landed.
// The single worker makes chain order the write order.
func (e *Emitter) persist(event *auditv1.AuditEvent) error {
	e.mu.Lock()
	hash := e.chainLocked(event)
	e.mu.Unlock()
	data, err := e.record(e.workerCtx, event)
	if err != nil {
		return err
	}
	if err := e.put(objectKey(event), data, hash); err != nil {
		return err
	}
	e.mu.Lock()
	e.lastHash[keySegment(event.TenantId)] = hash
	e.mu.Unlock()
	return nil
}

// objectKey lays events out as <tenant>/<YYYY>/<MM>/<DD>/<event_id>.json.
func objectKey(event *auditv1.AuditEvent) string {
	ts := time.Now().UTC()
	if event.OccurredAt != nil {
		ts = event.OccurredAt.AsTime().UTC()
	}
	return fmt.Sprintf("%s/%04d/%02d/%02d/%s.json",
		keySegment(event.TenantId), ts.Year(), int(ts.Month()), ts.Day(), keySegment(event.EventId))
}

// keySegment makes s safe as one segment of an object key and as a chain-head
// key: '/' cannot reach into another tenant's prefix, a leading '_' cannot
// reach the reserved _policy/ space and a leading '.' cannot form "." or "..".
// The escaping is injective ('%' itself is escaped) and leaves UUIDs as they
// are.
func keySegment(s string) string {
	s = url.PathEscape(s)
	if s != "" && (s[0] == '_' || s[0] == '.') {
		s = fmt.Sprintf("%%%02X", s[0]) + s[1:]
	}
	return s
}

// loadChainHeads seeds lastHash for every tenant with existing history, reading
// the chain hash of each tenant's chain tail (see resumeTail). Best-effort: a
// failure just means that tenant's chain restarts (logged), never a hard error.
func (e *Emitter) loadChainHeads(ctx context.Context, logger *zap.Logger) {
	tenants, _, err := e.store.listLevel(ctx, "")
	if err != nil {
		logger.Warn("audit: list tenants for chain resume failed", zap.Error(err))
		return
	}
	for _, prefix := range tenants {
		if strings.HasPrefix(prefix, "_") {
			continue // "_policy/" etc. are not tenants
		}
		tenant := strings.TrimSuffix(prefix, "/")
		key, ok := newestUnder(ctx, e.store, prefix)
		if !ok {
			continue
		}
		h, ok := resumeTail(ctx, e.store, key)
		if !ok {
			// Fall back to the newest object by LastModified (pre-tail-pick
			// behaviour). May fork the chain if uploads landed out of order.
			logger.Warn("audit: could not identify a unique chain tail among recent events; resuming from newest by LastModified",
				zap.String("tenant", tenant), zap.String("key", key))
			if h, err = e.store.chainHash(ctx, key); err != nil {
				logger.Warn("audit: stat newest event for chain resume failed", zap.String("tenant", tenant), zap.Error(err))
				continue
			}
		}
		if h != "" {
			e.lastHash[tenant] = h
			logger.Info("audit: resumed hash chain", zap.String("tenant", tenant), zap.String("head", h[:min(16, len(h))]))
		}
	}
}

// Chain-resume candidate bounds. The worker writes in chain order, but a trail
// written by an older broker (which chained before enqueueing) can have its
// tail trail the newest object by LastModified, by roughly how long a racing
// Emit waited to enqueue (≤ DropTimeout, default 100ms, plus scheduling). A
// minute is orders of magnitude above that; the count cap keeps startup to a
// bounded number of GETs per tenant even after a heavy burst.
// ponytail: if a single restart ever sees >resumeMaxCandidates events inside
// the window AND a misorder deeper than that, the pick can be wrong; raise the
// cap if that ever shows up.
const (
	resumeWindow        = time.Minute
	resumeMaxCandidates = 256
)

// resumeTail returns the chain hash of the tenant's true chain tail. newestKey
// is newestUnder's pick (<tenant>/YYYY/MM/DD/<id>.json). Candidates are the
// recent objects of that date directory and the previous day's (occurred_at is
// stamped before the chain lock, so a burst around midnight can put the tail in
// the earlier directory). The tail is the one candidate whose hash no other
// candidate names as prior_event_hash. ok=false when that is not exactly one
// candidate, or on any read/parse failure — the caller then falls back.
func resumeTail(ctx context.Context, store objectStore, newestKey string) (string, bool) {
	dir := path.Dir(newestKey) // <tenant>/YYYY/MM/DD
	dirs := []string{dir + "/"}
	if parts := strings.Split(dir, "/"); len(parts) == 4 {
		if d, err := time.Parse("2006/01/02", strings.Join(parts[1:], "/")); err == nil {
			dirs = append(dirs, parts[0]+"/"+d.AddDate(0, 0, -1).Format("2006/01/02")+"/")
		}
	}
	var metas []objMeta
	for _, d := range dirs {
		m, err := store.list(ctx, d)
		if err != nil {
			return "", false
		}
		metas = append(metas, m...)
	}
	if len(metas) == 0 {
		return "", false
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].lastModified.After(metas[j].lastModified) })
	var cands []*auditv1.AuditEvent
	for i, m := range metas {
		if i == resumeMaxCandidates || metas[0].lastModified.Sub(m.lastModified) > resumeWindow {
			break
		}
		data, err := store.get(ctx, m.key)
		if err != nil {
			return "", false
		}
		ev, err := unmarshalAuditEvent(data)
		if err != nil {
			return "", false
		}
		cands = append(cands, ev)
	}
	return chainTail(cands)
}

// chainTail returns the hash of the only event not referenced as another's
// prior_event_hash. The hash is computed exactly as the verifier does
// (canonicalBytes covers signing_key_version and prior_event_hash, not the
// signature), so it equals the chain-hash metadata written by put.
func chainTail(events []*auditv1.AuditEvent) (string, bool) {
	hashes := make([]string, len(events))
	referenced := make(map[string]bool, len(events))
	for i, ev := range events {
		hashes[i] = eventHash(canonicalBytes(ev))
		referenced[ev.PriorEventHash] = true
	}
	tail := ""
	for _, h := range hashes {
		if !referenced[h] {
			if tail != "" && tail != h {
				return "", false // two open ends, e.g. an already-forked history
			}
			tail = h
		}
	}
	return tail, tail != ""
}

// newestUnder finds the most recently written object under prefix. It descends
// the date "directories" (<tenant>/YYYY/MM/DD/) by lexicographically greatest —
// dates sort by time — then at the leaf picks the object with the latest
// LastModified. (event_id is a time-sortable UUIDv7, so lexical order would also
// work; LastModified keeps this robust regardless of the id scheme.) A handful
// of bounded list calls regardless of trail size.
func newestUnder(ctx context.Context, store chainStore, prefix string) (string, bool) {
	cur := prefix
	for depth := 0; depth < 8; depth++ {
		dirs, objs, err := store.listLevel(ctx, cur)
		if err != nil {
			return "", false
		}
		var maxDir string
		for _, d := range dirs {
			if d > maxDir {
				maxDir = d
			}
		}
		if maxDir != "" {
			cur = maxDir // descend into the latest date directory
			continue
		}
		var newest objMeta
		for _, o := range objs {
			if newest.key == "" || o.lastModified.After(newest.lastModified) {
				newest = o
			}
		}
		return newest.key, newest.key != ""
	}
	return "", false
}

// chainStore is the object-store view chain resume needs (*minioStore in
// production, a fake in tests).
type chainStore interface {
	objectStore
	// listLevel lists one level under prefix: sub-"directories" (keys ending
	// in "/") and objects.
	listLevel(ctx context.Context, prefix string) (dirs []string, objs []objMeta, err error)
	// chainHash reads the chain-hash metadata put wrote on key.
	chainHash(ctx context.Context, key string) (string, error)
	// put writes data under key with hash as its chain-hash metadata.
	put(ctx context.Context, key string, data []byte, hash string) error
}

func (m *minioStore) listLevel(ctx context.Context, prefix string) ([]string, []objMeta, error) {
	var dirs []string
	var objs []objMeta
	for obj := range m.client.ListObjects(ctx, m.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: false}) {
		if obj.Err != nil {
			return nil, nil, obj.Err
		}
		if strings.HasSuffix(obj.Key, "/") {
			dirs = append(dirs, obj.Key)
		} else {
			objs = append(objs, objMeta{key: obj.Key, lastModified: obj.LastModified})
		}
	}
	return dirs, objs, nil
}

func (m *minioStore) put(ctx context.Context, key string, data []byte, hash string) error {
	_, err := m.client.PutObject(ctx, m.bucket, key, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{
			ContentType:  "application/json",
			UserMetadata: map[string]string{chainHashMeta: hash},
		})
	return err
}

func (m *minioStore) chainHash(ctx context.Context, key string) (string, error) {
	info, err := m.client.StatObject(ctx, m.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		return "", err
	}
	return info.Metadata.Get("X-Amz-Meta-" + chainHashMeta), nil
}

// enqueue tries to hand the job to the worker, blocking up to dropTimeout
// before dropping (queue-full backpressure). Returns true if queued.
func (e *Emitter) enqueue(ctx context.Context, job uploadJob) bool {
	// NewTimer + Stop (not time.After) so the happy path — queue has space —
	// reclaims the timer immediately instead of leaking it for dropTimeout.
	t := time.NewTimer(e.dropTimeout)
	defer t.Stop()
	select {
	case e.queue <- job:
		return true
	case <-t.C:
		e.drop(ctx, job.event)
		return false
	}
}

// drop reports an event that will never be persisted. Its unchained copy still
// goes to the AUDIT log and the live stream, so a stalled store cannot hide it
// (a denial, say) from logs and alerting as well.
func (e *Emitter) drop(ctx context.Context, event *auditv1.AuditEvent) {
	RecordEventDropped(ctx, e.logger, event.EventType, event.TenantId, event.EventId)
	_, _ = e.record(ctx, event)
}

func (e *Emitter) worker() {
	defer e.wg.Done()
	for {
		select {
		case <-e.workerCtx.Done():
			return
		case job := <-e.queue:
			job.done <- e.persist(job.event)
		}
	}
}

// put writes one event, retrying twice. It returns the last error once every
// attempt failed or the emitter is closing, unless the object is in the store
// with this chain hash after all: a write can land while the client sees a
// timeout, and treating it as lost would fork the chain at the next event.
func (e *Emitter) put(key string, data []byte, hash string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(e.workerCtx, 10*time.Second)
		err := e.store.put(ctx, key, data, hash)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if e.workerCtx.Err() != nil {
			break
		}
		select {
		case <-e.workerCtx.Done():
		case <-time.After(time.Duration(attempt+1) * putBackoff):
		}
	}
	ctx, cancel := context.WithTimeout(e.workerCtx, 10*time.Second)
	got, err := e.store.chainHash(ctx, key)
	cancel()
	if err == nil && got == hash {
		return nil
	}
	e.logger.Error("audit: failed to persist event to MinIO after retries",
		zap.String("key", key), zap.Error(lastErr))
	return fmt.Errorf("audit: persist %s: %w", key, lastErr)
}

// Close drains the upload queue (best-effort, bounded) and stops the worker.
// Events still queued at the deadline are reported dropped and their waiting
// Emit calls get an error; none of them was chained, so the chain has no hole.
func (e *Emitter) Close() {
	if e.queue == nil {
		return
	}
	deadline := time.After(5 * time.Second)
drain:
	for len(e.queue) > 0 {
		select {
		case <-deadline:
			break drain
		case <-time.After(50 * time.Millisecond):
		}
	}
	e.cancel()
	e.wg.Wait()
	for {
		select {
		case job := <-e.queue:
			e.drop(context.Background(), job.event)
			job.done <- fmt.Errorf("audit: emitter closed, event %s not persisted", job.event.EventId)
		default:
			return
		}
	}
}

// EmitBatch emits multiple events efficiently.
func (e *Emitter) EmitBatch(ctx context.Context, events []*auditv1.AuditEvent) error {
	var firstErr error
	for _, ev := range events {
		if err := e.Emit(ctx, ev); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
