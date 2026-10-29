// Characters that change how text reads without being seen: C0/C1 controls
// (newline and tab stay, ArgsView shows them), bidi overrides and isolates,
// zero-width and invisible format characters, line and paragraph separators.
const HIDDEN = /[\u0000-\u0008\u000B-\u001F\u007F-\u009F\u00AD\u061C\u200B-\u200F\u2028-\u202E\u2060-\u2064\u2066-\u2069\uFEFF]/g;

/**
 * Replaces every hidden character in s with a visible \uXXXX escape, so an
 * approver reads the string the tool will receive, not the one a bidi
 * override draws (F213). Pure; exported for tests.
 */
export function revealHidden(s) {
  return String(s).replace(HIDDEN, (c) => `\\u${c.charCodeAt(0).toString(16).toUpperCase().padStart(4, "0")}`);
}
