// View tests for Connections.vue and ConnectCallback.vue.
// api/connectors.js is fully mocked so no server is needed.
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";
import { createPinia, setActivePinia } from "pinia";
import { createRouter, createMemoryHistory } from "vue-router";
import { useUserStore } from "../store/user.js";

vi.mock("../api/connectors.js", () => ({
  listConnectors:         vi.fn(),
  listConnectorProviders: vi.fn(),
  beginConnectorAuth:     vi.fn(),
  completeConnectorAuth:  vi.fn(),
  revokeConnector:        vi.fn(),
}));

import Connections from "../views/Connections.vue";
import ConnectCallback from "../views/ConnectCallback.vue";
import * as connApiView from "../api/connectors.js";

function makeRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: "/connections",      component: Connections },
      { path: "/connect/callback", component: ConnectCallback },
      { path: "/",                 component: { template: "<div/>" } },
    ],
  });
}

describe("Connections.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    vi.clearAllMocks();
    // Default: no providers configured. Tests that exercise Connect buttons override this.
    connApiView.listConnectorProviders.mockResolvedValue({ providers: [] });
  });

  afterEach(() => vi.restoreAllMocks());

  it("renders connector list from API response", async () => {
    connApiView.listConnectors.mockResolvedValue({
      connectors: [
        { connectorId: "c1", provider: 1, status: "active", scopes: [] },
        { connectorId: "c2", provider: 2, status: "active", scopes: [] },
      ],
    });
    const router = makeRouter();
    await router.push("/connections");
    const w = mount(Connections, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.findAll("[data-testid='connector-row']").length).toBe(2);
  });

  it("shows provider label for google_drive and onedrive", async () => {
    connApiView.listConnectors.mockResolvedValue({
      connectors: [
        { connectorId: "c1", provider: 1, status: "active", scopes: [] },
        { connectorId: "c2", provider: 2, status: "active", scopes: [] },
      ],
    });
    const router = makeRouter();
    await router.push("/connections");
    const w = mount(Connections, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.text()).toContain("Google Drive");
    expect(w.text()).toContain("OneDrive");
  });

  it("beginConnectorAuth opens authorizeUrl on connect click", async () => {
    connApiView.listConnectors.mockResolvedValue({ connectors: [] });
    connApiView.listConnectorProviders.mockResolvedValue({
      providers: [{ provider: 1, key: "google_drive", displayName: "Google Drive", connectorId: "gdrive" }],
    });
    connApiView.beginConnectorAuth.mockResolvedValue({ authorizeUrl: "https://accounts.google.com/oauth", state: "s1" });
    const assignSpy = vi.fn();
    Object.defineProperty(window, "location", { value: { assign: assignSpy }, configurable: true, writable: true });
    const router = makeRouter();
    await router.push("/connections");
    const w = mount(Connections, { global: { plugins: [router] } });
    await flushPromises();
    await w.find("[data-testid='connect-google_drive']").trigger("click");
    await flushPromises();
    expect(connApiView.beginConnectorAuth).toHaveBeenCalledWith("google_drive");
    expect(assignSpy).toHaveBeenCalledWith("https://accounts.google.com/oauth");
  });

  it("revokeConnector is called with connector id on revoke click", async () => {
    connApiView.listConnectors
      .mockResolvedValueOnce({ connectors: [{ connectorId: "c1", provider: 1, status: "active", scopes: [] }] })
      .mockResolvedValue({ connectors: [] });
    connApiView.revokeConnector.mockResolvedValue({ success: true });
    const router = makeRouter();
    await router.push("/connections");
    const w = mount(Connections, { global: { plugins: [router] } });
    await flushPromises();
    await w.find("[data-testid='revoke-c1']").trigger("click");
    await flushPromises();
    expect(connApiView.revokeConnector).toHaveBeenCalledWith("c1");
  });

  it("shows error banner on API error", async () => {
    connApiView.listConnectors.mockRejectedValue(new Error("network error"));
    const router = makeRouter();
    await router.push("/connections");
    const w = mount(Connections, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='error-banner']").exists()).toBe(true);
  });

  it("hides the Add connection section when no providers are configured", async () => {
    connApiView.listConnectors.mockResolvedValue({ connectors: [] });
    connApiView.listConnectorProviders.mockResolvedValue({ providers: [] });
    const router = makeRouter();
    await router.push("/connections");
    const w = mount(Connections, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.text()).not.toContain("Add connection");
    expect(w.find("[data-testid='connect-onedrive']").exists()).toBe(false);
    expect(w.find("[data-testid='connect-google_drive']").exists()).toBe(false);
  });

  it("renders Connect buttons only for the providers the broker reports configured", async () => {
    connApiView.listConnectors.mockResolvedValue({ connectors: [] });
    connApiView.listConnectorProviders.mockResolvedValue({
      providers: [{ provider: 2, key: "onedrive", displayName: "OneDrive", connectorId: "onedrive" }],
    });
    const router = makeRouter();
    await router.push("/connections");
    const w = mount(Connections, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='connect-onedrive']").exists()).toBe(true);
    expect(w.find("[data-testid='connect-onedrive']").text()).toContain("Connect OneDrive");
    // Google Drive has no creds → not advertised → no button.
    expect(w.find("[data-testid='connect-google_drive']").exists()).toBe(false);
  });
});

describe("ConnectCallback.vue", () => {
  beforeEach(() => {
    setActivePinia(createPinia());
    useUserStore().setFromProfile({ email: "alice@example.com", sub: "alice@example.com" });
    vi.clearAllMocks();
  });

  afterEach(() => vi.restoreAllMocks());

  it("posts code+state from query params to completeConnectorAuth", async () => {
    connApiView.completeConnectorAuth.mockResolvedValue({ connectorId: "c1", status: "active", scopes: [] });
    const router = makeRouter();
    await router.push("/connect/callback?code=CODE123&state=STATE456");
    const w = mount(ConnectCallback, { global: { plugins: [router] } });
    await flushPromises();
    expect(connApiView.completeConnectorAuth).toHaveBeenCalledWith("CODE123", "STATE456");
  });

  it("shows success state on completion", async () => {
    connApiView.completeConnectorAuth.mockResolvedValue({ connectorId: "c1", status: "active", scopes: [] });
    const router = makeRouter();
    await router.push("/connect/callback?code=CODE123&state=STATE456");
    const w = mount(ConnectCallback, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='callback-success']").exists()).toBe(true);
  });

  it("shows failure state on API error", async () => {
    connApiView.completeConnectorAuth.mockRejectedValue(new Error("exchange failed"));
    const router = makeRouter();
    await router.push("/connect/callback?code=BAD&state=BAD");
    const w = mount(ConnectCallback, { global: { plugins: [router] } });
    await flushPromises();
    expect(w.find("[data-testid='callback-error']").exists()).toBe(true);
  });
});
