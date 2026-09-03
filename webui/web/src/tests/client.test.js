import { describe, it, expect, beforeEach, afterEach, vi } from "vitest";
import { createPinia, setActivePinia } from "pinia";

// Hoisted mock — vi.mock is hoisted to the top of the module by vitest.
const mockGetAccessToken = vi.fn();
vi.mock("../auth/oidc.js", () => ({
  getAccessToken: mockGetAccessToken,
}));

describe("api/client.js", () => {
  let fetchMock;

  beforeEach(() => {
    setActivePinia(createPinia());
    fetchMock = vi.fn();
    global.fetch = fetchMock;
    // Return a valid token by default.
    mockGetAccessToken.mockResolvedValue("test-token-alice");
  });

  afterEach(() => {
    vi.restoreAllMocks();
    mockGetAccessToken.mockReset();
  });

  async function loadClient() {
    const mod = await import("../api/client.js");
    return mod;
  }

  it("prefixes /api for normal paths", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({ ok: true }),
    });
    const { get } = await loadClient();
    await get("/tasks");
    expect(fetchMock).toHaveBeenCalledWith(
      "/api/tasks",
      expect.any(Object)
    );
  });

  it("passes /agui through unchanged (no /api prefix)", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({}),
    });
    const { request } = await loadClient();
    await request("/agui", { method: "POST" });
    const [url] = fetchMock.mock.calls[0];
    expect(url).toBe("/agui");
  });

  it("passes /audit/stream through unchanged", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({}),
    });
    const { request } = await loadClient();
    await request("/audit/stream", {});
    const [url] = fetchMock.mock.calls[0];
    expect(url).toBe("/audit/stream");
  });

  it("attaches Authorization: Bearer from auth token", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({}),
    });
    mockGetAccessToken.mockResolvedValue("tok-bob");
    const { get } = await loadClient();
    await get("/foo");
    const [, opts] = fetchMock.mock.calls[0];
    expect(opts.headers["Authorization"]).toBe("Bearer tok-bob");
  });

  it("does NOT send x-aikonos-user header", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({}),
    });
    const { get } = await loadClient();
    await get("/foo");
    const [, opts] = fetchMock.mock.calls[0];
    expect(opts.headers["x-aikonos-user"]).toBeUndefined();
  });

  it("attaches content-type application/json when body is present", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({}),
    });
    const { post } = await loadClient();
    await post("/foo", { body: { x: 1 } });
    const [, opts] = fetchMock.mock.calls[0];
    expect(opts.headers["content-type"]).toBe("application/json");
  });

  it("GET with no body does not send content-type", async () => {
    fetchMock.mockResolvedValue({
      ok: true,
      status: 200,
      json: async () => ({}),
    });
    const { get } = await loadClient();
    await get("/foo");
    const [, opts] = fetchMock.mock.calls[0];
    expect(opts.headers["content-type"]).toBeUndefined();
  });

  it("throws when no access token is available (unauthenticated)", async () => {
    mockGetAccessToken.mockResolvedValue(null);
    const { request } = await loadClient();
    await expect(request("/foo")).rejects.toThrow("not authenticated");
  });

  it("403 returns { forbidden: true } without throwing", async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 403,
      json: async () => ({ error: "not allowed" }),
    });
    const { get } = await loadClient();
    const result = await get("/admin/foo");
    expect(result.forbidden).toBe(true);
    expect(result.error).toBe("not allowed");
  });

  it("500 throws Error with server message", async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 500,
      json: async () => ({ error: "internal error" }),
    });
    const { get } = await loadClient();
    await expect(get("/broken")).rejects.toThrow("internal error");
  });

  it("non-2xx throws an Error carrying the parsed body on .data (e.g. a 409's suggested_name)", async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 409,
      json: async () => ({ error: "already exists", suggested_name: "my-notes-2" }),
    });
    const { get } = await loadClient();
    await expect(get("/broken")).rejects.toMatchObject({
      message: "already exists",
      status: 409,
      data: { error: "already exists", suggested_name: "my-notes-2" },
    });
  });

  it("non-2xx with no JSON body throws generic error", async () => {
    fetchMock.mockResolvedValue({
      ok: false,
      status: 502,
      json: async () => { throw new SyntaxError("bad json"); },
    });
    const { get } = await loadClient();
    await expect(get("/broken")).rejects.toThrow(/request failed/);
  });
});
