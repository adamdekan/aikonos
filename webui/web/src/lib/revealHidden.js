// Characters that change how text reads without being seen: controls and
// format characters (Cc, Cf: C0/C1, bidi overrides and isolates, zero-width
// characters, Unicode tag characters), line and paragraph separators, Hangul
// fillers, the combining grapheme joiner and variation selectors. Newline and
// tab stay, ArgsView shows them.
const HIDDEN = /(?![\n\t])[\p{Cc}\p{Cf}\p{Zl}\p{Zp}\u115F\u1160\u3164\uFFA0\u034F\uFE00-\uFE0F\u{E0100}-\u{E01EF}]/gu;

/**
 * Replaces every hidden character in s with a visible escape (\uXXXX, or
 * \u{XXXXX} above the BMP), so an approver reads the string the tool will
 * receive, not the one a bidi override draws (F213). Pure; exported for tests.
 */
export function revealHidden(s) {
  return String(s).replace(HIDDEN, (c) => {
    const hex = c.codePointAt(0).toString(16).toUpperCase();
    return hex.length > 4 ? `\\u{${hex}}` : `\\u${hex.padStart(4, "0")}`;
  });
}
