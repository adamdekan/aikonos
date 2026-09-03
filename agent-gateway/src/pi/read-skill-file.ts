// read_skill_file — the second progressive-disclosure stage for skills
//. load_skill's activation manifest tells
// the model which files exist under a skill's tree; this tool reads one of
// them on demand.
//
// WHY resolution is byNameAll, not byName (unlike load_skill's model-facing
// map): the model may be reading files of a skill the PARENT activated via
// /command — including a disable_model_invocation bundle, which /command
// (resolveCommandSkill) honors but load_skill's own execute() refuses (the
// /command lesson — commits 26d549b/30c6f11). Excluding DMI entries here would
// leave the model unable to read files of a skill it was told (via the
// injected manifest) that it has access to.
//
// WHY the content gate lives here, not in GovernanceBridge.getSkillFile: a
// skill file may be binary (scripts/, assets/) — the broker and the IPC layer
// both carry the file's bytes (base64-encoded over the wire — see
// GetSkillFileResult) so nothing is lost in transit. This tool is the single
// point both the forked-child (IPC round-trip) and legacy in-process (direct
// GovernanceBridge call) paths go through, so gating here — never raw bytes to
// the model — applies identically regardless of which bridge implementation
// is behind it.
import type { SkillBundleEntry } from "../ipc/protocol.js";
import { isValidSkillFilePath } from "./skill-parser.js";

// Mirrors 's per-file read cap
// (personalskill.MaxSkillFileBytes) — enforced again here so the cap holds
// uniformly for both personal and admin-bundle origins regardless of what the
// broker itself enforces server-side.
const MAX_SKILL_FILE_READ_BYTES = 256 * 1024;

function textResult(text: string) {
  return { content: [{ type: "text" as const, text }], details: {} };
}

// SkillFileFetcher performs the on-demand single-file read. ref is either a
// bundle UUID or a "personal:<name>"-qualified id (GovernanceBridge.getSkillFile
// switches on the prefix); contentB64 is the file's bytes, base64-encoded (JSON-
// IPC-safe — see GetSkillFileResult) — the UTF-8/size gate below runs on the
// decoded bytes. Structurally satisfied by GovernanceBridge.getSkillFile and
// RemoteBridgeClient.getSkillFile — bind() the method before passing it in so
// `this` resolves correctly.
export interface SkillFileFetcher {
  (ref: string, path: string): Promise<{ ok: boolean; contentB64?: string; error?: string }>;
}

export interface ReadSkillFileToolDef {
  name: "read_skill_file";
  execute(
    toolCallId: string,
    params: { skill: string; path: string },
  ): Promise<{ content: { type: "text"; text: string }[]; details: unknown }>;
}

// makeReadSkillFileTool builds the read_skill_file tool bound to the
// session's FULL authorized catalog (bundles ∪ personal, DMI included) and a
// fetcher performing the actual IPC round-trip.
export function makeReadSkillFileTool(
  bundles: SkillBundleEntry[],
  fetchFile: SkillFileFetcher,
): ReadSkillFileToolDef {
  const byNameAll = new Map<string, SkillBundleEntry>();
  for (const b of bundles) byNameAll.set(b.name, b);

  return {
    name: "read_skill_file",
    async execute(_toolCallId, params) {
      const entry = byNameAll.get(params.skill);
      if (!entry) {
        return textResult(`ERROR: unknown skill "${params.skill}"`);
      }
      if (!isValidSkillFilePath(params.path)) {
        return textResult(`ERROR: invalid path "${params.path}"`);
      }

      const result = await fetchFile(entry.id, params.path);
      if (!result.ok || result.contentB64 === undefined) {
        return textResult(`ERROR: ${result.error ?? "read failed"}`);
      }

      const content = Buffer.from(result.contentB64, "base64");
      if (content.byteLength > MAX_SKILL_FILE_READ_BYTES) {
        return textResult(`ERROR: file exceeds the ${MAX_SKILL_FILE_READ_BYTES}-byte read cap`);
      }

      let text: string;
      try {
        text = new TextDecoder("utf-8", { fatal: true }).decode(content);
      } catch {
        return textResult("ERROR: file is not valid UTF-8 text (binary content is not readable here)");
      }

      return textResult(text);
    },
  };
}
