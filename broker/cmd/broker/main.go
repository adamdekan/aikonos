// broker/cmd/broker/main.go
// Aikonos Broker — main entry point.
// Wires: SPIFFE workload API, OTel tracing, gRPC servers (north + south bound),
// policy engine clients, audit emitter, sandbox manager.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"

	// SPIFFE — workload identity
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffegrpc/grpccredentials"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/svid/x509svid"
	"github.com/spiffe/go-spiffe/v2/workloadapi"

	// OTel
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.21.0"

	"github.com/spf13/viper"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	// Internal packages
	"github.com/adamdekan/aikonos/broker/internal/alerting"
	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/auth"
	"github.com/adamdekan/aikonos/broker/internal/baseline"
	brokersvc "github.com/adamdekan/aikonos/broker/internal/broker"
	"github.com/adamdekan/aikonos/broker/internal/capability"
	"github.com/adamdekan/aikonos/broker/internal/config"
	"github.com/adamdekan/aikonos/broker/internal/connector"
	"github.com/adamdekan/aikonos/broker/internal/connectorstore"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/identity"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/llmseed"
	"github.com/adamdekan/aikonos/broker/internal/metrics"
	"github.com/adamdekan/aikonos/broker/internal/netacl"
	"github.com/adamdekan/aikonos/broker/internal/notify"
	"github.com/adamdekan/aikonos/broker/internal/onedrivefs"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/provisioning"
	"github.com/adamdekan/aikonos/broker/internal/provisioningseed"
	"github.com/adamdekan/aikonos/broker/internal/ratelimit"
	"github.com/adamdekan/aikonos/broker/internal/secrets"
	"github.com/adamdekan/aikonos/broker/internal/toolproxy"
	"github.com/adamdekan/aikonos/broker/internal/toolregistry"
	"github.com/adamdekan/aikonos/broker/internal/workspacefs"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
	brokerv1 "github.com/adamdekan/aikonos/gen/go/broker/v1"
	"go.opentelemetry.io/otel/metric"
)

// netRuleSource adapts the DB repo to netacl.RuleSource, mapping db rule rows to
// the evaluator's rule type (the action/scope string constants are identical).
type netRuleSource struct{ repo *db.NetworkRuleRepo }

// policyTupleWriter adapts *policy.Engine to provisioning.TupleWriter.
// The provisioner calls WriteTuple one tuple at a time; WriteRelations
// accepts variadic tuples so we wrap a single-tuple call.
type policyTupleWriter struct{ eng *policy.Engine }

func (a policyTupleWriter) WriteTuple(ctx context.Context, user, relation, object string) error {
	return a.eng.WriteRelations(ctx, policy.Relation{User: user, Relation: relation, Object: object})
}

func (s netRuleSource) NetworkRules(ctx context.Context, tenant string) ([]netacl.Rule, error) {
	rows, err := s.repo.List(ctx, tenant)
	if err != nil {
		return nil, err
	}
	out := make([]netacl.Rule, 0, len(rows))
	for _, r := range rows {
		out = append(out, netacl.Rule{
			ScopeKind:   netacl.ScopeKind(r.ScopeKind),
			ScopeValue:  r.ScopeValue,
			Action:      netacl.Action(r.Action),
			HostPattern: r.HostPattern,
		})
	}
	return out, nil
}

func main() {
	// ── Config ──────────────────────────────────────────────────────────────
	viper.SetConfigName("broker")
	viper.SetConfigType("yaml")
	viper.AddConfigPath("/etc/aikonos")
	viper.AddConfigPath(".")
	viper.AutomaticEnv()
	viper.SetEnvPrefix("AIKONOS")
	// Map nested config keys to env vars: db.host → AIKONOS_DB_HOST.
	// Without this, AutomaticEnv looks for AIKONOS_DB.HOST (never set) and every
	// nested-key override (db.*, spiffe.*, policy.*, …) is silently ignored.
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	// Defaults
	viper.SetDefault("broker.grpc_port_north", 9090) // frontend → broker
	viper.SetDefault("broker.grpc_port_south", 9091) // sandbox → broker
	viper.SetDefault("broker.tenant_id", "aikonos-dev")
	// The UUID tenant the gateway queries by (AIKONOS_TENANT_ID); DB tenant_id
	// columns are UUIDs, so the LLM-provider seed must use this, not the
	// broker.tenant_id label above.
	viper.SetDefault("tenant_id", "11111111-1111-1111-1111-111111111111")
	viper.SetDefault("scheduler.gateway_spiffe_id", "spiffe://aikonos.com/agent-gateway")
	// Frequency floor for new/edited cron schedules — each fire is an unattended,
	// billable agent run, and `workflow_schedule` is agent-invokable. 0 disables.
	viper.SetDefault("scheduler.min_interval", "5m")
	// Seeds the admin console's user directory with principals who haven't logged
	// in yet. Superseded by user_directory (auto-populated on first OIDC login) +
	// FGA tuples, so the default is empty — real users surface from those sources.
	// A fresh Keycloak dev setup can restore the seed via AIKONOS_ADMIN_KNOWN_USERS.
	viper.SetDefault("admin.known_users", []string{})
	viper.SetDefault("spiffe.socket", "unix:///run/spire/agent/api.sock")
	// File-based TLS fallback (compose / dev without SPIRE). Empty = socket path.
	viper.SetDefault("broker.tls.cert_file", "")
	viper.SetDefault("broker.tls.key_file", "")
	viper.SetDefault("broker.tls.ca_file", "")
	viper.SetDefault("otel.endpoint", "") // empty → tracing disabled (opt-in; set to an OTLP collector to enable)
	viper.SetDefault("policy.openfga_endpoint", "http://openfga.aikonos-system.svc.cluster.local:8080")
	viper.SetDefault("policy.openfga_store_id", "") // empty → CheckFGA dev stub (allow-all)
	viper.SetDefault("policy.openfga_model_id", "")
	viper.SetDefault("policy.opa_endpoint", "http://localhost:8181") // OPA sidecar
	// OIDC north-bound auth. Empty issuer → validation disabled (local dev).
	viper.SetDefault("oidc.issuer", "")
	viper.SetDefault("oidc.audience", "aikonos-broker")
	viper.SetDefault("oidc.jwks_url", "")         // derived via discovery when empty
	viper.SetDefault("oidc.subject_claim", "sub") // "oid" for Entra
	viper.SetDefault("oidc.tenant_claim", "tenant_id")
	// South-bound: required SPIFFE path prefix for sandbox callers ("" = any in-domain workload).
	viper.SetDefault("spiffe.sandbox_path_prefix", "")
	// Capability tokens (Biscuit). Base64 ed25519 private key for the root that
	// signs capability tokens. Empty → the key is read-or-created in Vault (when
	// vault.addr is set), so it survives restarts and is shared across replicas;
	// with no Vault either, an ephemeral per-pod key is generated (dev only).
	viper.SetDefault("capability.root_key", "")
	// Vault (connector OAuth tokens + capability root key). Empty addr → Vault
	// disabled: connector tools are not registered and connector RPCs fail closed.
	// auth_method selects how the broker authenticates: "kubernetes" (SA-JWT, in
	// a pod) or "approle" (role_id/secret_id, the compose deployment). The approle
	// is bound to a least-privilege policy scoped to the broker's KV paths — the
	// broker never holds the Vault root token.
	viper.SetDefault("vault.addr", "")
	viper.SetDefault("vault.auth_method", "kubernetes")
	viper.SetDefault("vault.role", "aikonos-broker")
	viper.SetDefault("vault.role_id", "")
	viper.SetDefault("vault.secret_id", "")
	viper.SetDefault("vault.kv_mount", "secret")
	// Connector OAuth apps (platform secrets). Empty client_id → that provider
	// is unconfigured: its connector RPCs/tools are unavailable.
	viper.SetDefault("connectors.redirect_uri", "")
	viper.SetDefault("connectors.google.client_id", "")
	viper.SetDefault("connectors.google.client_secret", "")
	viper.SetDefault("connectors.microsoft.client_id", "")
	viper.SetDefault("connectors.microsoft.client_secret", "")
	viper.SetDefault("connectors.microsoft.tenant", "common")
	// NATS event routing. Empty → event bus disabled (StreamTaskEvents reports
	// Unimplemented; envelope/task events are dropped). Set to the NATS URL to enable.
	viper.SetDefault("nats.url", "")
	viper.SetDefault("audit.minio_endpoint", "minio.aikonos-system.svc.cluster.local:9000")
	viper.SetDefault("audit.bucket", "aikonos-audit")
	// MinIO sink stays off (logging-only) until access/secret keys are set.
	viper.SetDefault("audit.access_key", "")
	viper.SetDefault("audit.secret_key", "")
	viper.SetDefault("audit.use_ssl", false)
	viper.SetDefault("audit.signing_key", "") // HMAC key; empty → events chained but unsigned
	viper.SetDefault("audit.retention_days", 0)
	// CP4.2: HMAC key for .agent/Sessions/* integrity sidecars. env fallback
	// (AIKONOS_WORKSPACE_SESSION_SIGNING_KEY) when Vault is unavailable; empty →
	// sidecar signing disabled (Write/Read/Move degrade to unsigned — same
	// posture as an unsigned audit chain).
	viper.SetDefault("workspace.session_signing_key", "")
	// Skills directory. Manifests are loaded at startup from this path.
	// Empty dir or absent dir → baseline only (no error). Conflict → fatal.
	viper.SetDefault("skills.dir", "skills")
	// Tool Proxy: SSRF guard on by default (block private/loopback fetch targets).
	viper.SetDefault("toolproxy.web_fetch.allow_private", false)
	viper.SetDefault("toolproxy.workspace_root", "")
	// AllowMcpPrivate disables the SSRF guard for the broker-side MCP client so
	// it can reach ClusterIP services (private IPs). False in production; set
	// AIKONOS_TOOLPROXY_MCP_ALLOW_PRIVATE=true on the broker Deployment for the
	// in-cluster e2e test server, then revert after testing.
	viper.SetDefault("toolproxy.mcp.allow_private", false)
	// Office worker: empty URL → the office.*
	// toolproxy plugins report unavailable via Available(cfg) instead of failing.
	// Keys (not toolproxy.*-nested) so AutomaticEnv resolves the documented
	// AIKONOS_OFFICE_* names (check-env-drift.sh, .env.*.example).
	viper.SetDefault("office.worker_url", "")
	viper.SetDefault("office.job_timeout_ms", 120000)
	// Extra tool-call policy gates (restrict-only, registered at startup).
	// Format: comma-separated "name=path" pairs, e.g.
	//   policy.tool_gates: "tenant_quota=aikonos/tenant_quota,audit_filter=aikonos/audit_filter"
	// Each named gate Rego must be deployed in the opa-policies ConfigMap.
	// Empty string (default) → no extra gates (unchanged behaviour).
	viper.SetDefault("policy.tool_gates", "")
	viper.SetDefault("alerting.webhook_url", "")
	viper.SetDefault("alerting.deny_rate_threshold", 25)
	viper.SetDefault("alerting.deny_rate_window_seconds", 60)
	viper.SetDefault("alerting.deny_rate_cooldown_seconds", 300)
	// escalation_pattern rule (disabled by default: count=0)
	viper.SetDefault("alerting.escalation_deny_count", 0)
	viper.SetDefault("alerting.escalation_window_seconds", 300)
	viper.SetDefault("alerting.escalation_cooldown_seconds", 600)
	// unusual_actor_host rule (disabled by default: window=0)
	viper.SetDefault("alerting.unusual_baseline_window_seconds", 0)
	viper.SetDefault("alerting.unusual_cooldown_seconds", 300)
	viper.SetDefault("alerting.unusual_max_baseline_entries", 500)
	// off_hours_destructive rule (disabled by default: cooldown=0)
	viper.SetDefault("alerting.off_hours_destructive_cooldown_seconds", 0)
	// SMTP notifier (disabled by default: host empty)
	viper.SetDefault("alerting.smtp_host", "")
	viper.SetDefault("alerting.smtp_port", "587")
	viper.SetDefault("alerting.smtp_from", "")
	viper.SetDefault("alerting.smtp_to", "")
	viper.SetDefault("alerting.smtp_username", "")
	viper.SetDefault("alerting.smtp_password", "")
	// Dev-mode guard. When false (the default), the broker refuses to start if
	// any of OIDC / OpenFGA / capability-key are unconfigured/ephemeral.
	// Set AIKONOS_DEV_MODE=true to allow degraded startup (local dev only).
	viper.SetDefault("dev_mode", false)
	viper.SetDefault("db.host", "postgresql.aikonos-system.svc.cluster.local")
	viper.SetDefault("db.port", 5432)
	viper.SetDefault("db.name", "aikonos")
	viper.SetDefault("db.user", "aikonos")
	viper.SetDefault("db.password", "")
	// Per-agent API key pepper. Empty → mint/resolve return FailedPrecondition
	// (fail closed). Set AIKONOS_API_KEY_PEPPER in the environment or .env file.
	viper.SetDefault("api_key.pepper", "")
	// Gateway-grant HMAC key. Empty → resolved from Vault (secret/data/broker/
	// gateway-grant, cas=0, shared across replicas); if Vault is unavailable,
	// an ephemeral key is generated per-process (grants are short-lived, so a
	// restart mid-run drops in-flight grants — acceptable in that edge case).
	// Set AIKONOS_GATEWAY_GRANT_KEY (base64 of 32 bytes) for an explicit override.
	viper.SetDefault("gateway_grant.key", "")
	// Gateway-grant TTL — how long a minted grant stays valid. Must cover a full
	// multi-step agent loop. Default 1h is documented as the reuse window.
	viper.SetDefault("gateway_grant.ttl", "1h")
	// LLM provider seed file. Empty → skip seeding. The compose deployment
	// sets AIKONOS_LLM_PROVIDERS_SEED_FILE and bind-mounts the file (CP5).
	viper.SetDefault("llm_providers.seed_file", "")
	// Provisioning rules seed file. Empty → skip seeding (default). Set
	// AIKONOS_PROVISIONING_SEED_FILE to a YAML file with {rules:[{matcher,groups}]}.
	viper.SetDefault("provisioning.seed_file", "")
	// Automated agent behavioral baseline learning
	//. Enabled by default — monitoring
	// only, never blocks a call. window_seconds is the flush+detect tick and the
	// bucket size for agent_behavior_windows; learn_interval_seconds is how often
	// the learner recomputes each agent's envelope; min_sample_windows gates drift
	// emission until a baseline is mature; drift_multiplier sets the rate-drift
	// ceiling (baseline.RpmP95 * multiplier); retention_windows bounds how many
	// windows the learner keeps (and how far back it aggregates).
	viper.SetDefault("baseline.enabled", true)
	viper.SetDefault("baseline.window_seconds", 60)
	viper.SetDefault("baseline.learn_interval_seconds", 3600)
	viper.SetDefault("baseline.min_sample_windows", 30)
	viper.SetDefault("baseline.drift_multiplier", 2.0)
	viper.SetDefault("baseline.retention_windows", 10080)
	// Per-call LLM usage event retention.
	// 400 days covers 13 monthly dashboard buckets; 0 disables pruning. Viper-only
	// (no compose substitution) — same precedent as workflow.reason_max_tokens.
	viper.SetDefault("llm_usage.retention_days", 400)

	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			fmt.Fprintf(os.Stderr, "config error: %v\n", err)
			os.Exit(1)
		}
		// Config file not found is fine — use env vars and defaults
	}

	// ── Logger ───────────────────────────────────────────────────────────────
	logLevel := zapcore.InfoLevel
	if viper.GetBool("debug") {
		logLevel = zapcore.DebugLevel
	}
	logCfg := zap.NewProductionConfig()
	logCfg.Level = zap.NewAtomicLevelAt(logLevel)
	logCfg.OutputPaths = []string{"stdout"}
	log, err := logCfg.Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "logger init error: %v\n", err)
		os.Exit(1)
	}
	defer log.Sync()
	log.Info("Aikonos Broker starting")

	// Load skill manifests. Conflict with a baseline scope → fatal (fail-closed).
	// Absent or empty dir → OK, baseline only.
	skillsDir := viper.GetString("skills.dir")
	toolReg := toolregistry.NewRegistry()
	if err := toolReg.LoadManifests(skillsDir); err != nil {
		log.Fatal("skill manifest load failed — broker startup aborted", zap.String("skills_dir", skillsDir), zap.Error(err))
	} else {
		log.Info("skill manifests loaded", zap.String("skills_dir", skillsDir))
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// ── Credential source selection ──────────────────────────────────────────
	// File mode (compose/dev): when all three TLS file keys are set, load static
	// PEM sources and skip the Workload API dial entirely.
	// Socket mode (in-cluster default): when file keys are absent, connect to the
	// SPIFFE agent socket as before.
	tlsCfg := identity.Config{
		CertFile:     viper.GetString("broker.tls.cert_file"),
		KeyFile:      viper.GetString("broker.tls.key_file"),
		CAFile:       viper.GetString("broker.tls.ca_file"),
		SpiffeSocket: viper.GetString("spiffe.socket"),
	}
	fileSrc, credMode, err := identity.SelectCredSource(tlsCfg)
	if err != nil {
		log.Fatal("Failed to load TLS credential sources from files", zap.Error(err))
	}

	// svidSrc + bundleSrc are the two interfaces MTLSServerCredentials needs.
	var svidSrc x509svid.Source
	var bundleSrc x509bundle.Source

	if credMode == identity.ModeFile {
		log.Info("TLS credential source: file mode", zap.String("cert", tlsCfg.CertFile))
		svidSrc = fileSrc.SVIDSource
		bundleSrc = fileSrc.BundleSource
	} else {
		log.Info("TLS credential source: SPIFFE socket mode", zap.String("socket", viper.GetString("spiffe.socket")))
		x509Source, err := workloadapi.NewX509Source(ctx,
			workloadapi.WithClientOptions(workloadapi.WithAddr(viper.GetString("spiffe.socket"))),
		)
		if err != nil {
			log.Fatal("Failed to create X.509 source from SPIFFE agent", zap.Error(err))
		}
		defer x509Source.Close()
		svidSrc = x509Source
		bundleSrc = x509Source

		svid, err := x509Source.GetX509SVID()
		if err != nil {
			log.Fatal("Failed to fetch SVID", zap.Error(err))
		}
		log.Info("SVID fetched", zap.String("spiffe_id", svid.ID.String()))
	}

	// ── OpenTelemetry ────────────────────────────────────────────────────────
	otelEndpoint := viper.GetString("otel.endpoint")
	if otelEndpoint == "" {
		log.Info("OpenTelemetry tracing disabled (no otel.endpoint)")
	} else {
		log.Info("Initializing OpenTelemetry", zap.String("endpoint", otelEndpoint))
	}
	tp, err := initTracer(ctx, otelEndpoint)
	if err != nil {
		log.Fatal("Failed to init tracer", zap.Error(err))
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		if err := tp.Shutdown(shutCtx); err != nil {
			log.Error("Tracer shutdown error", zap.Error(err))
		}
	}()
	otel.SetTracerProvider(tp)

	mp, err := initMeter(ctx, otelEndpoint)
	if err != nil {
		log.Fatal("Failed to init meter", zap.Error(err))
	}
	defer func() {
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutCancel()
		if err := mp.Shutdown(shutCtx); err != nil {
			log.Error("Meter shutdown error", zap.Error(err))
		}
	}()
	otel.SetMeterProvider(mp)
	usageMetrics := metrics.New()
	taskMetrics := metrics.NewTaskRecorder()
	agentMetrics := metrics.NewAgentRecorder()

	// Rate-limit counter for enforcement telemetry.
	rlHitsCounter, _ := otel.Meter("github.com/adamdekan/aikonos/broker").Int64Counter(
		"rate_limit_hits_total",
		metric.WithDescription("Total rate limit enforcement actions by tenant/agent/provider/limit_type"),
	)

	log.Info("OpenTelemetry metrics initialized")
	if otelEndpoint != "" {
		if err := otelruntime.Start(otelruntime.WithMeterProvider(mp)); err != nil {
			log.Warn("Failed to start runtime metrics", zap.Error(err))
		}
	}
	log.Info("OpenTelemetry initialized")

	// ── Database ─────────────────────────────────────────────────────────────
	dbURL := fmt.Sprintf("postgres://%s:%s@%s:%d/%s",
		viper.GetString("db.user"),
		viper.GetString("db.password"),
		viper.GetString("db.host"),
		viper.GetInt("db.port"),
		viper.GetString("db.name"),
	)
	pool, err := db.NewPool(ctx, dbURL)
	if err != nil {
		log.Fatal("Failed to create DB pool", zap.Error(err))
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatal("DB not reachable", zap.Error(err))
	}
	log.Info("Database connected", zap.String("host", viper.GetString("db.host")))

	// One broker per deployment: hold a session-level advisory lock on a
	// dedicated connection for process lifetime. See docs/00-aikonos-architecture.md §10.
	// Never call singletonConn.Release() — releasing it drops the session-level
	// advisory lock and defeats the singleton guard.
	singletonConn, err := pool.Acquire(ctx)
	if err != nil {
		log.Fatal("Failed to acquire dedicated connection for singleton lock", zap.Error(err))
	}
	if err := acquireSingletonLock(ctx, &pgSingletonLocker{conn: singletonConn}, singletonLockKey); err != nil {
		log.Fatal("Singleton lock acquisition failed", zap.Error(err))
	}
	log.Info("Singleton lock acquired")

	rateLimiter := ratelimit.New(rlHitsCounter)
	defer rateLimiter.Close()
	// Shared by pointer across northDeps/sandbox Deps (CheckRateLimit runs
	// south, cap CRUD mutates north) so a cap change invalidates immediately.
	spendCache := brokersvc.NewSpendRollupCache()

	taskRepo := db.NewTaskRepo(pool, log)
	taskRepo.SetRecorder(taskMetrics)
	scheduledRepo := db.NewScheduledRunRepo(pool, log)
	alertRepo := db.NewAlertRepo(pool, log)
	agentRepo := db.NewAgentRepo(pool, log)
	apiKeyRepo := db.NewAgentApiKeyRepo(pool, log)
	llmProviderRepo := db.NewLlmProviderRepo(pool, log)
	configRepo := db.NewPlatformConfigRepo(pool, log)
	configStore := config.New(configRepo).WithLogger(log)

	// Vault secrets provider for per-user connector tokens, capability/gateway
	// keys, and (CP1.2) the audit signing key. Constructed before the audit
	// emitter so the emitter can be handed a Vault-backed signing key source
	// at startup. Nil when vault.addr is unset — dependents then degrade
	// closed/to-env-fallback. Assigned through an interface-typed var so the
	// nil case is a true nil interface (not a typed nil pointer that would
	// read as non-nil).
	vaultClient, err := secrets.NewVaultClient(secrets.Config{
		Addr:       viper.GetString("vault.addr"),
		AuthMethod: viper.GetString("vault.auth_method"),
		Role:       viper.GetString("vault.role"),
		RoleID:     viper.GetString("vault.role_id"),
		SecretID:   viper.GetString("vault.secret_id"),
		KVMount:    viper.GetString("vault.kv_mount"),
	})
	if err != nil {
		log.Fatal("Failed to init Vault secrets provider", zap.Error(err))
	}

	// ── Internal services (stubs in Phase 0) ─────────────────────────────────
	// Audit signing key: read-or-create in Vault (durable, versioned, shared by
	// every replica); falls back to the AIKONOS_AUDIT_SIGNING_KEY env value
	// (version 0, degraded mode) when Vault is unavailable. See CP1.2.
	auditKeySource := resolveAuditSigningKey(ctx, viper.GetString("audit.signing_key"), vaultClient, log)
	auditEmitter, err := audit.NewEmitter(ctx, audit.Config{
		MinioEndpoint:    viper.GetString("audit.minio_endpoint"),
		Bucket:           viper.GetString("audit.bucket"),
		TenantID:         viper.GetString("broker.tenant_id"),
		AccessKey:        viper.GetString("audit.access_key"),
		SecretKey:        viper.GetString("audit.secret_key"),
		UseSSL:           viper.GetBool("audit.use_ssl"),
		SigningKeySource: auditKeySource,
		RetentionDays:    viper.GetInt("audit.retention_days"),
	})
	if err != nil {
		log.Fatal("Failed to init audit emitter", zap.Error(err))
	}
	defer auditEmitter.Close()

	policyEngine, err := policy.NewEngine(ctx, policy.Config{
		OpenFGAEndpoint: viper.GetString("policy.openfga_endpoint"),
		OPAEndpoint:     viper.GetString("policy.opa_endpoint"),
		OpenFGAStoreID:  viper.GetString("policy.openfga_store_id"),
		OpenFGAModelID:  viper.GetString("policy.openfga_model_id"),
	})
	if err != nil {
		log.Fatal("Failed to init policy engine", zap.Error(err))
	}
	// Register extra tool-call gates from config (restrict-only). Each entry
	// must be present in the opa-policies ConfigMap before the broker starts.
	if rawGates := viper.GetString("policy.tool_gates"); rawGates != "" {
		gates := policy.ParseToolGates(rawGates)
		names := make([]string, 0, len(gates))
		for _, g := range gates {
			policyEngine.RegisterToolGate(g.Name, g.Path)
			names = append(names, g.Name)
		}
		log.Info("extra tool-call policy gates registered", zap.Strings("gates", names))
		rawTokens := 0
		for _, tok := range strings.Split(rawGates, ",") {
			if strings.TrimSpace(tok) != "" {
				rawTokens++
			}
		}
		if skipped := rawTokens - len(gates); skipped > 0 {
			log.Warn("policy.tool_gates: malformed entries skipped (empty name or path)",
				zap.Int("skipped", skipped))
		}
	}

	// OIDC bearer-token validator (north-bound). Disabled when oidc.issuer unset.
	oidcValidator, err := auth.NewValidator(auth.OIDCConfig{
		Issuer:          viper.GetString("oidc.issuer"),
		Audience:        viper.GetString("oidc.audience"),
		JWKSURL:         viper.GetString("oidc.jwks_url"),
		SubjectClaim:    viper.GetString("oidc.subject_claim"),
		TenantClaim:     viper.GetString("oidc.tenant_claim"),
		AuthorizedParty: viper.GetString("oidc.authorized_party"),
	})
	if err != nil {
		log.Fatal("Failed to init OIDC validator", zap.Error(err))
	}
	if oidcValidator.Enabled() {
		log.Info("OIDC validation enabled", zap.String("issuer", viper.GetString("oidc.issuer")))
	} else {
		log.Warn("OIDC validation DISABLED (oidc.issuer unset) — north-bound calls are not authenticated")
	}

	// Capability-token minter (Biscuit). Explicit config key wins; otherwise the
	// key is read-or-created in Vault (durable across restarts, shared by every
	// replica); failing that, an ephemeral dev key.
	capMinter, capDurable, err := newCapabilityMinter(ctx, viper.GetString("capability.root_key"), vaultClient, log)
	if err != nil {
		log.Fatal("Failed to init capability minter", zap.Error(err))
	}
	// Startup security guard: refuse to serve if critical subsystems are in dev
	// fallback state and AIKONOS_DEV_MODE is not explicitly true.
	{
		degraded, guardErr := devModeGuard(
			viper.GetBool("dev_mode"),
			oidcValidator.Enabled(),
			policyEngine.FGAEnabled(),
			capDurable,
		)
		if guardErr != nil {
			log.Fatal("startup guard", zap.Error(guardErr))
		}
		if len(degraded) > 0 {
			log.Warn("running in DEV_MODE with degraded security", zap.Strings("degraded", degraded))
		}
	}
	// F24: delegation scope-attenuation (senderDelegableScopes,
	// broker/internal/broker/delegation_scopes.go) derives real scopes from
	// OpenFGA once FGA is enabled — the hardcoded dev-stub list only remains
	// in the path while FGA is disabled, so the warning fires only then; an
	// unconditional warning with FGA enabled would be a false alarm.
	if !policyEngine.FGAEnabled() {
		log.Warn("delegation scope-attenuation uses a hardcoded dev stub — scopes are NOT derived from real grants (FGA disabled)")
	}

	var secretsProvider secrets.Provider
	if vaultClient != nil {
		secretsProvider = vaultClient
		log.Info("Vault secrets provider enabled", zap.String("vault_addr", viper.GetString("vault.addr")))
	} else {
		log.Warn("Vault secrets provider DISABLED (vault.addr unset) — connector tools unavailable")
	}

	// Gateway-grant HMAC key. Precedence: explicit config > Vault > ephemeral.
	// Mirrors the capability-key bootstrap above.
	gatewayGrantTTL, ttlErr := time.ParseDuration(viper.GetString("gateway_grant.ttl"))
	if ttlErr != nil || gatewayGrantTTL <= 0 {
		log.Warn("gateway_grant.ttl invalid or unset — defaulting to 1h", zap.String("raw", viper.GetString("gateway_grant.ttl")))
		gatewayGrantTTL = time.Hour
	}
	gatewayGrantKey, err := resolveGatewayGrantKey(ctx, viper.GetString("gateway_grant.key"), vaultClient, log)
	if err != nil {
		log.Fatal("Failed to resolve gateway grant key", zap.Error(err))
	}
	// CP4.2: shared by both Store instances below (north + south) so a session
	// signed on one gRPC surface verifies on the other.
	workspaceSessionKey := resolveWorkspaceSessionKey(ctx, viper.GetString("workspace.session_signing_key"), vaultClient, log)
	// Connector OAuth registry + state store + reference store. Wired only when
	// Vault is enabled (a token store with nowhere to put tokens is useless).
	var connRegistry *connector.Registry
	var connState connector.PendingAuthStore
	var connStore *connectorstore.Store
	if secretsProvider != nil {
		connRegistry = &connector.Registry{
			RedirectURI:     viper.GetString("connectors.redirect_uri"),
			Credentials:     buildConnectorCredentials(viper.GetString),
			MicrosoftTenant: viper.GetString("connectors.microsoft.tenant"),
			Logger:          log,
		}
		connState = brokersvc.NewDBPendingAuthStore(db.NewConnectorStateRepo(pool, log), 10*time.Minute)
		connStore = connectorstore.New(viper.GetString("toolproxy.workspace_root"))
		connRegistry.Status = connStore
		log.Info("Connectors enabled",
			zap.Bool("google", connRegistry.Configured(connector.ProviderGoogleDrive)),
			zap.Bool("onedrive", connRegistry.Configured(connector.ProviderOneDrive)))
	}

	// NATS event bus. Disabled (no-op) when nats.url is unset.
	eventBus, err := notify.New(viper.GetString("nats.url"), log)
	if err != nil {
		log.Fatal("Failed to connect event bus", zap.Error(err))
	}
	defer eventBus.Close()
	if eventBus.Enabled() {
		log.Info("Event bus connected", zap.String("nats_url", viper.GetString("nats.url")))
	} else {
		log.Warn("Event bus DISABLED (nats.url unset) — StreamTaskEvents unavailable, events dropped")
	}
	// Publish audit events to NATS too (live observability stream).
	auditEmitter.AttachBus(eventBus)

	// Alerting engine — subscribes to the audit bus and fires alerts to all
	// configured sinks (webhook and/or SMTP). Enabled when at least one sink is
	// configured and the bus is up. Observe-only: never affects any gate or decision.
	{
		var alertSinks []alerting.Notifier
		if webhookURL := viper.GetString("alerting.webhook_url"); webhookURL != "" {
			alertSinks = append(alertSinks, &alerting.WebhookNotifier{
				URL:    webhookURL,
				Client: &http.Client{Timeout: 5 * time.Second},
			})
			log.Info("alerting: webhook sink configured", zap.String("url", webhookURL))
		}
		if smtpHost := viper.GetString("alerting.smtp_host"); smtpHost != "" {
			alertSinks = append(alertSinks, &alerting.SMTPNotifier{
				Host:     smtpHost,
				Port:     viper.GetString("alerting.smtp_port"),
				From:     viper.GetString("alerting.smtp_from"),
				To:       viper.GetString("alerting.smtp_to"),
				Username: viper.GetString("alerting.smtp_username"),
				Password: viper.GetString("alerting.smtp_password"),
				Logger:   log,
			})
			log.Info("alerting: SMTP sink configured", zap.String("host", smtpHost))
		}
		if len(alertSinks) > 0 && eventBus.Enabled() {
			alertEv := alerting.NewEvaluator(alerting.Config{
				DenyRateThreshold:           viper.GetInt("alerting.deny_rate_threshold"),
				DenyRateWindow:              time.Duration(viper.GetInt("alerting.deny_rate_window_seconds")) * time.Second,
				DenyRateCooldown:            time.Duration(viper.GetInt("alerting.deny_rate_cooldown_seconds")) * time.Second,
				EscalationDenyCount:         viper.GetInt("alerting.escalation_deny_count"),
				EscalationWindow:            time.Duration(viper.GetInt("alerting.escalation_window_seconds")) * time.Second,
				EscalationCooldown:          time.Duration(viper.GetInt("alerting.escalation_cooldown_seconds")) * time.Second,
				UnusualBaselineWindow:       time.Duration(viper.GetInt("alerting.unusual_baseline_window_seconds")) * time.Second,
				UnusualCooldown:             time.Duration(viper.GetInt("alerting.unusual_cooldown_seconds")) * time.Second,
				MaxBaselineEntries:          viper.GetInt("alerting.unusual_max_baseline_entries"),
				OffHoursDestructiveCooldown: time.Duration(viper.GetInt("alerting.off_hours_destructive_cooldown_seconds")) * time.Second,
			})
			multi := alerting.NewMultiNotifier(log, alertSinks...)
			alertEngine, err := alerting.NewEngineWithRepo(eventBus, notify.AuditWildcardSubject(), alertEv, multi, alertRepo, log)
			if err != nil {
				log.Warn("alerting engine subscribe failed — alerting degraded, broker continues", zap.Error(err))
			} else {
				alertEngine.Start(ctx)
				defer alertEngine.Close()
				log.Info("alerting enabled", zap.Int("sinks", len(alertSinks)))
			}
		} else {
			log.Info("alerting disabled (no sinks configured or bus disabled)")
		}
	}

	// Network access-list (web egress). The Checker reads tenant rules from
	// Postgres (cached) + resolves the user's groups via OpenFGA; the web.fetch
	// handler and SubmitPlan consult it.
	networkRepo := db.NewNetworkRuleRepo(pool, log)
	netChecker := netacl.NewChecker(
		netRuleSource{repo: networkRepo},
		func(ctx context.Context, _, user string) ([]string, error) {
			return policyEngine.ListUserGroups(ctx, user)
		},
		30*time.Second,
	)

	mcpRepo := db.NewMcpConnectionRepo(pool, log)
	userDirRepo := db.NewUserDirectoryRepo(pool, log)
	provisioningRepo := db.NewProvisioningRepo(pool, log)
	skillOverlayRepo := db.NewSkillOverlayRepo(pool, log)
	agentSkillRepo := db.NewAgentSkillRepo(pool, log)
	workflowRepo := db.NewWorkflowRepo(pool, log)
	rateLimitRepo := db.NewRateLimitPolicyRepo(pool, log)
	spendCapRepo := db.NewSpendCapRepo(pool, log)
	spendCounterRepo := db.NewSpendCounterRepo(pool, log)
	usageEventRepo := db.NewUsageEventRepo(pool, log)
	orgSettingsRepo := db.NewOrgSettingsRepo(pool, log)
	workspacePrefsRepo := db.NewWorkspacePrefsRepo(pool, log)

	// Tenant-wide OneDrive OBO broker.
	// Nil when Vault is disabled — same precedent as connRegistry above;
	// ensureOneDriveOBO nil-checks Deps.OBOBroker so its absence means OneDrive
	// is never tenant-managed (legacy per-user connector path unaffected).
	var oboBroker *connector.OBOBroker
	// CP5: the prefs resolver + remote Backend adapter wired into both
	// workspace Routers below. Guarded identically to oboBroker itself (same
	// typed-nil trap this comment block already documents) — an unconfigured
	// tenant, or Vault disabled entirely, leaves these nil and both Routers
	// keep their CP3 all-local, behavior-neutral posture (Remote/Prefs nil).
	var workspacePrefResolver *brokersvc.WorkspacePrefResolver
	var oneDriveRemoteBackend workspacefs.Backend
	var oneDriveGraph brokersvc.OneDriveGraphStore
	if secretsProvider != nil {
		oboBroker = &connector.OBOBroker{
			Apps:    brokersvc.NewOrgSettingsM365Source(orgSettingsRepo, secretsProvider),
			Secrets: secretsProvider,
			Status:  connStore,
			HTTP:    &http.Client{Timeout: 12 * time.Second},
			Logger:  log,
		}

		odStore := &onedrivefs.Store{
			Tokens: brokersvc.NewOBOTokenSource(oboBroker),
			HTTP:   &http.Client{Timeout: 30 * time.Second},
		}
		oneDriveGraph = brokersvc.NewOneDriveGraphAdapter(odStore)
		workspacePrefResolver = brokersvc.NewWorkspacePrefResolver(workspacePrefsRepo, oboBroker)
		oneDriveRemoteBackend = brokersvc.NewOneDriveRemoteBackend(oneDriveGraph, workspacePrefResolver)
	}

	// F-8: ONE workspacefs.Store + ONE
	// Router, shared by the Tool Proxy, north, and south gRPC surfaces below —
	// *workspacefs.Router satisfies both broker's workspaceFS seam (Deps.
	// Workspace) and toolproxy's workspacefs.Backend seam (Config.WorkspaceFS)
	// unchanged, so one instance serves all three consumers. Previously each
	// surface built its own Store+Router over the identical root, each
	// carrying its own F14 per-(tenant,user) keyed mutex map — inert while
	// every consumer only called Read/Write/List (lock-free), but a future
	// Move/Mkdir/Delete reachable from more than one surface wouldn't have
	// serialized against a concurrent call via another. Sharing one instance
	// makes F14's mutex actually global across every consumer. Reuses the
	// SAME workspacePrefResolver/oneDriveRemoteBackend instances constructed
	// above — never a second WorkspacePrefResolver (a second instance would
	// run its own TTL cache, and SetWorkspaceBackend's invalidation would miss
	// it: stale routing for up to the TTL, an unacceptable split-brain).
	// Guarded identically to workspacePrefResolver itself: an unconfigured
	// tenant, or Vault disabled entirely, leaves Remote/Prefs nil, so every
	// doc.write/doc.read/workspace.read call with a known user still lands in
	// exactly the same on-disk directory doc.write always wrote to.
	sharedWorkspace := workspacefs.New(viper.GetString("toolproxy.workspace_root"))
	sharedWorkspace.SetSessionSigningKey(workspaceSessionKey)
	sharedWorkspaceRouter := &workspacefs.Router{Local: sharedWorkspace}
	if workspacePrefResolver != nil {
		sharedWorkspaceRouter.Remote = oneDriveRemoteBackend
		sharedWorkspaceRouter.Prefs = workspacePrefResolver
	}

	// Automated agent behavioral baseline learning
	//. baselineRecorder stays
	// nil when disabled — SandboxService.Deps.Baseline is nil-safe, so
	// InvokeTool's Observe call is simply skipped and no goroutines start.
	var baselineRecorder *baseline.DBRecorder
	baselineCfg := baseline.Config{
		WindowSize:       time.Duration(viper.GetInt("baseline.window_seconds")) * time.Second,
		LearnInterval:    time.Duration(viper.GetInt("baseline.learn_interval_seconds")) * time.Second,
		MinSampleWindows: viper.GetInt("baseline.min_sample_windows"),
		DriftMultiplier:  viper.GetFloat64("baseline.drift_multiplier"),
		RetentionWindows: viper.GetInt("baseline.retention_windows"),
	}
	var baselineRepo *db.AgentBaselineRepo
	var baselineLearner *baseline.Learner
	baselineDetector := baseline.NewDetector()
	// baselineIface is the interface-typed handle passed into Deps.Baseline —
	// deliberately left as a nil interface (not a nil *DBRecorder in an
	// interface box, which would compare non-nil) when baseline learning is
	// disabled, so InvokeTool's `s.deps.Baseline != nil` nil-check still holds.
	var baselineIface baseline.Recorder
	if viper.GetBool("baseline.enabled") {
		baselineRepo = db.NewAgentBaselineRepo(pool, log)
		baselineRecorder = baseline.NewDBRecorder(baselineRepo, baselineCfg.WindowSize, log)
		baselineLearner = baseline.NewLearner(baselineRepo, baselineCfg)
		baselineIface = baselineRecorder
	}

	// Load persisted rate-limit policies from DB into the in-process limiter.
	// ListAll, not List: the limiter's policy set spans every tenant and there is
	// no request-scoped tenant at startup. The previous List call was passed
	// broker.tenant_id, which is an FGA/audit *label* (e.g. "aikonos-dev"), not the
	// UUID the tenant_id column holds — so on any real deployment it failed with
	// SQLSTATE 22P02 and the limiter silently armed with nothing.
	//
	// Still non-fatal: a DB hiccup at boot must not brick the deployment, and an
	// admin can re-arm at runtime via SetRateLimitPolicy. Logged at Error, not
	// Warn, because an unarmed limiter is a security control that is off.
	if initPolicies, err := rateLimitRepo.ListAll(ctx); err != nil {
		log.Error("rate-limit: failed to load initial policies — limiter is UNRESTRICTED until a policy is set",
			zap.Error(err))
	} else {
		rateLimiter.SetPolicies(brokersvc.DBPoliciesToRatelimit(initPolicies))
		log.Info("rate-limit: loaded initial policies", zap.Int("count", len(initPolicies)))
	}

	// Enrich audit events with a human-readable actor_email (oid stays the stable
	// actor_user_id). Cached per-tenant so the audit hot path doesn't query per event.
	auditEmitter.AttachEmailResolver(brokersvc.NewCachedEmailResolver(userDirRepo, 60*time.Second))

	// MCP bearer-token store: reuse the Vault client (which satisfies
	// secrets.McpBearerStore — write, read, delete) when Vault is configured.
	var mcpSecrets secrets.McpBearerStore
	if vaultClient != nil {
		mcpSecrets = vaultClient
	}

	// Tool Proxy — executes registered tools (InvokeTool routes here after the
	// capability gate; the executor drives plan steps through it).
	toolProxyCfg := toolproxy.Config{
		WebFetch: toolproxy.WebFetchConfig{
			AllowPrivateHosts: viper.GetBool("toolproxy.web_fetch.allow_private"),
			ACL:               netChecker,
		},
		Workspace:       toolproxy.WorkspaceConfig{Root: viper.GetString("toolproxy.workspace_root")},
		WorkspaceFS:     sharedWorkspaceRouter,
		Secrets:         secretsProvider,
		OAuth:           connRegistry,
		McpConnections:  mcpRepo,
		McpBearer:       mcpSecrets,
		AllowMcpPrivate: viper.GetBool("toolproxy.mcp.allow_private"),
		OfficeWorkerURL: viper.GetString("office.worker_url"),
		OfficeTimeout:   time.Duration(viper.GetInt("office.job_timeout_ms")) * time.Millisecond,
		WebSearch: toolproxy.WebSearchConfig{
			Settings: brokersvc.NewOrgSettingsWebSearchSource(orgSettingsRepo),
			ACL:      netChecker,
		},
	}
	// Typed-nil trap (same precedent as northDeps.OBOBroker below): oboBroker
	// is a possibly-nil *connector.OBOBroker, and Config.OBO is interface-
	// typed — packing a nil pointer into it directly would make the interface
	// itself non-nil, defeating freshAccessToken's `d.obo != nil` guard.
	if oboBroker != nil {
		toolProxyCfg.OBO = oboBroker
	}
	toolProxy := toolproxy.New(toolProxyCfg, log)

	// C11: the plugin is the single source of
	// truth for its own scope + effect class — derive toolReg's baseline entries
	// for every self-registered plugin instead of hand-syncing a second table.
	toolReg.LoadFromPlugins(toolproxy.RegisteredPluginBaselines())

	// Provisioner: nil FGA writer when FGA is disabled so Seen is a no-op.
	// Must be constructed before the north gRPC server since the interceptor
	// is wired into the server chain at construction time.
	var fgaWriter provisioning.TupleWriter
	if policyEngine.FGAEnabled() {
		fgaWriter = policyTupleWriter{eng: policyEngine}
	}
	provisioner := provisioning.NewProvisioner(provisioningRepo, fgaWriter, auditEmitter, log)

	// ── gRPC server — North bound (frontend → Broker) ────────────────────────
	// mTLS using SPIFFE SVID; also validates OIDC token in metadata interceptor
	northCreds := grpccredentials.MTLSServerCredentials(svidSrc, bundleSrc,
		tlsconfig.AuthorizeAny())

	// 16 MiB message cap (default is 4) so workspace file upload/download
	// (ReadWorkspaceFile/UploadWorkspaceFile carry the file bytes) fits the
	// 10 MiB per-file limit with headroom.
	const maxMsgBytes = 16 << 20
	northServer := grpc.NewServer(
		grpc.Creds(northCreds),
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.MaxRecvMsgSize(maxMsgBytes),
		grpc.MaxSendMsgSize(maxMsgBytes),
		grpc.ChainUnaryInterceptor(
			auth.OIDCInterceptor(oidcValidator, auditEmitter, viper.GetString("broker.tenant_id"), log),
			provisioner.Interceptor(),
			auth.TenantInterceptor(auditEmitter, viper.GetString("broker.tenant_id"), log),
			auth.AuditInterceptor(auditEmitter, viper.GetString("broker.tenant_id"), log),
		),
	)

	auditReader := audit.NewReader(audit.Config{
		MinioEndpoint:    viper.GetString("audit.minio_endpoint"),
		Bucket:           viper.GetString("audit.bucket"),
		AccessKey:        viper.GetString("audit.access_key"),
		SecretKey:        viper.GetString("audit.secret_key"),
		UseSSL:           viper.GetBool("audit.use_ssl"),
		SigningKeySource: auditKeySource,
	})
	var auditReaderIface audit.ReaderIface
	if auditReader.Configured() {
		auditReaderIface = auditReader
	}

	// Seed LLM providers on boot (idempotent — existing ids are skipped).
	// Seeds under the UUID tenant the gateway queries by (AIKONOS_TENANT_ID),
	// not the broker.tenant_id label — DB tenant_id is a UUID column.
	llmseed.Seed(ctx, llmProviderRepo, secretsProvider,
		viper.GetString("tenant_id"),
		viper.GetString("llm_providers.seed_file"),
		log)

	// Seed provisioning rules on boot (idempotent — existing matchers skipped).
	provisioningseed.Seed(ctx, provisioningRepo, viper.GetString("tenant_id"),
		viper.GetString("provisioning.seed_file"), log)

	northDeps := brokersvc.Deps{
		Logger:      log,
		Tasks:       taskRepo,
		Policy:      policyEngine,
		Audit:       auditEmitter,
		AuditReader: auditReaderIface,
		Capability:  capMinter,
		Notify:      eventBus,
		ToolProxy:   toolProxy,
		TenantID:    viper.GetString("broker.tenant_id"),
		Scheduled:   scheduledRepo,
		// Only the north service creates/edits schedules — the south twins
		// (ClaimDue/ReportResult) never validate a cron, so the floor is wired here only.
		SchedulerMinInterval: viper.GetDuration("scheduler.min_interval"),
		Workspace:            brokersvc.WorkspaceDeps{Local: sharedWorkspaceRouter},
		Network:              networkRepo,
		ACL:                  netChecker,
		KnownUsers:           viper.GetStringSlice("admin.known_users"),
		Secrets:              secretsProvider,
		ConnAuth:             connState,
		OAuth:                connRegistry,
		Connectors:           connStore,
		Mcp: brokersvc.McpDeps{
			Connections:  mcpRepo,
			Bearer:       mcpSecrets,
			AllowPrivate: viper.GetBool("toolproxy.mcp.allow_private"),
		},
		Config:                   configStore,
		ToolRegistry:             toolReg,
		AlertRepo:                alertRepo,
		Agents:                   agentRepo,
		ApiKeys:                  apiKeyRepo,
		ApiKeyPepper:             viper.GetString("api_key.pepper"),
		Providers:                llmProviderRepo,
		AllowProviderTestPrivate: viper.GetBool("toolproxy.web_fetch.allow_private"),
		UsageMetrics:             usageMetrics,
		RateLimiter:              rateLimiter,
		RateLimitPolicies:        rateLimitRepo,
		SpendCaps:                spendCapRepo,
		SpendCounters:            spendCounterRepo,
		UsageEvents:              usageEventRepo,
		SpendCache:               spendCache,
		OrgSettings:              orgSettingsRepo,
		OtelEndpoint:             otelEndpoint,
		UserDirectory:            userDirRepo,
		Provisioning:             provisioningRepo,
		GatewayGrantKey:          gatewayGrantKey,
		GatewayGrantTTL:          gatewayGrantTTL,
		SkillOverlay:             skillOverlayRepo,
		AgentSkillBundles:        agentSkillRepo,
		Workflows:                workflowRepo,
	}
	// Assigned only when non-nil: oboBroker is a possibly-nil *connector.OBOBroker,
	// and Deps.OBOBroker is interface-typed — packing a nil pointer into it
	// directly would make the interface itself non-nil (the classic Go nil-
	// interface trap), defeating ensureOneDriveOBO's `== nil` guard and panicking
	// on the first Configured call. Same precedent as baselineIface above.
	if oboBroker != nil {
		northDeps.OBOBroker = oboBroker
		// workspacePrefResolver/oneDriveGraph are constructed in lockstep with
		// oboBroker above (same `if secretsProvider != nil` block), so this
		// guard covers all three — no separate nil-interface risk here since
		// Deps.Workspace.Prefs is a concrete *WorkspacePrefResolver field (plain
		// pointer, not interface-boxed) and oneDriveGraph was declared as the
		// OneDriveGraphStore interface type from the start (a genuine nil
		// interface in the unconfigured case, not a typed-nil pointer).
		northDeps.Workspace.Prefs = workspacePrefResolver
		northDeps.Workspace.Graph = oneDriveGraph
	}
	northSvc := brokersvc.NewBrokerService(northDeps)
	brokerv1.RegisterBrokerServiceServer(northServer, northSvc)
	reflection.Register(northServer)

	// ── gRPC server — South bound (sandbox → Broker) ─────────────────────────
	// mTLS using SPIFFE SVID; only SPIFFE auth, no OIDC
	southCreds := grpccredentials.MTLSServerCredentials(svidSrc, bundleSrc,
		identity.SouthAuthorizer())

	southServer := grpc.NewServer(
		grpc.Creds(southCreds),
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(
			auth.SPIFFEInterceptor(viper.GetString("spiffe.sandbox_path_prefix"), auditEmitter, viper.GetString("broker.tenant_id"), log),
			auth.AuditInterceptor(auditEmitter, viper.GetString("broker.tenant_id"), log),
		),
	)

	sandboxSvc := brokersvc.NewSandboxService(brokersvc.Deps{
		Logger:          log,
		Tasks:           taskRepo,
		Policy:          policyEngine,
		Audit:           auditEmitter,
		Capability:      capMinter,
		Notify:          eventBus,
		ToolProxy:       toolProxy,
		TenantID:        viper.GetString("broker.tenant_id"),
		Scheduled:       scheduledRepo,
		Workspace:       brokersvc.WorkspaceDeps{Local: sharedWorkspaceRouter},
		GatewaySpiffeID: viper.GetString("scheduler.gateway_spiffe_id"),
		ACL:             netChecker,
		Mcp: brokersvc.McpDeps{
			Connections:  mcpRepo,
			Bearer:       mcpSecrets,
			AllowPrivate: viper.GetBool("toolproxy.mcp.allow_private"),
		},
		ToolRegistry:      toolReg,
		Agents:            agentRepo,
		ApiKeys:           apiKeyRepo,
		ApiKeyPepper:      viper.GetString("api_key.pepper"),
		Secrets:           secretsProvider,
		Providers:         llmProviderRepo,
		UsageMetrics:      usageMetrics,
		TaskMetrics:       taskMetrics,
		Baseline:          baselineIface,
		RateLimiter:       rateLimiter,
		SpendCaps:         spendCapRepo,
		SpendCounters:     spendCounterRepo,
		UsageEvents:       usageEventRepo,
		SpendCache:        spendCache,
		GatewayGrantKey:   gatewayGrantKey,
		GatewayGrantTTL:   gatewayGrantTTL,
		SkillOverlay:      skillOverlayRepo,
		AgentSkillBundles: agentSkillRepo,
		OrgSettings:       orgSettingsRepo,
		Workflows:         workflowRepo,
	})
	brokerv1.RegisterSandboxServiceServer(southServer, sandboxSvc)
	reflection.Register(southServer) // dev convenience; south is mTLS+SPIFFE-gated

	// ── Start listeners ───────────────────────────────────────────────────────
	northAddr := fmt.Sprintf(":%d", viper.GetInt("broker.grpc_port_north"))
	southAddr := fmt.Sprintf(":%d", viper.GetInt("broker.grpc_port_south"))

	northLis, err := net.Listen("tcp", northAddr)
	if err != nil {
		log.Fatal("Failed to listen (north)", zap.Error(err))
	}
	southLis, err := net.Listen("tcp", southAddr)
	if err != nil {
		log.Fatal("Failed to listen (south)", zap.Error(err))
	}

	log.Info("Broker listening", zap.String("north", northAddr), zap.String("south", southAddr))

	go func() {
		if err := northServer.Serve(northLis); err != nil {
			log.Error("North gRPC server error", zap.Error(err))
		}
	}()
	go func() {
		if err := southServer.Serve(southLis); err != nil {
			log.Error("South gRPC server error", zap.Error(err))
		}
	}()

	// Automated agent behavioral baseline learning. Two tickers, both honoring ctx cancellation for
	// graceful shutdown; both no-ops when baseline learning is disabled.
	if baselineRecorder != nil {
		go runBaselineFlushDetectLoop(ctx, baselineCfg, baselineRecorder, baselineRepo, baselineDetector, agentMetrics, auditEmitter, log)
		go runBaselineLearnLoop(ctx, baselineCfg, baselineLearner, log)
	}

	// Per-call LLM usage event retention.
	if days := viper.GetInt("llm_usage.retention_days"); days > 0 {
		go runLlmUsagePruneLoop(ctx, usageEventRepo, days, log)
	}

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	<-ctx.Done()
	log.Info("Shutdown signal received")

	northServer.GracefulStop()
	southServer.GracefulStop()
	log.Info("Broker stopped cleanly")
}

// runBaselineFlushDetectLoop ticks every baselineCfg.WindowSize, flushing the
// recorder's in-memory buffer to Postgres and running the detector over every
// window that just closed. Each detected drift emits one
// aikonos.agent.baseline_drift audit event (fire-and-forget, RecordEmitFailure
// on error) plus one agentMetrics.RecordBaselineDrift counter increment.
// Honors ctx cancellation for graceful shutdown.
func runBaselineFlushDetectLoop(
	ctx context.Context,
	cfg baseline.Config,
	recorder *baseline.DBRecorder,
	repo *db.AgentBaselineRepo,
	detector *baseline.Detector,
	agentMetrics metrics.AgentMetricsRecorder,
	auditEmitter *audit.Emitter,
	log *zap.Logger,
) {
	ticker := time.NewTicker(cfg.WindowSize)
	defer ticker.Stop()
	detectorCfg := baseline.DetectorConfig{
		MinSampleWindows: cfg.MinSampleWindows,
		DriftMultiplier:  cfg.DriftMultiplier,
	}
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			closedWindows := recorder.Flush(ctx, now)
			for _, cw := range closedWindows {
				b, err := repo.GetBaseline(ctx, cw.Tenant, cw.Agent)
				if err != nil {
					// Learning phase (no baseline yet) or a transient DB error —
					// either way, monitoring is best-effort and must not block
					// the next window's flush.
					continue
				}
				window := baseline.WindowSummary{
					Tenant:      cw.Tenant,
					Agent:       cw.Agent,
					WindowStart: cw.WindowStart,
					Tools:       cw.Tools,
					Invocations: cw.Invocations,
				}
				detectorBaseline := baseline.Baseline{
					ToolSet:       b.ToolSet,
					RpmP95:        b.RpmP95,
					CostP95:       b.CostP95,
					SampleWindows: b.SampleWindows,
				}
				drifts := detector.Check(detectorBaseline, window, detectorCfg)
				for _, d := range drifts {
					emitBaselineDrift(ctx, auditEmitter, agentMetrics, cw.Tenant, cw.Agent, d, log)
				}
			}
		}
	}
}

// emitBaselineDrift fires one aikonos.agent.baseline_drift audit event (DENY,
// resourceRef aikonos:agent:<id>) and one OTel counter increment for a single
// detected drift. Fire-and-forget — never affects the flush/detect loop.
func emitBaselineDrift(ctx context.Context, auditEmitter *audit.Emitter, agentMetrics metrics.AgentMetricsRecorder, tenant, agent string, d baseline.Drift, log *zap.Logger) {
	if auditEmitter != nil {
		ctxStruct, _ := structpb.NewStruct(map[string]any{
			"kind":     d.Kind,
			"observed": d.Observed,
			"ceiling":  d.Ceiling,
			"tool":     d.Tool,
		})
		if err := auditEmitter.Emit(ctx, &auditv1.AuditEvent{
			EventId:     ids.EventID(),
			TenantId:    tenant,
			OccurredAt:  timestamppb.New(time.Now().UTC()),
			EventType:   "aikonos.agent.baseline_drift",
			ResourceRef: "aikonos:agent:" + agent,
			Decision:    auditv1.PolicyDecision_DENY,
			Context:     ctxStruct,
		}); err != nil {
			audit.RecordEmitFailure(ctx, log, err, "aikonos.agent.baseline_drift")
		}
	}
	if agentMetrics != nil {
		agentMetrics.RecordBaselineDrift(ctx, tenant, agent, d.Kind)
	}
}

// runLlmUsagePruneLoop ticks daily, deleting llm_usage_events rows older than
// retentionDays. Analytics retention only — the monthly spend counters caps
// enforce against are never pruned. Honors ctx cancellation for graceful
// shutdown. First sweep runs one tick in, not at startup: a broker that
// restart-loops must not hammer the DELETE.
func runLlmUsagePruneLoop(ctx context.Context, repo *db.UsageEventRepo, retentionDays int, log *zap.Logger) {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			cutoff := now.UTC().AddDate(0, 0, -retentionDays)
			removed, err := repo.PruneBefore(ctx, cutoff)
			if err != nil {
				log.Warn("llm usage retention: prune failed", zap.Error(err))
				continue
			}
			if removed > 0 {
				log.Info("llm usage retention: pruned events",
					zap.Int64("removed", removed), zap.Time("cutoff", cutoff))
			}
		}
	}
}

// runBaselineLearnLoop ticks every cfg.LearnInterval, calling
// learner.Recompute to refresh every agent's learned envelope and prune
// expired windows. Honors ctx cancellation for graceful shutdown.
func runBaselineLearnLoop(ctx context.Context, cfg baseline.Config, learner *baseline.Learner, log *zap.Logger) {
	interval := cfg.LearnInterval
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := learner.Recompute(ctx, now); err != nil {
				log.Warn("baseline learner: recompute failed", zap.Error(err))
			}
		}
	}
}

// newCapabilityMinter builds the Biscuit capability minter, resolving the root
// key in precedence order: explicit config key > Vault (read-or-create) >
// ephemeral dev key. The Vault path makes the key durable across broker
// restarts and identical across replicas; tokens minted by one replica then
// verify on another.
//
// durable is true when the key came from config or Vault (survives restarts);
// false only on the ephemeral fallback (dev only — tokens are lost on restart).
func newCapabilityMinter(ctx context.Context, rootKeyB64 string, vault *secrets.VaultClient, log *zap.Logger) (*capability.Minter, bool, error) {
	if rootKeyB64 != "" {
		m, err := minterFromB64(rootKeyB64)
		if err != nil {
			return nil, false, fmt.Errorf("capability.root_key: %w", err)
		}
		log.Info("capability minter initialized from configured root key",
			zap.String("public_key", base64.StdEncoding.EncodeToString(m.PublicKey())))
		return m, true, nil
	}

	if vault != nil {
		if m, err := capabilityKeyFromVault(ctx, vault, log); err != nil {
			log.Warn("capability: Vault key resolution failed — falling back to an EPHEMERAL key (dev only)", zap.Error(err))
		} else {
			return m, true, nil
		}
	}

	m, err := capability.GenerateMinter()
	if err != nil {
		return nil, false, err
	}
	log.Warn("capability root key not configured (no config key, no Vault) — generated an EPHEMERAL key (dev only)",
		zap.String("public_key", base64.StdEncoding.EncodeToString(m.PublicKey())))
	return m, false, nil
}

// capabilityKeyFromVault reads the shared root key from Vault, creating it on
// first boot (check-and-set so concurrent replicas converge on one key).
func capabilityKeyFromVault(ctx context.Context, vault *secrets.VaultClient, log *zap.Logger) (*capability.Minter, error) {
	for attempt := 0; attempt < 3; attempt++ {
		b64, err := vault.ReadCapabilityKey(ctx)
		if err == nil {
			m, derr := minterFromB64(b64)
			if derr != nil {
				return nil, fmt.Errorf("vault capability key invalid: %w", derr)
			}
			log.Info("capability minter initialized from Vault root key",
				zap.String("public_key", base64.StdEncoding.EncodeToString(m.PublicKey())))
			return m, nil
		}
		if !errors.Is(err, secrets.ErrNotFound) {
			return nil, err
		}
		// Not provisioned — generate and try to claim it.
		_, priv, gerr := ed25519.GenerateKey(rand.Reader)
		if gerr != nil {
			return nil, gerr
		}
		werr := vault.WriteCapabilityKey(ctx, base64.StdEncoding.EncodeToString(priv))
		if errors.Is(werr, secrets.ErrCASConflict) {
			continue // another replica won the race — re-read its key
		}
		if werr != nil {
			return nil, werr
		}
		m, derr := capability.NewMinter(priv)
		if derr != nil {
			return nil, derr
		}
		log.Info("capability: generated a new root key and stored it in Vault",
			zap.String("public_key", base64.StdEncoding.EncodeToString(m.PublicKey())))
		return m, nil
	}
	return nil, fmt.Errorf("capability: gave up resolving Vault key after retries")
}

// resolveGatewayGrantKey returns the 32-byte HMAC key used to sign owner-grant
// tokens, in precedence order: explicit config override > Vault read-or-create
// > ephemeral per-process random bytes (dev fallback, with a Warn log).
// Mirrors newCapabilityMinter — same Vault semantics (cas=0, shared replicas).
func resolveGatewayGrantKey(ctx context.Context, b64Override string, vault *secrets.VaultClient, log *zap.Logger) ([]byte, error) {
	if b64Override != "" {
		key, err := base64.StdEncoding.DecodeString(b64Override)
		if err != nil {
			// Try RawURL encoding as well (spec says "base64 std or raw-url").
			key, err = base64.RawURLEncoding.DecodeString(b64Override)
			if err != nil {
				return nil, fmt.Errorf("gateway_grant.key: decode: %w", err)
			}
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("gateway_grant.key: must be 32 bytes, got %d", len(key))
		}
		log.Info("gateway grant key initialized from configured override")
		return key, nil
	}

	if vault != nil {
		if key, err := gatewayGrantKeyFromVault(ctx, vault, log); err != nil {
			log.Warn("gateway grant: Vault key resolution failed — falling back to an EPHEMERAL key (dev only)", zap.Error(err))
		} else {
			return key, nil
		}
	}

	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("gateway grant: generate ephemeral key: %w", err)
	}
	log.Warn("gateway grant key not configured (no config key, no Vault) — generated an EPHEMERAL key (dev only)")
	return key, nil
}

// gatewayGrantKeyFromVault reads the shared grant key from Vault, creating it
// on first boot (cas=0 so concurrent replicas converge on one key).
func gatewayGrantKeyFromVault(ctx context.Context, vault *secrets.VaultClient, log *zap.Logger) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		b64, err := vault.ReadGatewayGrantKey(ctx)
		if err == nil {
			key, derr := base64.StdEncoding.DecodeString(b64)
			if derr != nil {
				return nil, fmt.Errorf("vault gateway-grant key invalid: %w", derr)
			}
			log.Info("gateway grant key initialized from Vault")
			return key, nil
		}
		if !errors.Is(err, secrets.ErrNotFound) {
			return nil, err
		}
		// Not provisioned — generate and try to claim it.
		key := make([]byte, 32)
		if _, gerr := rand.Read(key); gerr != nil {
			return nil, gerr
		}
		werr := vault.WriteGatewayGrantKey(ctx, base64.StdEncoding.EncodeToString(key))
		if errors.Is(werr, secrets.ErrCASConflict) {
			continue // another replica won the race — re-read its key
		}
		if werr != nil {
			return nil, werr
		}
		log.Info("gateway grant: generated a new key and stored it in Vault")
		return key, nil
	}
	return nil, fmt.Errorf("gateway grant: gave up resolving Vault key after retries")
}

// ── Audit signing key (CP1.2) ────────────────────────────────────────────────
// This is the one place that depends on both broker/internal/audit and
// broker/internal/secrets — audit.SigningKeySource stays a plain interface so
// the audit package itself never imports the Vault client and remains
// testable with a fake.

// auditSigningKeySource implements audit.SigningKeySource. currentKey/
// currentVersion are resolved once at startup (Vault read-or-create, or the
// env fallback at version 0). ForVersion resolves *other* historic Vault
// versions on demand — this is what lets VerifyChain span a rotation without
// requiring a broker restart to pick up the new version's key.
type auditSigningKeySource struct {
	vault          *secrets.VaultClient // nil when Vault is disabled/unavailable — env-only mode
	envKey         string               // AIKONOS_AUDIT_SIGNING_KEY fallback, always version 0
	currentKey     string
	currentVersion int32
}

func (s *auditSigningKeySource) Current() (string, int32) { return s.currentKey, s.currentVersion }

func (s *auditSigningKeySource) ForVersion(ctx context.Context, version int32) (string, error) {
	if version == 0 {
		return s.envKey, nil
	}
	if s.vault == nil {
		return "", fmt.Errorf("audit: signing_key_version %d requires Vault, but Vault is not configured", version)
	}
	b64, err := s.vault.ReadAuditSigningKeyVersion(ctx, version)
	if err != nil {
		return "", err
	}
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", fmt.Errorf("vault audit-signing-key version %d invalid: %w", version, err)
	}
	return string(key), nil
}

// resolveAuditSigningKey returns the audit.SigningKeySource used to sign and
// verify audit events, in precedence order: Vault (read-or-create, versioned)
// > AIKONOS_AUDIT_SIGNING_KEY env fallback (version 0). Never errors — an
// empty envKey and no Vault yields an unsigned (chained-only) source, mirroring
// the emitter's pre-existing degraded behavior.
//
// The warning fires whenever the Vault path isn't the one actually in use
// (Vault disabled, or a read/write error) — grep-stable, mirrors the existing
// "provider key missing from Vault" convention.
func resolveAuditSigningKey(ctx context.Context, envKey string, vault *secrets.VaultClient, log *zap.Logger) audit.SigningKeySource {
	if vault != nil {
		if key, version, err := auditSigningKeyFromVault(ctx, vault, log); err == nil {
			log.Info("audit signing key initialized from Vault", zap.Int32("version", version))
			return &auditSigningKeySource{vault: vault, envKey: envKey, currentKey: key, currentVersion: version}
		}
	}
	log.Warn("audit signing key: Vault unavailable — falling back to static env key",
		zap.Bool("signed", envKey != ""))
	return &auditSigningKeySource{vault: vault, envKey: envKey, currentKey: envKey, currentVersion: 0}
}

// auditSigningKeyFromVault reads the shared signing key from Vault, creating
// it on first boot (cas=0 so concurrent replicas converge on one key). Mirrors
// gatewayGrantKeyFromVault, but also returns the KV-v2 version in use.
func auditSigningKeyFromVault(ctx context.Context, vault *secrets.VaultClient, log *zap.Logger) (string, int32, error) {
	for attempt := 0; attempt < 3; attempt++ {
		b64, version, err := vault.ReadAuditSigningKey(ctx)
		if err == nil {
			key, derr := base64.StdEncoding.DecodeString(b64)
			if derr != nil {
				return "", 0, fmt.Errorf("vault audit-signing-key invalid: %w", derr)
			}
			return string(key), version, nil
		}
		if !errors.Is(err, secrets.ErrNotFound) {
			return "", 0, err
		}
		// Not provisioned — generate and try to claim it.
		key := make([]byte, 32)
		if _, gerr := rand.Read(key); gerr != nil {
			return "", 0, gerr
		}
		werr := vault.WriteAuditSigningKey(ctx, base64.StdEncoding.EncodeToString(key))
		if errors.Is(werr, secrets.ErrCASConflict) {
			continue // another replica won the race — re-read its key
		}
		if werr != nil {
			return "", 0, werr
		}
		log.Info("audit: generated a new signing key and stored it in Vault")
		// Re-read to capture the authoritative KV-v2 version (mirrors the
		// capability/gateway-grant bootstrap pattern's re-read-after-write).
		b64, version, err = vault.ReadAuditSigningKey(ctx)
		if err != nil {
			return "", 0, err
		}
		key, derr := base64.StdEncoding.DecodeString(b64)
		if derr != nil {
			return "", 0, fmt.Errorf("vault audit-signing-key invalid: %w", derr)
		}
		return string(key), version, nil
	}
	return "", 0, fmt.Errorf("audit signing key: gave up resolving Vault key after retries")
}

// ── Workspace session integrity key (CP4.2) ──────────────────────────────────
// Mirrors resolveAuditSigningKey/auditSigningKeyFromVault above: Vault
// read-or-create (cas=0, shared by every replica) with a static env fallback
// when Vault is unavailable — same degraded posture as CP1.2, minus
// versioning (a single active key; rotating it invalidates older sidecars,
// which is an accepted operator tradeoff documented in OPS-RUNBOOK.md, not a
// requirement of CP4.2).

// resolveWorkspaceSessionKey returns the raw HMAC key bytes for session-file
// integrity sidecars, in precedence order: Vault (read-or-create) > the
// AIKONOS_WORKSPACE_SESSION_SIGNING_KEY env fallback > disabled (nil — no
// sidecars are written or verified). Never errors: an empty/invalid env value
// and no Vault degrades to disabled rather than failing broker startup.
func resolveWorkspaceSessionKey(ctx context.Context, envB64 string, vault *secrets.VaultClient, log *zap.Logger) []byte {
	if vault != nil {
		if key, err := workspaceSessionKeyFromVault(ctx, vault, log); err == nil {
			log.Info("workspace session signing key initialized from Vault")
			return key
		}
	}
	// Grep-stable — mirrors the existing "provider key missing from Vault" /
	// "audit signing key: Vault unavailable" convention.
	log.Warn("workspace session signing key: Vault unavailable — falling back to static env key",
		zap.Bool("signed", envB64 != ""))
	if envB64 == "" {
		return nil
	}
	key, err := base64.StdEncoding.DecodeString(envB64)
	if err != nil {
		log.Warn("workspace.session_signing_key: invalid base64 — session sidecars disabled", zap.Error(err))
		return nil
	}
	return key
}

// workspaceSessionKeyFromVault reads the shared signing key from Vault,
// creating it on first boot (cas=0 so concurrent replicas converge on one
// key). Mirrors gatewayGrantKeyFromVault/auditSigningKeyFromVault.
func workspaceSessionKeyFromVault(ctx context.Context, vault *secrets.VaultClient, log *zap.Logger) ([]byte, error) {
	for attempt := 0; attempt < 3; attempt++ {
		b64, err := vault.ReadWorkspaceSessionKey(ctx)
		if err == nil {
			key, derr := base64.StdEncoding.DecodeString(b64)
			if derr != nil {
				return nil, fmt.Errorf("vault workspace-session-key invalid: %w", derr)
			}
			return key, nil
		}
		if !errors.Is(err, secrets.ErrNotFound) {
			return nil, err
		}
		// Not provisioned — generate and try to claim it.
		key := make([]byte, 32)
		if _, gerr := rand.Read(key); gerr != nil {
			return nil, gerr
		}
		werr := vault.WriteWorkspaceSessionKey(ctx, base64.StdEncoding.EncodeToString(key))
		if errors.Is(werr, secrets.ErrCASConflict) {
			continue // another replica won the race — re-read its key
		}
		if werr != nil {
			return nil, werr
		}
		log.Info("workspace session key: generated a new key and stored it in Vault")
		return key, nil
	}
	return nil, fmt.Errorf("workspace session key: gave up resolving Vault key after retries")
}

func minterFromB64(b64 string) (*capability.Minter, error) {
	keyBytes, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return capability.NewMinter(ed25519.PrivateKey(keyBytes))
}

// otelResource builds the shared OTel resource (service.name + service.version)
// used by both the tracer and meter providers so traces and metrics carry the
// same identifying attributes.
func otelResource(ctx context.Context) (*resource.Resource, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("aikonos-broker"),
			semconv.ServiceVersion("0.1.0"),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}
	return res, nil
}

// initTracer sets up the OTLP gRPC trace exporter. An empty endpoint disables
// tracing (no exporter, no-op provider) — same opt-in posture as OIDC/OpenFGA,
// so the broker doesn't spam export retries when no collector is deployed.
func initTracer(ctx context.Context, endpoint string) (*sdktrace.TracerProvider, error) {
	res, err := otelResource(ctx)
	if err != nil {
		return nil, err
	}

	if endpoint == "" {
		return sdktrace.NewTracerProvider(sdktrace.WithResource(res)), nil
	}

	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(endpoint),
		otlptracegrpc.WithInsecure(), // TLS on the OTel collector link in Phase 5
	)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()), // Phase 5: adaptive sampler
	)
	return tp, nil
}

// buildConnectorCredentials populates one AppCredentials entry per registered
// connector provider by reading "connectors.<key>.client_id"/".client_secret"
// via get (viper.GetString in production). Looping RegisteredProviders()
// instead of hardcoding google/microsoft means a third-party plugin's
// credentials are picked up with no main.go edit; ConfigKeyFor preserves the
// existing "connectors.google.*" / "connectors.microsoft.*" viper keys — the
// provider constant's own string value ("google_drive", "onedrive") is
// deliberately not used as the config-key segment.
func buildConnectorCredentials(get func(string) string) map[connector.Provider]connector.AppCredentials {
	creds := make(map[connector.Provider]connector.AppCredentials)
	for _, p := range connector.RegisteredProviders() {
		key, ok := connector.ConfigKeyFor(p)
		if !ok {
			continue
		}
		creds[p] = connector.AppCredentials{
			ClientID:     get("connectors." + key + ".client_id"),
			ClientSecret: get("connectors." + key + ".client_secret"),
		}
	}
	return creds
}

// initMeter sets up the OTLP gRPC metric exporter. An empty endpoint disables
// metrics (no exporter, no-op provider) — mirrors initTracer's opt-in posture.
func initMeter(ctx context.Context, endpoint string) (*sdkmetric.MeterProvider, error) {
	res, err := otelResource(ctx)
	if err != nil {
		return nil, err
	}

	if endpoint == "" {
		return sdkmetric.NewMeterProvider(sdkmetric.WithResource(res)), nil
	}

	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(endpoint),
		otlpmetricgrpc.WithInsecure(), // TLS on the OTel collector link in Phase 5
	)
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}

	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
	)
	return mp, nil
}
