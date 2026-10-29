// Tests for views/admin/ScimSettings.vue and its place in Provisioning.vue (AG-16).
import { describe, it, expect, vi, beforeEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

vi.mock("../api/client.js", () => ({
  get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn(), patch: vi.fn(), upload: vi.fn(),
}));

import ScimSettings from "../views/admin/ScimSettings.vue";
import Provisioning from "../views/admin/Provisioning.vue";
import * as client from "../api/client.js";

const TOKEN = { id: "t1", prefix: "scim_abc", createdBy: "admin", createdAt: "2026-10-01T00:00:00Z", lastUsedAt: null, expiresAt: "2027-10-01T00:00:00Z" };

function mockGet(scim) {
  client.get.mockImplementation((url) =>
    Promise.resolve(url === "/admin/scim" ? scim : { rules: [] }));
}

describe("ScimSettings.vue", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.spyOn(window, "confirm").mockReturnValue(true);
  });

  it("shows mode, the configured SCIM URL and token prefixes, never a raw token", async () => {
    mockGet({ mode: "scim", baseUrl: "https://gw.example.com:8090/scim/v2", tokens: [TOKEN] });
    const w = mount(ScimSettings);
    await flushPromises();
    expect(w.find("[data-testid='scim-mode-select']").element.value).toBe("scim");
    expect(w.find("[data-testid='scim-base-url']").element.value).toBe("https://gw.example.com:8090/scim/v2");
    const rows = w.findAll("[data-testid='scim-token-row']");
    expect(rows.length).toBe(1);
    expect(rows[0].text()).toContain("scim_abc");
    // F13: the admin sees when the token stops working.
    expect(rows[0].find("[data-testid='scim-token-expires']").text()).toBe(new Date(TOKEN.expiresAt).toLocaleString());
    expect(w.find("[data-testid='scim-raw-token']").exists()).toBe(false);
  });

  it("creates a token and shows it once until dismissed", async () => {
    mockGet({ mode: "jit", baseUrl: "", tokens: [] });
    client.post.mockResolvedValue({ token: TOKEN, rawToken: "scim_SECRETVALUE" });
    const w = mount(ScimSettings);
    await flushPromises();
    expect(w.find("[data-testid='scim-base-url']").element.value).toMatch(/:8090\/scim\/v2$/);
    await w.find("[data-testid='scim-create-token']").trigger("click");
    await flushPromises();
    expect(client.post).toHaveBeenCalledWith("/admin/scim/tokens", { body: { replacesId: "" } });
    expect(w.find("[data-testid='scim-raw-token']").text()).toBe("scim_SECRETVALUE");
    await w.find("[data-testid='scim-raw-dismiss']").trigger("click");
    expect(w.find("[data-testid='scim-raw-token']").exists()).toBe(false);
  });

  it("rotates and revokes a token", async () => {
    mockGet({ mode: "scim", baseUrl: "", tokens: [TOKEN] });
    client.post.mockResolvedValue({ token: { ...TOKEN, id: "t2" }, rawToken: "scim_NEW" });
    const w = mount(ScimSettings);
    await flushPromises();
    await w.find("[data-testid='scim-rotate-t1']").trigger("click");
    await flushPromises();
    expect(client.post).toHaveBeenCalledWith("/admin/scim/tokens", { body: { replacesId: "t1" } });
    client.post.mockResolvedValue({});
    await w.find("[data-testid='scim-revoke-t1']").trigger("click");
    await flushPromises();
    expect(client.post).toHaveBeenCalledWith("/admin/scim/tokens/revoke", { body: { id: "t1" } });
  });

  it("switching mode saves provisioning.mode", async () => {
    mockGet({ mode: "jit", baseUrl: "", tokens: [] });
    client.put.mockResolvedValue({});
    const w = mount(ScimSettings);
    await flushPromises();
    await w.find("[data-testid='scim-mode-select']").setValue("scim");
    await flushPromises();
    expect(client.put).toHaveBeenCalledWith("/admin/config", { body: { key: "provisioning.mode", value: "scim" } });
    expect(w.emitted("mode").at(-1)).toEqual(["scim"]);
  });
});

describe("Provisioning.vue under scim", () => {
  beforeEach(() => vi.clearAllMocks());

  it("turns the email-rule form off", async () => {
    mockGet({ mode: "scim", baseUrl: "", tokens: [] });
    const w = mount(Provisioning);
    await flushPromises();
    expect(w.find("[data-testid='rules-off-notice']").exists()).toBe(true);
    expect(w.find("[data-testid='add-rule-btn']").element.disabled).toBe(true);
  });
});
