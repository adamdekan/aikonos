import { describe, it, expect } from "vitest";
import { mount } from "@vue/test-utils";
import ArgsView from "../components/ArgsView.vue";
import { revealHidden } from "../lib/revealHidden.js";

// F213: the approver must read the argument the tool will get, not what a
// bidi override or a zero-width character makes it look like.
describe("ArgsView hidden characters", () => {
  it("revealHidden escapes bidi, zero-width and control characters but keeps newline and tab", () => {
    expect(revealHidden("a\u202Eb\u2066c\u200Bd\u0007e\rf")).toBe("a\\u202Eb\\u2066c\\u200Bd\\u0007e\\u000Df");
    expect(revealHidden("line1\nline2\tx")).toBe("line1\nline2\tx");
    expect(revealHidden(42)).toBe("42");
  });

  it("revealHidden escapes invisible code points outside the BMP list too", () => {
    // Unicode tag characters carry ASCII a model reads but a person does not see.
    const tagged = "ok" + [..."attack"].map((c) => String.fromCodePoint(0xe0000 + c.charCodeAt(0))).join("");
    expect(revealHidden(tagged)).toBe("ok\\u{E0061}\\u{E0074}\\u{E0074}\\u{E0061}\\u{E0063}\\u{E006B}");
    expect(revealHidden("a\u3164b\u180Ec\u206Ad\uFE0Fe\u034Ff\u{E0100}g")).toBe(
      "a\\u3164b\\u180Ec\\u206Ad\\uFE0Fe\\u034Ff\\u{E0100}g",
    );
    expect(revealHidden("héllo € 😀")).toBe("héllo € 😀");
    const w = mount(ArgsView, { props: { args: { body: "ok\u{E0061}" } } });
    expect(w.find(".arg-hidden").exists()).toBe(true);
  });

  it("shows an override in a value as an escape with a marker", () => {
    const w = mount(ArgsView, { props: { args: { to: "ceo@corp.example\u202E\u2066moc.live@attacker\u2069", subject: "Weekly report" } } });
    const rows = w.findAll(".arg-row");
    expect(rows[0].text()).not.toContain("\u202E");
    expect(rows[0].text()).toContain("\\u202E");
    expect(rows[0].find(".arg-hidden").exists()).toBe(true);
    expect(rows[1].find(".arg-hidden").exists()).toBe(false);
  });

  it("reveals hidden characters in keys and in nested and depth-capped values", () => {
    const deep = { a: { b: { c: { d: { e: "x\u202Ey" } } } } };
    const w = mount(ArgsView, { props: { args: { "to\u200B": "x", nested: { k: "v\u2067" }, deep } } });
    expect(w.text()).not.toMatch(/[\u200B\u2067\u202E]/);
    expect(w.text()).toContain("to\\u200B");
    expect(w.text()).toContain("v\\u2067");
    expect(w.text()).toContain("x\\u202Ey");
  });
});
