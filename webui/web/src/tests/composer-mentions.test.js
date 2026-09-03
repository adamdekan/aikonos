// Composer.vue — @user and #file mention autocomplete tests (CP4).
// Tests trigger detection, palette rendering, item selection, keyboard
// interception, and that existing Enter-submit is unaffected when no mention is open.
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

const USERS = [
  { userId: "alice@example.com", displayName: "Alice Smith" },
  { userId: "bob@example.com",   displayName: "Bob Jones" },
];

const GROUPS = [
  { groupId: "group:eng",     displayName: "Engineering", memberCount: 5 },
  { groupId: "group:support", displayName: "Support",     memberCount: 3 },
];

const FILES = [
  { path: "reports/q1.md" },
  { path: "notes/budget.txt" },
];

beforeEach(() => {
  setActivePinia(createPinia());
});

// grantedBundles/delegatableUsers/delegatableGroups/files now live on the
// shared discovery store rather than as props.
function mountComposer({ grantedBundles = [], delegatableUsers = USERS, delegatableGroups = GROUPS, files = FILES, ...props } = {}) {
  const store = useDiscoveryStore();
  store.grantedBundles = grantedBundles;
  store.delegatableUsers = delegatableUsers;
  store.delegatableGroups = delegatableGroups;
  store.mentionFiles = files;
  return mount(Composer, {
    props: { modelValue: "", ...props },
    attachTo: document.body,
  });
}

// Simulate real textarea input: set element value + selectionStart, then fire the input event.
// This is necessary because jsdom doesn't update selectionStart on prop changes alone.
async function typeIntoTextarea(wrapper, text) {
  const ta = wrapper.find("textarea").element;
  ta.value = text;
  ta.selectionStart = text.length;
  ta.selectionEnd = text.length;
  await wrapper.find("textarea").trigger("input");
}

describe("Composer.vue — @user and #file mention palette", () => {
  // Intent: typing @b shows users whose displayName or userId contains "b"
  it("@b with a matching user shows the mention palette with that user", async () => {
    const w = mountComposer();
    await typeIntoTextarea(w, "@b");
    const palette = w.find("[data-testid='mention-palette']");
    expect(palette.exists()).toBe(true);
    const items = w.findAll("[data-testid='mention-item']");
    // Bob Jones contains "b"; Alice Smith does not contain "b" in name or userId
    expect(items.length).toBe(1);
    expect(items[0].text()).toContain("Bob Jones");
  });

  // Intent: selecting a user inserts "@<displayName> " into update:modelValue and closes the palette
  it("selecting a user inserts @<displayName> into update:modelValue and closes palette", async () => {
    const w = mountComposer();
    await typeIntoTextarea(w, "@b");
    const item = w.find("[data-testid='mention-item']");
    expect(item.exists()).toBe(true);
    await item.trigger("mousedown");
    const emitted = w.emitted("update:modelValue");
    expect(emitted).toBeTruthy();
    const last = emitted[emitted.length - 1][0];
    expect(last).toBe("@Bob Jones ");
    // Palette should be gone after selection
    expect(w.find("[data-testid='mention-palette']").exists()).toBe(false);
  });

  // Intent: Enter while the mention palette is open must NOT emit submit (it selects the item instead)
  it("Enter while palette open does not emit submit", async () => {
    const w = mountComposer();
    await typeIntoTextarea(w, "@b");
    expect(w.find("[data-testid='mention-palette']").exists()).toBe(true);
    await w.find("textarea").trigger("keydown", { key: "Enter", shiftKey: false });
    expect(w.emitted("submit")).toBeFalsy();
  });

  // Intent: # with a matching file shows it in the palette and selection inserts "#<path> "
  it("# with a matching file shows it and selection inserts #<path>", async () => {
    const w = mountComposer();
    await typeIntoTextarea(w, "#q1");
    const palette = w.find("[data-testid='mention-palette']");
    expect(palette.exists()).toBe(true);
    const items = w.findAll("[data-testid='mention-item']");
    expect(items.length).toBe(1);
    expect(items[0].text()).toContain("reports/q1.md");

    await items[0].trigger("mousedown");
    const emitted = w.emitted("update:modelValue");
    const last = emitted[emitted.length - 1][0];
    expect(last).toBe("#reports/q1.md ");
    expect(w.find("[data-testid='mention-palette']").exists()).toBe(false);
  });

  // Intent: whitespace after the trigger char closes the palette (space terminates the token)
  it("whitespace after the trigger closes the palette", async () => {
    const w = mountComposer();
    await typeIntoTextarea(w, "@ ");
    expect(w.find("[data-testid='mention-palette']").exists()).toBe(false);
  });

  // Intent (F39): the mention palette is exposed to assistive tech as a listbox,
  // with the highlighted item wired via aria-activedescendant/aria-selected.
  it("ARIA: palette is a listbox, options carry aria-selected, textarea tracks activedescendant", async () => {
    const w = mountComposer();
    const textarea = w.find("textarea");
    expect(textarea.attributes("aria-expanded")).toBe("false");

    await typeIntoTextarea(w, "@a");
    const palette = w.find("[data-testid='mention-palette']");
    expect(palette.attributes("role")).toBe("listbox");
    expect(palette.attributes("id")).toBeTruthy();

    const items = w.findAll("[data-testid='mention-item']");
    // Both Alice and Bob match "a" (Bob Jones's userId is bob@example.com, no "a"... use displayName)
    expect(items.length).toBeGreaterThan(0);
    items.forEach((item) => expect(item.attributes("role")).toBe("option"));

    expect(textarea.attributes("aria-expanded")).toBe("true");
    expect(textarea.attributes("aria-controls")).toBe(palette.attributes("id"));

    // First item (index 0) is highlighted by default.
    expect(items[0].attributes("aria-selected")).toBe("true");
    expect(textarea.attributes("aria-activedescendant")).toBe(items[0].attributes("id"));

    // ArrowDown moves the highlight and activedescendant follows.
    await textarea.trigger("keydown", { key: "ArrowDown" });
    expect(items[0].attributes("aria-selected")).toBe("false");
    if (items.length > 1) {
      expect(items[1].attributes("aria-selected")).toBe("true");
      expect(textarea.attributes("aria-activedescendant")).toBe(items[1].attributes("id"));
    }
  });

  // Intent: with no trigger present, Enter still emits submit (existing behavior unaffected)
  it("no trigger present — Enter emits submit as normal", async () => {
    const w = mountComposer({ modelValue: "hello agent" });
    // Ensure no mention palette is active
    const ta = w.find("textarea").element;
    ta.value = "hello agent";
    ta.selectionStart = 11;
    ta.selectionEnd = 11;
    await w.find("textarea").trigger("input");
    expect(w.find("[data-testid='mention-palette']").exists()).toBe(false);
    await w.find("textarea").trigger("keydown", { key: "Enter", shiftKey: false });
    expect(w.emitted("submit")).toBeTruthy();
  });
});

describe("Composer.vue — CP1 delegation target capture", () => {
  // Intent: selecting @peer then submitting via Enter emits submit with delegateTo
  it("selecting @Bob then Enter emits submit with delegateTo { userId, displayName }", async () => {
    const w = mountComposer();
    // Trigger the @ palette for Bob
    await typeIntoTextarea(w, "@Bob");
    const item = w.find("[data-testid='mention-item']");
    expect(item.exists()).toBe(true);
    // Select Bob from the palette
    await item.trigger("mousedown");
    // Now textarea should have "@Bob Jones " — simulate typing that value and submitting
    const ta = w.find("textarea").element;
    ta.value = "@Bob Jones ";
    ta.selectionStart = 11;
    ta.selectionEnd = 11;
    await w.find("textarea").trigger("input");
    // Submit via Enter
    await w.find("textarea").trigger("keydown", { key: "Enter", shiftKey: false });
    const emitted = w.emitted("submit");
    expect(emitted).toBeTruthy();
    const lastPayload = emitted[emitted.length - 1][0];
    expect(lastPayload).toEqual({ delegateTo: { userId: "bob@example.com", displayName: "Bob Jones" } });
  });

  // Intent: selecting @Bob then submitting via Send button emits submit with delegateTo
  it("selecting @Bob then clicking Send emits submit with delegateTo", async () => {
    const w = mountComposer();
    await typeIntoTextarea(w, "@Bob");
    const item = w.find("[data-testid='mention-item']");
    await item.trigger("mousedown");
    const ta = w.find("textarea").element;
    ta.value = "@Bob Jones ";
    ta.selectionStart = 11;
    ta.selectionEnd = 11;
    await w.find("textarea").trigger("input");
    // Submit via form submit (Send button)
    await w.find("form").trigger("submit");
    const emitted = w.emitted("submit");
    expect(emitted).toBeTruthy();
    const lastPayload = emitted[emitted.length - 1][0];
    expect(lastPayload).toEqual({ delegateTo: { userId: "bob@example.com", displayName: "Bob Jones" } });
  });

  // Intent: submitting plain text with no @selection emits submit with no payload
  it("submitting plain text (no @ selection) emits submit with no payload", async () => {
    const w = mountComposer();
    const ta = w.find("textarea").element;
    ta.value = "hello agent";
    ta.selectionStart = 11;
    ta.selectionEnd = 11;
    await w.find("textarea").trigger("input");
    await w.find("textarea").trigger("keydown", { key: "Enter", shiftKey: false });
    const emitted = w.emitted("submit");
    expect(emitted).toBeTruthy();
    // Payload must be undefined (no delegateTo)
    expect(emitted[emitted.length - 1][0]).toBeUndefined();
  });

  // Intent: selecting @Bob then deleting the @Bob Jones text before submit emits no delegateTo
  it("selecting @Bob then deleting the @<displayName> text emits submit with no delegateTo", async () => {
    const w = mountComposer();
    await typeIntoTextarea(w, "@Bob");
    const item = w.find("[data-testid='mention-item']");
    await item.trigger("mousedown");
    // Now delete the mention text — just send plain text without @Bob Jones
    const ta = w.find("textarea").element;
    ta.value = "some other text";
    ta.selectionStart = 15;
    ta.selectionEnd = 15;
    await w.find("textarea").trigger("input");
    await w.find("textarea").trigger("keydown", { key: "Enter", shiftKey: false });
    const emitted = w.emitted("submit");
    expect(emitted).toBeTruthy();
    expect(emitted[emitted.length - 1][0]).toBeUndefined();
  });

  // Intent: selecting a #file does NOT produce a delegateTo on submit
  it("selecting a #file does not produce delegateTo on submit", async () => {
    const w = mountComposer();
    await typeIntoTextarea(w, "#q1");
    const item = w.find("[data-testid='mention-item']");
    expect(item.exists()).toBe(true);
    await item.trigger("mousedown");
    const ta = w.find("textarea").element;
    ta.value = "#reports/q1.md some task";
    ta.selectionStart = 24;
    ta.selectionEnd = 24;
    await w.find("textarea").trigger("input");
    await w.find("textarea").trigger("keydown", { key: "Enter", shiftKey: false });
    const emitted = w.emitted("submit");
    expect(emitted).toBeTruthy();
    expect(emitted[emitted.length - 1][0]).toBeUndefined();
  });

  // Intent: /skill path is unaffected by the delegation logic
  it("/skill palette submit path is byte-for-byte unchanged", async () => {
    const w = mountComposer({
      grantedBundles: [{ id: "1", name: "search", description: "Web search" }],
      modelValue: "/search",
    });
    // Palette is driven by props.modelValue for slashPrefix, so just await next tick
    await w.vm.$nextTick();
    const paletteItem = w.find("[data-testid='palette-item']");
    expect(paletteItem.exists()).toBe(true);
    await paletteItem.trigger("mousedown");
    const emitted = w.emitted("submit");
    expect(emitted).toBeTruthy();
    expect(emitted[emitted.length - 1][0]).toEqual({ skillName: "search" });
  });

  // Intent: selecting a @group emits delegateTo with groupId + memberCount (no userId)
  it("selecting @Engineering group then Enter emits submit with delegateTo { groupId, displayName, memberCount }", async () => {
    const w = mountComposer();
    await typeIntoTextarea(w, "@Eng");
    const items = w.findAll("[data-testid='mention-item']");
    const groupItem = items.find(i => i.text().includes("Engineering"));
    expect(groupItem).toBeTruthy();
    await groupItem.trigger("mousedown");

    const ta = w.find("textarea").element;
    ta.value = "@Engineering ";
    ta.selectionStart = 13;
    ta.selectionEnd = 13;
    await w.find("textarea").trigger("input");

    await w.find("textarea").trigger("keydown", { key: "Enter", shiftKey: false });
    const emitted = w.emitted("submit");
    expect(emitted).toBeTruthy();
    const lastPayload = emitted[emitted.length - 1][0];
    expect(lastPayload).toEqual({
      delegateTo: { groupId: "group:eng", displayName: "Engineering", memberCount: 5 },
    });
  });

  // Intent: a user mention still emits userId (group path must not affect user path)
  it("user mention still emits userId not groupId after groups are added", async () => {
    const w = mountComposer();
    await typeIntoTextarea(w, "@Bob");
    const item = w.find("[data-testid='mention-item']");
    expect(item.exists()).toBe(true);
    await item.trigger("mousedown");
    const ta = w.find("textarea").element;
    ta.value = "@Bob Jones ";
    ta.selectionStart = 11;
    ta.selectionEnd = 11;
    await w.find("textarea").trigger("input");
    await w.find("textarea").trigger("keydown", { key: "Enter", shiftKey: false });
    const emitted = w.emitted("submit");
    expect(emitted).toBeTruthy();
    const lastPayload = emitted[emitted.length - 1][0];
    expect(lastPayload).toEqual({ delegateTo: { userId: "bob@example.com", displayName: "Bob Jones" } });
  });

  // Intent: deleting the @<groupDisplayName> text before submit produces no delegateTo
  it("selecting @Engineering then deleting the text before submit emits no delegateTo", async () => {
    const w = mountComposer();
    await typeIntoTextarea(w, "@Eng");
    const items = w.findAll("[data-testid='mention-item']");
    const groupItem = items.find(i => i.text().includes("Engineering"));
    expect(groupItem).toBeTruthy();
    await groupItem.trigger("mousedown");

    const ta = w.find("textarea").element;
    ta.value = "some unrelated text";
    ta.selectionStart = 19;
    ta.selectionEnd = 19;
    await w.find("textarea").trigger("input");

    await w.find("textarea").trigger("keydown", { key: "Enter", shiftKey: false });
    const emitted = w.emitted("submit");
    expect(emitted).toBeTruthy();
    expect(emitted[emitted.length - 1][0]).toBeUndefined();
  });

  // Intent: when two peers are selected but one's @<displayName> is deleted before submit,
  // delegationTarget resolves the surviving peer — not the deleted one.
  it("multi-peer: deleting @Bob text resolves surviving @Alice as delegateTo", async () => {
    const w = mountComposer();

    // Select @Bob
    await typeIntoTextarea(w, "@Bob");
    const bobItem = w.findAll("[data-testid='mention-item']").find(i => i.text().includes("Bob"));
    expect(bobItem.exists()).toBe(true);
    await bobItem.trigger("mousedown");

    // Now trigger @Alice — type "@Al" into the textarea
    const ta = w.find("textarea").element;
    ta.value = "@Bob Jones @Al";
    ta.selectionStart = 14;
    ta.selectionEnd = 14;
    await w.find("textarea").trigger("input");

    const aliceItem = w.findAll("[data-testid='mention-item']").find(i => i.text().includes("Alice"));
    expect(aliceItem.exists()).toBe(true);
    await aliceItem.trigger("mousedown");

    // Delete Bob's text — keep only Alice's mention
    ta.value = "@Alice Smith some task";
    ta.selectionStart = 22;
    ta.selectionEnd = 22;
    await w.find("textarea").trigger("input");

    await w.find("textarea").trigger("keydown", { key: "Enter", shiftKey: false });
    const emitted = w.emitted("submit");
    expect(emitted).toBeTruthy();
    expect(emitted[emitted.length - 1][0]).toEqual({
      delegateTo: { userId: "alice@example.com", displayName: "Alice Smith" },
    });
  });
});
