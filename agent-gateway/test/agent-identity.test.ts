// F26: agentOverridesFromEnv fails loud on malformed AIKONOS_AGENT_FOR_USER
// JSON instead of silently returning an empty override map.
import { test } from "node:test";
import assert from "node:assert/strict";
import { agentForUser, agentOverridesFromEnv } from "../src/broker/agent-identity.js";

function withEnv(name: string, value: string | undefined, fn: () => void): void {
  const prev = process.env[name];
  if (value === undefined) delete process.env[name];
  else process.env[name] = value;
  try {
    fn();
  } finally {
    if (prev === undefined) delete process.env[name];
    else process.env[name] = prev;
  }
}

test("agentOverridesFromEnv: unset env returns an empty map", () => {
  withEnv("AIKONOS_AGENT_FOR_USER", undefined, () => {
    assert.deepEqual(agentOverridesFromEnv(), {});
  });
});

test("agentOverridesFromEnv: valid JSON object of string overrides is parsed", () => {
  withEnv("AIKONOS_AGENT_FOR_USER", '{"alice@example.com":"alice-custom-agent"}', () => {
    assert.deepEqual(agentOverridesFromEnv(), { "alice@example.com": "alice-custom-agent" });
  });
});

// RED-FIRST: before this change, malformed JSON was swallowed by a bare catch
// that returned {} — an operator typo in AIKONOS_AGENT_FOR_USER silently
// produced no overrides instead of failing at startup. This test fails on
// the old catch-and-swallow implementation.
test("agentOverridesFromEnv: malformed JSON throws a named, actionable error instead of silently returning {}", () => {
  withEnv("AIKONOS_AGENT_FOR_USER", "{not valid json", () => {
    assert.throws(
      () => agentOverridesFromEnv(),
      (err: unknown) => {
        assert.ok(err instanceof Error);
        assert.match(err.message, /AIKONOS_AGENT_FOR_USER/);
        return true;
      },
    );
  });
});

test("agentOverridesFromEnv: valid JSON that is not an object (e.g. an array) returns an empty map, not a throw", () => {
  withEnv("AIKONOS_AGENT_FOR_USER", "[1,2,3]", () => {
    assert.deepEqual(agentOverridesFromEnv(), {});
  });
});

test("agentOverridesFromEnv: non-string values in the override object are dropped", () => {
  withEnv("AIKONOS_AGENT_FOR_USER", '{"alice@example.com":"agent-a","bob@example.com":42}', () => {
    assert.deepEqual(agentOverridesFromEnv(), { "alice@example.com": "agent-a" });
  });
});

test("agentForUser: falls back to localpart-agent when no override matches", () => {
  assert.equal(agentForUser("carol@example.com", {}), "carol-agent");
});
