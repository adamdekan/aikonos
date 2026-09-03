import { readFileSync } from "fs";
import { resolve, dirname } from "path";
import { fileURLToPath } from "url";
import { describe, it, expect } from "vitest";

const __dir = dirname(fileURLToPath(import.meta.url));
const css = readFileSync(resolve(__dir, "../styles/tokens.css"), "utf8");

const REQUIRED_TOKENS = [
  "--bg",
  "--bg-sidebar",
  "--bg-elevated",
  "--bg-hover",
  "--bg-active",
  "--text",
  "--text-muted",
  "--text-faint",
  "--text-on-accent",
  "--border",
  "--accent",
  "--accent-hover",
  "--danger",
  "--ok",
  "--radius",
  "--radius-sm",
  "--font-sans",
  "--font-serif",
  "--font-mono",
  "--overlay",
  "--fill-muted",
  "--fill-accent",
  "--fill-danger",
];

describe("tokens.css", () => {
  it.each(REQUIRED_TOKENS)("defines %s", (token) => {
    expect(css).toContain(token + ":");
  });
});
