// Tests for resolveInvocationArgs — the env/argv precedence resolver smoke-south.ts
// uses to pick its tool id / args JSON / effect class.
//
// WHY this exists: compose-verify.sh drives smoke-south.ts via
// `docker compose exec ... npm run --silent smoke -- ...`; argv forwarded
// through npm's `--` doesn't reliably survive that hop (confirmed live — the
// docx.create check silently ran the workspace_read default instead). Env
// vars are now the primary channel; this pins env > argv > default so a
// future edit can't silently regress back to the argv-only path that broke.
import { test } from "node:test";
import assert from "node:assert/strict";
import { resolveInvocationArgs } from "../src/cli/smoke-south.js";
import { EffectClass, effectClassFromJSON } from "../gen/ts/proto/plan.js";

test("resolveInvocationArgs: no env, no argv → defaults", () => {
  assert.deepEqual(resolveInvocationArgs([], {}), {
    toolArg: "workspace_read",
    argsJson: "{}",
    effectClassArg: undefined,
  });
});

test("resolveInvocationArgs: argv used when env is unset", () => {
  const argv = ["node", "smoke-south.ts", "doc.read", '{"path":"x"}', "READ_ONLY"];
  assert.deepEqual(resolveInvocationArgs(argv, {}), {
    toolArg: "doc.read",
    argsJson: '{"path":"x"}',
    effectClassArg: "READ_ONLY",
  });
});

test("resolveInvocationArgs: env wins over argv when both are set", () => {
  const argv = ["node", "smoke-south.ts", "workspace_read", "{}", undefined as unknown as string];
  const env = { SMOKE_TOOL_ID: "docx.create", SMOKE_ARGS: '{"output_path":"o.docx"}', SMOKE_EFFECT_CLASS: "WRITE_LOCAL" };
  assert.deepEqual(resolveInvocationArgs(argv, env), {
    toolArg: "docx.create",
    argsJson: '{"output_path":"o.docx"}',
    effectClassArg: "WRITE_LOCAL",
  });
});

test("resolveInvocationArgs: a raw tool id with SMOKE_EFFECT_CLASS=WRITE_LOCAL resolves for docx.create", () => {
  const { toolArg, effectClassArg } = resolveInvocationArgs([], {
    SMOKE_TOOL_ID: "docx.create",
    SMOKE_EFFECT_CLASS: "WRITE_LOCAL",
  });
  assert.equal(toolArg, "docx.create");
  assert.equal(effectClassArg, "WRITE_LOCAL");
  assert.equal(effectClassFromJSON(effectClassArg), EffectClass.WRITE_LOCAL);
});
