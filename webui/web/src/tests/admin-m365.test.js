// Tests for views/admin/M365Connection.vue — tenant M365 OBO admin panel
//.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/admin.js", () => ({
  getM365Connection:    vi.fn(),
  upsertM365Connection: vi.fn(),
  deleteM365Connection: vi.fn(),
  testM365Connection:   vi.fn(),
}));

import M365Connection from "../views/admin/M365Connection.vue";
import * as adminApi from "../api/admin.js";
import { useToast } from "../components/ui/useToast.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/m365", component: M365Connection },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

async function mountView() {
  const router = makeRouter();
  await router.push("/admin/m365");
  const w = mount(M365Connection, { global: { plugins: [router] }, attachTo: document.body });
  await flushPromises();
  return w;
}

function q(testid) {
  return document.querySelector(`[data-testid='${testid}']`);
}

function setField(testid, value, evt = "input") {
  const el = q(testid);
  el.value = value;
  el.dispatchEvent(new Event(evt));
}

const CONFIGURED = {
  entraTenantId: "11111111-1111-1111-1111-111111111111",
  clientId: "22222222-2222-2222-2222-222222222222",
  hasSecret: true,
  enabled: true,
  updatedBy: "admin@example.com",
  updatedAt: "2026-07-10T00:00:00Z",
};

describe("M365Connection.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
    const { toasts } = useToast();
    toasts.splice(0);
  });
  afterEach(() => {
    document.body.innerHTML = "";
    vi.restoreAllMocks();
  });

  it("loads and renders the stored connection", async () => {
    adminApi.getM365Connection.mockResolvedValue({ connection: CONFIGURED });
    await mountView();

    expect(adminApi.getM365Connection).toHaveBeenCalledTimes(1);
    expect(q("m365-tenant-id").value).toBe(CONFIGURED.entraTenantId);
    expect(q("m365-client-id").value).toBe(CONFIGURED.clientId);
    // The secret is never pre-filled — has_secret only drives the placeholder.
    expect(q("m365-secret").value).toBe("");
    expect(q("m365-secret").placeholder).toContain("unchanged");
  });

  it("renders the forbidden empty-state for a non-admin caller", async () => {
    adminApi.getM365Connection.mockResolvedValue({ forbidden: true });
    await mountView();

    expect(q("forbidden")).not.toBeNull();
    expect(q("m365-save-btn")).toBeNull();
  });

  it("mandatory help text names the same-app-registration + admin-consent guidance", async () => {
    adminApi.getM365Connection.mockResolvedValue({ connection: null });
    await mountView();

    const help = q("m365-help").textContent;
    expect(help).toContain("same Entra app registration");
    expect(help).toContain("Files.ReadWrite");
    expect(help).toContain("offline_access");
    expect(help).toContain("admin consent");
    expect(help).toContain("AADSTS65001");
    expect(help).toContain("AADSTS7000215");
    expect(help).toContain("AADSTS500011");
  });

  it("blank-secret-preserve: submitting without touching the secret sends clientSecret: \"\"", async () => {
    adminApi.getM365Connection.mockResolvedValue({ connection: CONFIGURED });
    adminApi.upsertM365Connection.mockResolvedValue({ connection: CONFIGURED });
    await mountView();

    q("m365-save-btn").click();
    await flushPromises();

    expect(adminApi.upsertM365Connection).toHaveBeenCalledTimes(1);
    const [payload] = adminApi.upsertM365Connection.mock.calls[0];
    expect(payload).toEqual({
      entraTenantId: CONFIGURED.entraTenantId,
      clientId: CONFIGURED.clientId,
      clientSecret: "",
      enabled: true,
    });
  });

  it("sends the new secret when the admin types one", async () => {
    adminApi.getM365Connection.mockResolvedValue({ connection: CONFIGURED });
    adminApi.upsertM365Connection.mockResolvedValue({ connection: CONFIGURED });
    await mountView();

    setField("m365-secret", "new-secret-value");
    q("m365-save-btn").click();
    await flushPromises();

    const [payload] = adminApi.upsertM365Connection.mock.calls[0];
    expect(payload.clientSecret).toBe("new-secret-value");
  });

  it("Test connection ok renders the success detail", async () => {
    adminApi.getM365Connection.mockResolvedValue({ connection: CONFIGURED });
    adminApi.testM365Connection.mockResolvedValue({ ok: true, detail: "OBO exchange succeeded" });
    await mountView();

    q("m365-test-btn").click();
    await flushPromises();

    const result = q("m365-test-result");
    expect(result).not.toBeNull();
    expect(result.textContent).toContain("OBO exchange succeeded");
  });

  it("Test connection failure renders the classified AADSTS detail", async () => {
    adminApi.getM365Connection.mockResolvedValue({ connection: CONFIGURED });
    adminApi.testM365Connection.mockResolvedValue({
      ok: false,
      detail: "admin consent required (AADSTS65001)",
    });
    await mountView();

    q("m365-test-btn").click();
    await flushPromises();

    const result = q("m365-test-result");
    expect(result).not.toBeNull();
    expect(result.textContent).toContain("AADSTS65001");
  });

  it("Disconnect asks for confirmation, then calls deleteM365Connection and resets the form", async () => {
    adminApi.getM365Connection.mockResolvedValue({ connection: CONFIGURED });
    adminApi.deleteM365Connection.mockResolvedValue({});
    vi.stubGlobal("confirm", () => true);
    await mountView();

    q("m365-disconnect-btn").click();
    await flushPromises();

    expect(adminApi.deleteM365Connection).toHaveBeenCalledTimes(1);
    expect(q("m365-tenant-id").value).toBe("");
    expect(q("m365-client-id").value).toBe("");
    expect(q("m365-secret").placeholder).not.toContain("unchanged");
  });

  it("Disconnect does nothing when the confirmation is declined", async () => {
    adminApi.getM365Connection.mockResolvedValue({ connection: CONFIGURED });
    vi.stubGlobal("confirm", () => false);
    await mountView();

    q("m365-disconnect-btn").click();
    await flushPromises();

    expect(adminApi.deleteM365Connection).not.toHaveBeenCalled();
    expect(q("m365-tenant-id").value).toBe(CONFIGURED.entraTenantId);
  });
});
