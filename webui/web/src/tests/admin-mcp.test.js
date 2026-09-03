// Tests for views/admin/Mcp.vue (CP7).
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/admin.js", () => ({
  listAssignments:      vi.fn(),
  assignRole:           vi.fn(),
  revokeRole:           vi.fn(),
  listNetworkRules:     vi.fn(),
  addNetworkRule:       vi.fn(),
  deleteNetworkRule:    vi.fn(),
  listAdminRuns:        vi.fn(),
  listMcpConnections:   vi.fn(),
  addMcpConnection:     vi.fn(),
  updateMcpConnection:  vi.fn(),
  deleteMcpConnection:  vi.fn(),
}));

import Mcp from "../views/admin/Mcp.vue";
import * as adminApi from "../api/admin.js";
import { useToast } from "../components/ui/useToast.js";

const SAMPLE_CONN = {
  id: "c1",
  name: "my-server",
  url: "https://mcp.example.com",
  transport: "streamable_http",
  authType: "bearer",
  createdBy: "admin@example.com",
  createdAt: "2024-01-01T00:00:00Z",
};

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/admin/mcp", component: Mcp },
      { path: "/", component: { template: "<div/>" } },
    ],
  });
}

describe("Mcp.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "admin@example.com", sub: "admin@example.com" });
    vi.clearAllMocks();
    // drain any leftover toasts between tests
    const { toasts } = useToast();
    toasts.splice(0);
  });
  afterEach(() => vi.restoreAllMocks());

  it("renders connection list from API response", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [SAMPLE_CONN] });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.findAll("[data-testid='conn-row']").length).toBe(1);
    expect(w.text()).toContain("my-server");
    expect(w.text()).toContain("https://mcp.example.com");
    // bearer token value must never leak into the rendered DOM
    expect(w.text()).not.toContain("secret");
    // ui-set: DataTable renders the table
    expect(w.find(".dt-table").exists()).toBe(true);
  });

  it("addMcpConnection is called with correct body including bearerToken when auth_type=bearer", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [] });
    adminApi.addMcpConnection.mockResolvedValue({ connection: SAMPLE_CONN });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='conn-name']").setValue("my-server");
    await w.find("[data-testid='conn-url']").setValue("https://mcp.example.com");
    await w.find("[data-testid='conn-transport']").setValue("streamable_http");
    await w.find("[data-testid='conn-auth-type']").setValue("bearer");
    await w.find("[data-testid='conn-bearer']").setValue("secret-token");
    await w.find("[data-testid='add-conn-btn']").trigger("click");
    await flushPromises();

    expect(adminApi.addMcpConnection).toHaveBeenCalledWith(
      expect.objectContaining({
        name: "my-server",
        url: "https://mcp.example.com",
        transport: "streamable_http",
        authType: "bearer",
        bearerToken: "secret-token",
      }),
    );
  });

  it("bearer field is hidden when auth_type is none", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [] });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='conn-auth-type']").setValue("none");
    expect(w.find("[data-testid='conn-bearer']").exists()).toBe(false);
  });

  it("addMcpConnection is called WITHOUT bearerToken when auth_type is none", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [] });
    adminApi.addMcpConnection.mockResolvedValue({ connection: { ...SAMPLE_CONN, authType: "none" } });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='conn-name']").setValue("no-auth");
    await w.find("[data-testid='conn-url']").setValue("https://mcp.example.com");
    await w.find("[data-testid='conn-auth-type']").setValue("none");
    await w.find("[data-testid='add-conn-btn']").trigger("click");
    await flushPromises();

    const callArg = adminApi.addMcpConnection.mock.calls[0][0];
    expect(callArg).not.toHaveProperty("bearerToken");
  });

  it("edit pre-fills existing fields from the row", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [SAMPLE_CONN] });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-c1']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='conn-name']").element.value).toBe("my-server");
    expect(w.find("[data-testid='conn-url']").element.value).toBe("https://mcp.example.com");
    expect(w.find("[data-testid='conn-transport']").element.value).toBe("streamable_http");
    expect(w.find("[data-testid='conn-auth-type']").element.value).toBe("bearer");
  });

  it("updateMcpConnection is called with correct id and body on edit submit", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [SAMPLE_CONN] });
    adminApi.updateMcpConnection.mockResolvedValue({ connection: SAMPLE_CONN });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-c1']").trigger("click");
    await flushPromises();

    await w.find("[data-testid='conn-name']").setValue("renamed");
    await w.find("[data-testid='add-conn-btn']").trigger("click");
    await flushPromises();

    expect(adminApi.updateMcpConnection).toHaveBeenCalledWith(
      "c1",
      expect.objectContaining({ name: "renamed" }),
    );
    // bearer field was left blank on edit — keep-existing-token path must not send a token
    const callArg = adminApi.updateMcpConnection.mock.calls[0][1];
    expect(callArg).not.toHaveProperty("bearerToken");
  });

  it("deleteMcpConnection is called with connection id on delete click", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [SAMPLE_CONN] });
    adminApi.deleteMcpConnection.mockResolvedValue({ success: true });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='delete-c1']").trigger("click");
    await flushPromises();
    expect(adminApi.deleteMcpConnection).toHaveBeenCalledWith("c1");
  });

  it("renders not-an-admin empty-state on forbidden response without throwing", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ forbidden: true });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='forbidden']").exists()).toBe(true);
    expect(w.find("[data-testid='conn-row']").exists()).toBe(false);
  });

  // --- new: forbidden mutation handling ---

  it("add submit with forbidden response shows error banner, no toast, form NOT cleared", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [] });
    adminApi.addMcpConnection.mockResolvedValue({ forbidden: true, error: "not an admin" });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='conn-name']").setValue("test-srv");
    await w.find("[data-testid='conn-url']").setValue("https://test.com");
    await w.find("[data-testid='add-conn-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.find("[data-testid='error-banner']").text()).toContain("not an admin");
    // form NOT cleared
    expect(w.find("[data-testid='conn-name']").element.value).toBe("test-srv");
    // load NOT called again
    expect(adminApi.listMcpConnections).toHaveBeenCalledTimes(1);
    // no toast
    const { toasts } = useToast();
    expect(toasts.filter(t => t.type === "ok").length).toBe(0);
  });

  it("update submit with forbidden response shows error banner, form NOT cleared", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [SAMPLE_CONN] });
    adminApi.updateMcpConnection.mockResolvedValue({ forbidden: true, error: "not an admin" });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='edit-c1']").trigger("click");
    await flushPromises();
    await w.find("[data-testid='conn-name']").setValue("renamed");
    await w.find("[data-testid='add-conn-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.find("[data-testid='error-banner']").text()).toContain("not an admin");
    expect(w.find("[data-testid='conn-name']").element.value).toBe("renamed");
  });

  it("delete with forbidden response shows error banner", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [SAMPLE_CONN] });
    adminApi.deleteMcpConnection.mockResolvedValue({ forbidden: true, error: "not an admin" });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='delete-c1']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.find("[data-testid='error-banner']").text()).toContain("not an admin");
    expect(adminApi.listMcpConnections).toHaveBeenCalledTimes(1);
  });

  it("submit with empty required fields shows validation error, addMcpConnection NOT called", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [] });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();

    // name and url both empty
    await w.find("[data-testid='add-conn-btn']").trigger("click");
    await flushPromises();

    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
    expect(w.find("[data-testid='error-banner']").text().toLowerCase()).toContain("name and url");
    expect(adminApi.addMcpConnection).not.toHaveBeenCalled();
  });

  it("add success fires a toast", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [] });
    adminApi.addMcpConnection.mockResolvedValue({ connection: SAMPLE_CONN });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='conn-name']").setValue("my-server");
    await w.find("[data-testid='conn-url']").setValue("https://mcp.example.com");
    await w.find("[data-testid='add-conn-btn']").trigger("click");
    await flushPromises();

    const { toasts } = useToast();
    expect(toasts.some(t => t.type === "ok")).toBe(true);
  });

  it("delete success fires a toast", async () => {
    adminApi.listMcpConnections.mockResolvedValue({ connections: [SAMPLE_CONN] });
    adminApi.deleteMcpConnection.mockResolvedValue({ success: true });
    const router = makeRouter();
    await router.push("/admin/mcp");
    const w = mount(Mcp, { global: { plugins: [router] } });
    await flushPromises();

    await w.find("[data-testid='delete-c1']").trigger("click");
    await flushPromises();

    const { toasts } = useToast();
    expect(toasts.some(t => t.type === "ok")).toBe(true);
  });
});
