/**
 * Strip the first occurrence of `@<displayName>` from text.
 *
 * Composer inserts the token as `@${displayName} ` (trailing space). Removing it
 * mid-text could leave a double-space; this function collapses that and trims.
 * A stray `@` elsewhere in the content is left intact.
 *
 * @param {string} text        - Raw text containing the mention token.
 * @param {string} displayName - The display name used in the token (without `@`).
 * @returns {string}
 */
export function stripMention(text, displayName) {
  if (!displayName) return text.trim();
  // Escape any regex metacharacters in the display name.
  const escaped = displayName.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  // Match `@<displayName>` plus an optional single trailing space.
  // Replace with empty string (not a space) so mid-text removal doesn't widen the gap;
  // collapse only runs of 2+ spaces afterward — newlines are intentionally preserved.
  const re = new RegExp(`@${escaped} ?`);
  return text.replace(re, "").replace(/ {2,}/g, " ").trim();
}
