import { describe, it, expect, beforeEach, vi } from "vitest";
import { mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { readFileSync } from "fs";
import { fileURLToPath } from "url";
import { dirname, resolve } from "path";
import { useUserStore } from "../store/user.js";
import { usePromptStore } from "../store/prompt.js";
import Home from "../views/Home.vue";
import Chat from "../views/Chat.vue";

vi.mock("vue-stream-markdown", () => ({
  Markdown: {
    props: ["content"],
    template: "<span>{{ content }}</span>",
  },
}));
vi.mock("vue-stream-markdown/index.css", () => ({}));

const __dirname = dirname(fileURLToPath(import.meta.url));
const homeVueSrc = readFileSync(resolve(__dirname, "../views/Home.vue"), "utf8");

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/", component: Home },
      { path: "/chat", component: Chat },
    ],
  });
}

describe("Home.vue", () => {
  let router;

  beforeEach(() => {
    setActivePinia(createPinia());
    router = makeRouter();
  });

  it("shows acting user display name in a serif element", async () => {
    const store = useUserStore();
    store.setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    const w = mount(Home, { global: { plugins: [router] } });
    // The greeting should contain "Alice"
    expect(w.text()).toContain("Alice");
    // The greeting element must use the serif font class/style
    const heading = w.find(".home-greeting");
    expect(heading.exists()).toBe(true);
  });

  it("home-greeting style references var(--font-serif) in source", () => {
    // jsdom cannot read scoped CSS computed styles, so we assert at the source level.
    expect(homeVueSrc).toContain("var(--font-serif)");
    // Ensure it is within the .home-greeting rule block.
    const greetingRuleMatch = homeVueSrc.match(/\.home-greeting\s*\{([^}]*)\}/s);
    expect(greetingRuleMatch).not.toBeNull();
    expect(greetingRuleMatch[1]).toContain("var(--font-serif)");
  });

  it("renders all five quick-action chips", () => {
    const w = mount(Home, { global: { plugins: [router] } });
    const chips = w.findAll(".chip");
    expect(chips.length).toBeGreaterThanOrEqual(5);
    const text = w.text();
    expect(text).toContain("Write");
    expect(text).toContain("Code");
  });

  it("submitting a non-empty prompt navigates to /chat", async () => {
    const w = mount(Home, { global: { plugins: [router] } });
    const textarea = w.find("textarea");
    await textarea.setValue("Hello agent");
    const form = w.find("form");
    await form.trigger("submit");
    await router.isReady();
    expect(router.currentRoute.value.path).toBe("/chat");
  });

  it("prompt is stored in promptStore after submit", async () => {
    const w = mount(Home, { global: { plugins: [router] } });
    const textarea = w.find("textarea");
    await textarea.setValue("My test prompt");
    await w.find("form").trigger("submit");
    const ps = usePromptStore();
    expect(ps.pending).toBe("My test prompt");
  });

  it("does not navigate on empty prompt", async () => {
    const w = mount(Home, { global: { plugins: [router] } });
    await router.push("/");
    await router.isReady();
    await w.find("form").trigger("submit");
    expect(router.currentRoute.value.path).toBe("/");
  });
});
