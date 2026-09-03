// Composer.vue — /command palette tests (CP9b).
// Tests the slash-command palette: discovery of granted skill bundles,
// prefix filtering, empty-state, and structured {skillName} submit.
// The palette is DISCOVERY ONLY — no client-side authz decisions.
import { describe, it, expect, beforeEach, vi } from "vitest";
import { mount } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import Composer from "../components/Composer.vue";
import { useDiscoveryStore } from "../store/discovery.js";

// Composer's onMounted unconditionally loads the workspace store (CP6,
// ) — mock it here so this suite doesn't leak
// a real (token-less) client.js call.
vi.mock("../api/workspace.js", () => ({
  getWorkspaceBackend: vi.fn().mockResolvedValue({
    pref: { backend: "local", onedriveFolderPath: "" },
    onedriveAvailable: false,
    onedriveStatus: "",
  }),
  setWorkspaceBackend: vi.fn(),
  listOneDriveFolders: vi.fn(),
}));

const BUNDLES = [
  { id: "id-1", name: "web-research", description: "Fetch and summarise web pages" },
  { id: "id-2", name: "doc-writer",   description: "Write documents to workspace" },
  { id: "id-3", name: "data-analyst", description: "Analyse data files" },
];

beforeEach(() => {
  setActivePinia(createPinia());
});

// Mount with a given modelValue. Composer uses :value (controlled) so the palette
// computed reads props.modelValue — we drive it via setProps to simulate parent updates.
// grantedBundles now lives on the shared discovery store rather than a prop.
function mountComposer({ grantedBundles = [], ...props } = {}) {
  useDiscoveryStore().grantedBundles = grantedBundles;
  return mount(Composer, {
    props: { modelValue: "", ...props },
    attachTo: document.body,
  });
}

async function typeValue(wrapper, text) {
  await wrapper.setProps({ modelValue: text });
}

describe("Composer.vue — /command palette", () => {
  it("palette is absent when grantedBundles is empty and user types /", async () => {
    const w = mountComposer({ grantedBundles: [] });
    await typeValue(w, "/");
    expect(w.find("[data-testid='skill-palette']").exists()).toBe(false);
  });

  it("typing / opens palette listing all granted bundle names", async () => {
    const w = mountComposer({ grantedBundles: BUNDLES });
    await typeValue(w, "/");
    const palette = w.find("[data-testid='skill-palette']");
    expect(palette.exists()).toBe(true);
    const items = w.findAll("[data-testid='palette-item']");
    expect(items.length).toBe(3);
    expect(items[0].text()).toContain("web-research");
    expect(items[1].text()).toContain("doc-writer");
    expect(items[2].text()).toContain("data-analyst");
  });

  it("palette shows description alongside name", async () => {
    const w = mountComposer({ grantedBundles: BUNDLES });
    await typeValue(w, "/");
    const first = w.find("[data-testid='palette-item']");
    expect(first.text()).toContain("Fetch and summarise web pages");
  });

  it("filters by prefix after the slash (case-insensitive)", async () => {
    const w = mountComposer({ grantedBundles: BUNDLES });
    await typeValue(w, "/doc");
    const items = w.findAll("[data-testid='palette-item']");
    expect(items.length).toBe(1);
    expect(items[0].text()).toContain("doc-writer");
  });

  it("prefix filter is case-insensitive", async () => {
    const w = mountComposer({ grantedBundles: BUNDLES });
    await typeValue(w, "/WEB");
    const items = w.findAll("[data-testid='palette-item']");
    expect(items.length).toBe(1);
    expect(items[0].text()).toContain("web-research");
  });

  it("palette closes when no bundle matches the typed prefix", async () => {
    const w = mountComposer({ grantedBundles: BUNDLES });
    await typeValue(w, "/zzz");
    expect(w.find("[data-testid='skill-palette']").exists()).toBe(false);
  });

  it("palette is absent when value does not start with /", async () => {
    const w = mountComposer({ grantedBundles: BUNDLES });
    await typeValue(w, "hello");
    expect(w.find("[data-testid='skill-palette']").exists()).toBe(false);
  });

  it("palette is absent when value is empty string", async () => {
    const w = mountComposer({ grantedBundles: BUNDLES });
    await typeValue(w, "");
    expect(w.find("[data-testid='skill-palette']").exists()).toBe(false);
  });

  it("selecting an entry emits submit with {skillName} (not raw text)", async () => {
    const w = mountComposer({ grantedBundles: BUNDLES });
    await typeValue(w, "/web");
    const item = w.find("[data-testid='palette-item']");
    expect(item.exists()).toBe(true);
    await item.trigger("mousedown");
    const emitted = w.emitted("submit");
    expect(emitted).toBeTruthy();
    expect(emitted.length).toBe(1);
    // Must carry the structured skillName, NOT the raw typed text.
    expect(emitted[0][0]).toEqual({ skillName: "web-research" });
  });

  it("selecting emits the exact bundle name (discovery-only: no body, no authz)", async () => {
    const w = mountComposer({ grantedBundles: BUNDLES });
    await typeValue(w, "/doc");
    await w.find("[data-testid='palette-item']").trigger("mousedown");
    const [[payload]] = w.emitted("submit");
    // Palette sends ONLY the name. Server resolves body + gates can_use.
    expect(Object.keys(payload)).toEqual(["skillName"]);
    expect(payload.skillName).toBe("doc-writer");
  });

  it("plain-text submit (no slash) emits submit with no skillName payload", async () => {
    const w = mountComposer({ grantedBundles: BUNDLES, modelValue: "hello agent" });
    await w.find("form").trigger("submit");
    const emitted = w.emitted("submit");
    expect(emitted).toBeTruthy();
    // Plain-text submit: emitted[0] has no payload arg.
    expect(emitted[0]).toEqual([]);
  });

  it("Enter plain-submits WITHOUT self-clearing the draft (parent reads then clears)", async () => {
    // Regression: onEnter previously emitted update:modelValue("") BEFORE submit.
    // emit() runs the parent listener synchronously, so the parent's bound draft
    // was emptied before its submit handler read it → plain-text messages were
    // lost (only /skill submits, which pass skillName explicitly, survived).
    const w = mountComposer({ grantedBundles: BUNDLES, modelValue: "hello agent" });
    const updatesBefore = (w.emitted("update:modelValue") ?? []).length;
    await w.find("textarea").trigger("keydown", { key: "Enter", shiftKey: false });
    // submit fired …
    expect(w.emitted("submit")).toBeTruthy();
    // … and Enter did NOT clear the draft itself (no update:modelValue from the keydown).
    const updatesAfter = (w.emitted("update:modelValue") ?? []).length;
    expect(updatesAfter).toBe(updatesBefore);
  });

  it("Enter with the palette open activates the highlighted skill, not a raw-text submit", async () => {
    // Regression: Enter used to plain-submit the raw "/name" text to the agent
    // as chat prose; only a mouse click activated the skill.
    const w = mountComposer({ grantedBundles: BUNDLES });
    await typeValue(w, "/web");
    expect(w.find("[data-testid='skill-palette']").exists()).toBe(true);
    await w.find("textarea").trigger("keydown", { key: "Enter", shiftKey: false });
    const [[payload]] = w.emitted("submit");
    expect(payload).toEqual({ skillName: "web-research" });
  });

  it("ArrowDown moves the palette highlight and Enter selects the highlighted entry", async () => {
    const w = mountComposer({ grantedBundles: BUNDLES });
    await typeValue(w, "/");
    const textarea = w.find("textarea");
    await textarea.trigger("keydown", { key: "ArrowDown" });
    const items = w.findAll("[data-testid='palette-item']");
    expect(items[1].classes()).toContain("palette-item--highlighted");
    await textarea.trigger("keydown", { key: "Enter", shiftKey: false });
    const [[payload]] = w.emitted("submit");
    expect(payload).toEqual({ skillName: "doc-writer" });
  });

  it("form submit of an exact '/name' draft activates the skill even without the palette path", async () => {
    // Covers the Send-button path: doSubmit resolves an exact granted-bundle
    // name so raw slash text never reaches the agent as a chat message.
    const w = mountComposer({ grantedBundles: BUNDLES, modelValue: "/data-analyst" });
    await w.find("form").trigger("submit");
    const [[payload]] = w.emitted("submit");
    expect(payload).toEqual({ skillName: "data-analyst" });
  });

  it("selecting a personal skill emits the qualified skillName, not the bare display name", async () => {
    const personal = {
      id: "personal:mine", name: "mine", description: "my skill",
      personal: true, skillName: "personal:mine",
    };
    const w = mountComposer({ grantedBundles: [...BUNDLES, personal] });
    await typeValue(w, "/mine");
    const item = w.find("[data-testid='palette-item']");
    expect(item.text()).toContain("personal");
    await item.trigger("mousedown");
    const [[payload]] = w.emitted("submit");
    expect(payload).toEqual({ skillName: "personal:mine" });
  });
});
