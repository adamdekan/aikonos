// Grep-contract test: repo-root
// contracts.json pins two string families a shared TypeScript symbol can't
// reach — the 4 gateway CUSTOM SSE event names (webui/web is plain JS, no
// npm-workspace link to agent-gateway) and the 3-language (Go/TS/bash) log
// strings. This test is the whole "codegen": every producer/consumer file
// listed must still literally contain its string, or CI fails before a
// reword ships. Deliberately no codegen pipeline — see design doc C9.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const __dirname = dirname(fileURLToPath(import.meta.url));
const REPO_ROOT = join(__dirname, "..", "..");

interface ContractEntry {
  value: string;
  family: string;
  producers: string[];
  consumers: string[];
}

interface Contracts {
  strings: ContractEntry[];
}

const contracts: Contracts = JSON.parse(
  readFileSync(join(REPO_ROOT, "contracts.json"), "utf8"),
);

test("contracts.json lists a producer and consumer for every string", () => {
  assert.ok(contracts.strings.length > 0, "contracts.json must list at least one string");
  for (const entry of contracts.strings) {
    assert.ok(entry.producers.length > 0, `${entry.value}: needs at least one producer file`);
    assert.ok(entry.consumers.length > 0, `${entry.value}: needs at least one consumer file`);
  }
});

test("every producer/consumer file listed in contracts.json still contains its string", () => {
  for (const entry of contracts.strings) {
    for (const relPath of [...entry.producers, ...entry.consumers]) {
      const contents = readFileSync(join(REPO_ROOT, relPath), "utf8");
      assert.ok(
        contents.includes(entry.value),
        `${relPath} no longer contains "${entry.value}" (family: ${entry.family}) — ` +
          `update contracts.json or restore the string`,
      );
    }
  }
});
