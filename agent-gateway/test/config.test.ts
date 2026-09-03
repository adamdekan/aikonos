// Tests for the config validation layer (CP2 — platform-hardening).
// Uses buildConfig(env) so tests can supply a synthetic env without touching
// process.env or triggering the real .env file load.
import { test } from "node:test";
import assert from "node:assert/strict";

import { buildConfig } from "../src/config.js";

// Minimal valid env — all required vars at their sensible defaults.
function validEnv(): Record<string, string> {
  return {
    OPENROUTER_API_KEY: "sk-test",
    AIKONOS_LLM_MODEL: "anthropic/claude-sonnet-4.6",
    AIKONOS_BROKER_NORTH_ADDR: "127.0.0.1:9090",
    AIKONOS_BROKER_SOUTH_ADDR: "127.0.0.1:9091",
    AIKONOS_BROKER_SERVER_NAME: "broker.aikonos-platform.svc.cluster.local",
    AIKONOS_TLS_CERT: ".svid/svid.pem",
    AIKONOS_TLS_KEY: ".svid/key.pem",
    AIKONOS_TLS_CA: ".svid/ca.pem",
    AIKONOS_GATEWAY_SPIFFE_ID: "spiffe://aikonos.com/agent-gateway",
    PORT: "8080",
    AIKONOS_TENANT_ID: "11111111-1111-1111-1111-111111111111",
    AIKONOS_OIDC_ISSUER: "",
    AIKONOS_OIDC_JWKS_URL: "",
    AIKONOS_OIDC_AUDIENCE: "aikonos-broker",
    AIKONOS_OIDC_SUBJECT_CLAIM: "sub",
    AIKONOS_OIDC_TENANT_CLAIM: "tenant_id",
    AIKONOS_SCHEDULER_ENABLED: "false",
    AIKONOS_SCHEDULER_TICK_MS: "30000",
    AIKONOS_SCHEDULER_CLAIM_LIMIT: "10",
    AIKONOS_SCHEDULER_RUN_TIMEOUT_MS: "180000",
    AIKONOS_EXTERNAL_PORT: "8090",
    AIKONOS_EXTERNAL_CORS_ORIGINS: "",
    AIKONOS_EXTERNAL_RATE_LIMIT: "60",
    AIKONOS_GATEWAY_THREAD_TTL_MS: "1800000",
    AIKONOS_GATEWAY_MAX_CHILDREN: "32",
    AIKONOS_GATEWAY_CHILD_TTL_MS: "1800000",
    AIKONOS_NATS_URL: "nats://nats:4222",
    AIKONOS_AUDIT_SUBJECT: "aikonos.audit.>",
    AIKONOS_GATEWAY_EGRESS_TIMEOUT_MS: "120000",
    AIKONOS_GATEWAY_BROKER_TIMEOUT_MS: "30000",
  };
}

// ── Valid config ──────────────────────────────────────────────────────────────

test("config: valid env loads without error", () => {
  const cfg = buildConfig(validEnv());
  assert.equal(cfg.port, 8080);
  assert.equal(cfg.schedulerEnabled, false);
  assert.equal(cfg.externalPort, 8090);
});

test("config: schedulerEnabled true is accepted", () => {
  const cfg = buildConfig({ ...validEnv(), AIKONOS_SCHEDULER_ENABLED: "true" });
  assert.equal(cfg.schedulerEnabled, true);
});

// ── Empty-meaning-off vars stay valid ────────────────────────────────────────

test("config: empty AIKONOS_OIDC_ISSUER is accepted (passthrough mode)", () => {
  const cfg = buildConfig({ ...validEnv(), AIKONOS_OIDC_ISSUER: "" });
  assert.equal(cfg.oidcIssuer, "");
});

test("config: empty AIKONOS_OIDC_JWKS_URL is accepted", () => {
  const cfg = buildConfig({ ...validEnv(), AIKONOS_OIDC_JWKS_URL: "" });
  assert.equal(cfg.oidcJwksUrl, "");
});

test("config: empty AIKONOS_EXTERNAL_CORS_ORIGINS is accepted (no CORS applied)", () => {
  const cfg = buildConfig({ ...validEnv(), AIKONOS_EXTERNAL_CORS_ORIGINS: "" });
  assert.deepEqual(cfg.externalCorsOrigins, []);
});

// ── Boolean validation ────────────────────────────────────────────────────────

test("config: non-boolean string for AIKONOS_SCHEDULER_ENABLED is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_SCHEDULER_ENABLED: "yes" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_SCHEDULER_ENABLED/);
      assert.match(err.message, /true.*false|false.*true/);
      return true;
    },
  );
});

test("config: '1' for AIKONOS_SCHEDULER_ENABLED is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_SCHEDULER_ENABLED: "1" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_SCHEDULER_ENABLED/);
      return true;
    },
  );
});

test("config: 'maybe' for AIKONOS_SCHEDULER_ENABLED is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_SCHEDULER_ENABLED: "maybe" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_SCHEDULER_ENABLED/);
      return true;
    },
  );
});

test("config: '1x' for AIKONOS_SCHEDULER_ENABLED is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_SCHEDULER_ENABLED: "1x" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_SCHEDULER_ENABLED/);
      return true;
    },
  );
});

// ── Numeric validation: NaN ───────────────────────────────────────────────────

test("config: non-numeric PORT is rejected with var name in error", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), PORT: "abc" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /PORT/);
      assert.match(err.message, /positive integer/);
      return true;
    },
  );
});

test("config: non-numeric AIKONOS_SCHEDULER_TICK_MS is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_SCHEDULER_TICK_MS: "fast" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_SCHEDULER_TICK_MS/);
      return true;
    },
  );
});

test("config: non-numeric AIKONOS_SCHEDULER_CLAIM_LIMIT is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_SCHEDULER_CLAIM_LIMIT: "lots" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_SCHEDULER_CLAIM_LIMIT/);
      return true;
    },
  );
});

test("config: non-numeric AIKONOS_SCHEDULER_RUN_TIMEOUT_MS is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_SCHEDULER_RUN_TIMEOUT_MS: "inf" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_SCHEDULER_RUN_TIMEOUT_MS/);
      return true;
    },
  );
});

test("config: non-numeric AIKONOS_EXTERNAL_PORT is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_EXTERNAL_PORT: "none" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_EXTERNAL_PORT/);
      return true;
    },
  );
});

test("config: non-numeric AIKONOS_EXTERNAL_RATE_LIMIT is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_EXTERNAL_RATE_LIMIT: "many" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_EXTERNAL_RATE_LIMIT/);
      return true;
    },
  );
});

test("config: non-numeric AIKONOS_GATEWAY_THREAD_TTL_MS is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_THREAD_TTL_MS: "forever" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_THREAD_TTL_MS/);
      return true;
    },
  );
});

// ── Numeric validation: negative ──────────────────────────────────────────────

test("config: negative PORT is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), PORT: "-1" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /PORT/);
      return true;
    },
  );
});

test("config: zero PORT is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), PORT: "0" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /PORT/);
      return true;
    },
  );
});

test("config: negative AIKONOS_SCHEDULER_TICK_MS is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_SCHEDULER_TICK_MS: "-1000" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_SCHEDULER_TICK_MS/);
      return true;
    },
  );
});

test("config: negative AIKONOS_EXTERNAL_PORT is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_EXTERNAL_PORT: "-8090" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_EXTERNAL_PORT/);
      return true;
    },
  );
});

test("config: negative AIKONOS_EXTERNAL_RATE_LIMIT is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_EXTERNAL_RATE_LIMIT: "-5" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_EXTERNAL_RATE_LIMIT/);
      return true;
    },
  );
});

test("config: negative AIKONOS_GATEWAY_THREAD_TTL_MS is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_THREAD_TTL_MS: "-100" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_THREAD_TTL_MS/);
      return true;
    },
  );
});

// ── Default-fallback regression ───────────────────────────────────────────────

test("config: minimal env (only OPENROUTER_API_KEY) uses correct numeric and boolean defaults", () => {
  // Only supply the one var that has no fallback (empty-string fallback is fine
  // for OIDC vars, but OPENROUTER_API_KEY defaults to "" which is falsy — keep
  // it non-empty so the intent of the test is clear). All other vars absent →
  // must resolve via the hardcoded fallback in buildConfig.
  const cfg = buildConfig({ OPENROUTER_API_KEY: "sk-minimal" });
  assert.equal(cfg.port, 8080);
  assert.equal(cfg.externalPort, 8090);
  assert.equal(cfg.schedulerEnabled, false);
  assert.equal(cfg.schedulerTickMs, 30000);
  assert.equal(cfg.schedulerClaimLimit, 10);
  assert.equal(cfg.schedulerRunTimeoutMs, 180000);
  assert.equal(cfg.threadTtlMs, 1800000);
  assert.equal(cfg.maxChildren, 32);
  assert.equal(cfg.childTtlMs, 1800000);
  assert.equal(cfg.natsUrl, "nats://nats:4222");
  assert.equal(cfg.auditSubject, "aikonos.audit.>");
  assert.equal(cfg.egressTimeoutMs, 120000);
  assert.equal(cfg.brokerTimeoutMs, 30000);
});

// ── F26: gateway pool / audit / egress-timeout config ────────────────────────

test("config: valid AIKONOS_GATEWAY_MAX_CHILDREN and AIKONOS_GATEWAY_CHILD_TTL_MS parse", () => {
  const cfg = buildConfig({
    ...validEnv(),
    AIKONOS_GATEWAY_MAX_CHILDREN: "8",
    AIKONOS_GATEWAY_CHILD_TTL_MS: "60000",
  });
  assert.equal(cfg.maxChildren, 8);
  assert.equal(cfg.childTtlMs, 60000);
});

// RED-FIRST for F26: before this change, supervisor.ts's defaultSupervisorConfig
// read AIKONOS_GATEWAY_MAX_CHILDREN itself via `Number(...)`, so a malformed value
// silently became NaN and wedged the pool cap/LRU-eviction comparisons instead of
// failing at startup. Moving the parse behind buildConfig's parsePositiveInt makes
// this fail loud where an operator will actually see it.
test("config: non-numeric AIKONOS_GATEWAY_MAX_CHILDREN is rejected at buildConfig (not silently NaN)", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_MAX_CHILDREN: "abc" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_MAX_CHILDREN/);
      assert.match(err.message, /positive integer/);
      return true;
    },
  );
});

test("config: non-numeric AIKONOS_GATEWAY_CHILD_TTL_MS is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_CHILD_TTL_MS: "never" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_CHILD_TTL_MS/);
      return true;
    },
  );
});

test("config: negative AIKONOS_GATEWAY_MAX_CHILDREN is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_MAX_CHILDREN: "-1" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_MAX_CHILDREN/);
      return true;
    },
  );
});

test("config: non-numeric AIKONOS_GATEWAY_EGRESS_TIMEOUT_MS is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_EGRESS_TIMEOUT_MS: "slow" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_EGRESS_TIMEOUT_MS/);
      return true;
    },
  );
});

test("config: AIKONOS_GATEWAY_EGRESS_TIMEOUT_MS parses and defaults to 120000", () => {
  const cfg = buildConfig({ ...validEnv(), AIKONOS_GATEWAY_EGRESS_TIMEOUT_MS: "60000" });
  assert.equal(cfg.egressTimeoutMs, 60000);
  const defaulted = buildConfig({ OPENROUTER_API_KEY: "sk-minimal" });
  assert.equal(defaulted.egressTimeoutMs, 120000);
});

test("config: AIKONOS_NATS_URL takes precedence over NATS_URL", () => {
  const cfg = buildConfig({
    ...validEnv(),
    AIKONOS_NATS_URL: "nats://primary:4222",
    NATS_URL: "nats://legacy:4222",
  });
  assert.equal(cfg.natsUrl, "nats://primary:4222");
});

test("config: NATS_URL is used when AIKONOS_NATS_URL is unset (legacy fallback preserved)", () => {
  const env = validEnv();
  delete (env as Record<string, string | undefined>).AIKONOS_NATS_URL;
  const cfg = buildConfig({ ...env, NATS_URL: "nats://legacy-only:4222" });
  assert.equal(cfg.natsUrl, "nats://legacy-only:4222");
});

test("config: natsUrl defaults to nats://nats:4222 when neither var is set", () => {
  const env = validEnv();
  delete (env as Record<string, string | undefined>).AIKONOS_NATS_URL;
  const cfg = buildConfig(env);
  assert.equal(cfg.natsUrl, "nats://nats:4222");
});

test("config: explicit empty AIKONOS_NATS_URL is preserved (disables the audit consumer)", () => {
  const cfg = buildConfig({ ...validEnv(), AIKONOS_NATS_URL: "" });
  assert.equal(cfg.natsUrl, "");
});

test("config: AIKONOS_AUDIT_SUBJECT defaults to aikonos.audit.>", () => {
  const env = validEnv();
  delete (env as Record<string, string | undefined>).AIKONOS_AUDIT_SUBJECT;
  const cfg = buildConfig(env);
  assert.equal(cfg.auditSubject, "aikonos.audit.>");
});

test("config: AIKONOS_AUDIT_SUBJECT is honoured when set", () => {
  const cfg = buildConfig({ ...validEnv(), AIKONOS_AUDIT_SUBJECT: "custom.audit.>" });
  assert.equal(cfg.auditSubject, "custom.audit.>");
});

// ── CP1 (rpc-twins-tails): broker unary deadline ─────────────────────────────

test("config: AIKONOS_GATEWAY_BROKER_TIMEOUT_MS parses and defaults to 30000", () => {
  const cfg = buildConfig({ ...validEnv(), AIKONOS_GATEWAY_BROKER_TIMEOUT_MS: "5000" });
  assert.equal(cfg.brokerTimeoutMs, 5000);
  const defaulted = buildConfig({ OPENROUTER_API_KEY: "sk-minimal" });
  assert.equal(defaulted.brokerTimeoutMs, 30000);
});

test("config: non-numeric AIKONOS_GATEWAY_BROKER_TIMEOUT_MS is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_BROKER_TIMEOUT_MS: "slow" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_BROKER_TIMEOUT_MS/);
      assert.match(err.message, /positive integer/);
      return true;
    },
  );
});

test("config: negative AIKONOS_GATEWAY_BROKER_TIMEOUT_MS is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_BROKER_TIMEOUT_MS: "-1" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_BROKER_TIMEOUT_MS/);
      return true;
    },
  );
});

// ── CP4.1 (zt-enterprise-ladder): rate-limit circuit breaker threshold ───────

test("config: AIKONOS_GATEWAY_RATELIMIT_BREAKER_THRESHOLD parses and defaults to 5", () => {
  const cfg = buildConfig({ ...validEnv(), AIKONOS_GATEWAY_RATELIMIT_BREAKER_THRESHOLD: "3" });
  assert.equal(cfg.rateLimitBreakerThreshold, 3);
  const defaulted = buildConfig({ OPENROUTER_API_KEY: "sk-minimal" });
  assert.equal(defaulted.rateLimitBreakerThreshold, 5);
});

test("config: non-numeric AIKONOS_GATEWAY_RATELIMIT_BREAKER_THRESHOLD is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_RATELIMIT_BREAKER_THRESHOLD: "many" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_RATELIMIT_BREAKER_THRESHOLD/);
      assert.match(err.message, /positive integer/);
      return true;
    },
  );
});

test("config: negative AIKONOS_GATEWAY_RATELIMIT_BREAKER_THRESHOLD is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_RATELIMIT_BREAKER_THRESHOLD: "-1" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_RATELIMIT_BREAKER_THRESHOLD/);
      return true;
    },
  );
});

// ── Numeric validation: float ─────────────────────────────────────────────────

test("config: float PORT is rejected (not a positive integer)", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), PORT: "8080.5" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /PORT/);
      assert.match(err.message, /positive integer/);
      return true;
    },
  );
});

// ── Subagent fan-out knobs ────────────────

test("config: AIKONOS_GATEWAY_SUBAGENT_MAX_WIDTH parses and defaults to 3", () => {
  const cfg = buildConfig({ ...validEnv(), AIKONOS_GATEWAY_SUBAGENT_MAX_WIDTH: "5" });
  assert.equal(cfg.subagentMaxWidth, 5);
  const defaulted = buildConfig({ OPENROUTER_API_KEY: "sk-minimal" });
  assert.equal(defaulted.subagentMaxWidth, 3);
});

test("config: non-numeric AIKONOS_GATEWAY_SUBAGENT_MAX_WIDTH is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_SUBAGENT_MAX_WIDTH: "wide" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_SUBAGENT_MAX_WIDTH/);
      assert.match(err.message, /positive integer/);
      return true;
    },
  );
});

test("config: zero AIKONOS_GATEWAY_SUBAGENT_MAX_WIDTH is rejected (a width-0 fan-out has no meaning)", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_SUBAGENT_MAX_WIDTH: "0" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_SUBAGENT_MAX_WIDTH/);
      return true;
    },
  );
});

test("config: AIKONOS_GATEWAY_SUBAGENT_BRANCH_TIMEOUT_MS parses and defaults to 180000 (mirrors the scheduler run timeout)", () => {
  const cfg = buildConfig({ ...validEnv(), AIKONOS_GATEWAY_SUBAGENT_BRANCH_TIMEOUT_MS: "60000" });
  assert.equal(cfg.subagentBranchTimeoutMs, 60000);
  const defaulted = buildConfig({ OPENROUTER_API_KEY: "sk-minimal" });
  assert.equal(defaulted.subagentBranchTimeoutMs, 180000);
  assert.equal(defaulted.subagentBranchTimeoutMs, defaulted.schedulerRunTimeoutMs);
});

test("config: non-numeric AIKONOS_GATEWAY_SUBAGENT_BRANCH_TIMEOUT_MS is rejected", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_SUBAGENT_BRANCH_TIMEOUT_MS: "soon" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_SUBAGENT_BRANCH_TIMEOUT_MS/);
      assert.match(err.message, /positive integer/);
      return true;
    },
  );
});

test("config: zero AIKONOS_GATEWAY_SUBAGENT_BRANCH_TIMEOUT_MS is rejected (0 would mean an unbounded branch, not a disabled guard)", () => {
  assert.throws(
    () => buildConfig({ ...validEnv(), AIKONOS_GATEWAY_SUBAGENT_BRANCH_TIMEOUT_MS: "0" }),
    (err: unknown) => {
      assert.ok(err instanceof Error);
      assert.match(err.message, /AIKONOS_GATEWAY_SUBAGENT_BRANCH_TIMEOUT_MS/);
      return true;
    },
  );
});
