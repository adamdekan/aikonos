// broker/internal/audit/emitter.go
// Audit emitter — hash-chained, signed AuditEvents written to MinIO (WORM via
// object lock). Falls back to logging-only when MinIO isn't configured, so the
// broker still runs (and still publishes to the live observability stream)
// without object storage. The MinIO write is asynchronous: Emit stays
// fire-and-forget so it never adds latency to the audited RPC.
package audit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"

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

type uploadJob struct {
	key  string
	data []byte
	hash string
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

	keySource SigningKeySource // never nil after NewEmitter

	emailResolver EmailResolver // optional: enriches actor_email before chain/sign

	dropTimeout time.Duration

	mu       sync.Mutex
	lastHash map[string]string // tenant → last event hash (chain head, per broker lifetime)

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
	// Resume each tenant's hash chain from the newest persisted event so a
	// restart continues the chain instead of starting a fresh one.
	e.loadChainHeads(ctx, logger)
	e.queue = make(chan uploadJob, 1024)
	e.workerCtx, e.cancel = context.WithCancel(context.Background())
	e.wg.Add(1)
	go e.worker()

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

// Emit chains, signs, logs, publishes, and (async) persists an audit event.
// Fire-and-forget — never blocks on the MinIO write.
func (e *Emitter) Emit(ctx context.Context, event *auditv1.AuditEvent) error {
	if event == nil {
		return nil
	}
	if event.EventId == "" || event.TenantId == "" || event.EventType == "" {
		return fmt.Errorf("audit event missing required fields: event_id=%q tenant_id=%q event_type=%q",
			event.EventId, event.TenantId, event.EventType)
	}

	// Enrich actor_email before the chain/sign critical section so the email is
	// covered by the signature. Only when currently empty and actor_user_id is set
	// — never overwrite an email the interceptor already supplied.
	if event.ActorEmail == "" && event.ActorUserId != "" && e.emailResolver != nil {
		event.ActorEmail = e.emailResolver.Email(ctx, event.TenantId, event.ActorUserId)
	}

	// Chain + sign under a lock so the per-tenant hash chain is deterministic
	// regardless of concurrent callers. The hash covers prior_event_hash, so any
	// tamper breaks every downstream link.
	//
	// canonicalBytes (verify.go) is the single canonical form used by both the
	// emitter and the verifier — keeping them in sync is the entire point.
	e.mu.Lock()
	event.PriorEventHash = e.lastHash[event.TenantId]
	// SigningKeyVersion must be set before canonicalBytes so it's covered by
	// both the chain hash and the signature — recording it after signing would
	// let the version field itself go untamper-evident.
	key, version := e.keySource.Current()
	event.SigningKeyVersion = version
	canonical := canonicalBytes(event)
	sum := sha256.Sum256(canonical)
	thisHash := hex.EncodeToString(sum[:])
	e.lastHash[event.TenantId] = thisHash
	if key != "" {
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write(canonical)
		event.Signature = hex.EncodeToString(mac.Sum(nil))
	}
	e.mu.Unlock()

	// Plain encoding/json is the canonical wire+storage serialization for
	// AuditEvent (snake_case field names, numeric enums, {seconds,nanos}
	// timestamps) — pinned by TestGoldenJSON_CanonicalShape. protojson is
	// accepted defensively on read only (unmarshalAuditEvent); never write it.
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("audit marshal: %w", err)
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

	// Durable WORM write — handed to the async worker. Blocks up to dropTimeout
	// on a full queue (bounded backpressure) before dropping.
	if e.client != nil {
		e.enqueue(ctx, uploadJob{key: objectKey(event), data: data, hash: thisHash},
			event.EventType, event.TenantId, event.EventId)
	}
	return nil
}

// objectKey lays events out as <tenant>/<YYYY>/<MM>/<DD>/<event_id>.json.
func objectKey(event *auditv1.AuditEvent) string {
	ts := time.Now().UTC()
	if event.OccurredAt != nil {
		ts = event.OccurredAt.AsTime().UTC()
	}
	return fmt.Sprintf("%s/%04d/%02d/%02d/%s.json",
		event.TenantId, ts.Year(), int(ts.Month()), ts.Day(), event.EventId)
}

// loadChainHeads seeds lastHash for every tenant with existing history, reading
// the chain-hash metadata of each tenant's newest stored event. Best-effort: a
// failure just means that tenant's chain restarts (logged), never a hard error.
func (e *Emitter) loadChainHeads(ctx context.Context, logger *zap.Logger) {
	for obj := range e.client.ListObjects(ctx, e.cfg.Bucket, minio.ListObjectsOptions{Recursive: false}) {
		if obj.Err != nil {
			logger.Warn("audit: list tenants for chain resume failed", zap.Error(obj.Err))
			return
		}
		if !strings.HasSuffix(obj.Key, "/") {
			continue // tenant prefixes are "directories"
		}
		tenant := strings.TrimSuffix(obj.Key, "/")
		key, ok := e.newestUnder(ctx, obj.Key)
		if !ok {
			continue
		}
		info, err := e.client.StatObject(ctx, e.cfg.Bucket, key, minio.StatObjectOptions{})
		if err != nil {
			logger.Warn("audit: stat newest event for chain resume failed", zap.String("tenant", tenant), zap.Error(err))
			continue
		}
		h := info.Metadata.Get("X-Amz-Meta-" + chainHashMeta)
		if h != "" {
			e.lastHash[tenant] = h
			logger.Info("audit: resumed hash chain", zap.String("tenant", tenant), zap.String("head", h[:16]))
		}
	}
}

// newestUnder finds the most recently written object under prefix. It descends
// the date "directories" (<tenant>/YYYY/MM/DD/) by lexicographically greatest —
// dates sort by time — then at the leaf picks the object with the latest
// LastModified. (event_id is a time-sortable UUIDv7, so lexical order would also
// work; LastModified keeps this robust regardless of the id scheme.) A handful
// of bounded list calls regardless of trail size.
func (e *Emitter) newestUnder(ctx context.Context, prefix string) (string, bool) {
	cur := prefix
	for depth := 0; depth < 8; depth++ {
		var maxDir, newestObj string
		var newestT time.Time
		for obj := range e.client.ListObjects(ctx, e.cfg.Bucket, minio.ListObjectsOptions{Prefix: cur, Recursive: false}) {
			if obj.Err != nil {
				return "", false
			}
			if strings.HasSuffix(obj.Key, "/") {
				if obj.Key > maxDir {
					maxDir = obj.Key
				}
			} else if newestObj == "" || obj.LastModified.After(newestT) {
				newestObj, newestT = obj.Key, obj.LastModified
			}
		}
		if maxDir != "" {
			cur = maxDir // descend into the latest date directory
			continue
		}
		if newestObj != "" {
			return newestObj, true
		}
		return "", false
	}
	return "", false
}

// enqueue tries to hand the job to the worker, blocking up to dropTimeout
// before dropping (queue-full backpressure). Returns true if queued.
func (e *Emitter) enqueue(ctx context.Context, job uploadJob, eventType, tenant, eventID string) bool {
	// NewTimer + Stop (not time.After) so the happy path — queue has space —
	// reclaims the timer immediately instead of leaking it for dropTimeout.
	t := time.NewTimer(e.dropTimeout)
	defer t.Stop()
	select {
	case e.queue <- job:
		return true
	case <-t.C:
		RecordEventDropped(ctx, e.logger, eventType, tenant, eventID)
		return false
	}
}

func (e *Emitter) worker() {
	defer e.wg.Done()
	for {
		select {
		case <-e.workerCtx.Done():
			return
		case job := <-e.queue:
			e.put(job)
		}
	}
}

func (e *Emitter) put(job uploadJob) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(e.workerCtx, 10*time.Second)
		_, err := e.client.PutObject(ctx, e.cfg.Bucket, job.key,
			bytes.NewReader(job.data), int64(len(job.data)),
			minio.PutObjectOptions{
				ContentType:  "application/json",
				UserMetadata: map[string]string{chainHashMeta: job.hash},
			})
		cancel()
		if err == nil {
			return
		}
		lastErr = err
		select {
		case <-e.workerCtx.Done():
			return
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
	e.logger.Error("audit: failed to persist event to MinIO after retries",
		zap.String("key", job.key), zap.Error(lastErr))
}

// Close drains the upload queue (best-effort, bounded) and stops the worker.
func (e *Emitter) Close() {
	if e.client == nil {
		return
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case <-deadline:
			e.cancel()
			e.wg.Wait()
			return
		default:
			if len(e.queue) == 0 {
				e.cancel()
				e.wg.Wait()
				return
			}
			time.Sleep(50 * time.Millisecond)
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
