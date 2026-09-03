// CP2 tests: .agent/Sessions/ relocation, includeHidden, migration.
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get:   vi.fn(),
  post:  vi.fn(),
  del:   vi.fn(),
  patch: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import { listFiles, uploadFile, downloadFile, deleteFile } from "../api/files.js";
import {
  listSessionFiles,
  readSession,
  writeSession,
  deleteSession,
  readManifest,
  writeManifest,
  migrateLegacySessions,
} from "../api/sessions.js";

const NEW_PREFIX = ".agent/Sessions/";

beforeEach(() => {
  vi.clearAllMocks();
});

// ---------------------------------------------------------------------------
// api/files.js — includeHidden param
// ---------------------------------------------------------------------------
describe("api/files.js — listFiles includeHidden", () => {
  it("bare listFiles() calls GET /files (no query param)", async () => {
    clientMod.get.mockResolvedValue({ files: [] });
    await listFiles();
    expect(clientMod.get).toHaveBeenCalledWith("/files");
  });

  it("listFiles({ includeHidden: true }) calls GET /files?includeHidden=1", async () => {
    clientMod.get.mockResolvedValue({ files: [] });
    await listFiles({ includeHidden: true });
    expect(clientMod.get).toHaveBeenCalledWith("/files?includeHidden=1");
  });

  it("listFiles({ includeHidden: false }) calls GET /files (no param)", async () => {
    clientMod.get.mockResolvedValue({ files: [] });
    await listFiles({ includeHidden: false });
    expect(clientMod.get).toHaveBeenCalledWith("/files");
  });
});

// ---------------------------------------------------------------------------
// api/sessions.js — path constants
// ---------------------------------------------------------------------------
describe("api/sessions.js — path relocation", () => {
  it("readSession builds .agent/Sessions/<id>.json path", async () => {
    clientMod.get.mockResolvedValue({ contentBase64: btoa(JSON.stringify({ id: "abc" })) });
    await readSession("abc");
    expect(clientMod.get).toHaveBeenCalledWith(
      `/files/content?path=${encodeURIComponent(NEW_PREFIX + "abc.json")}`
    );
  });

  it("writeSession builds .agent/Sessions/<id>.json path", async () => {
    clientMod.post.mockResolvedValue({});
    await writeSession({ id: "abc", title: "t" });
    expect(clientMod.post).toHaveBeenCalledWith(
      "/files",
      expect.objectContaining({ body: expect.objectContaining({ path: NEW_PREFIX + "abc.json" }) })
    );
  });

  it("deleteSession builds .agent/Sessions/<id>.json path", async () => {
    clientMod.del.mockResolvedValue({});
    await deleteSession("abc");
    expect(clientMod.del).toHaveBeenCalledWith(
      `/files?path=${encodeURIComponent(NEW_PREFIX + "abc.json")}`
    );
  });

  it("readManifest uses .agent/Sessions/index.json", async () => {
    clientMod.get.mockResolvedValue({ contentBase64: btoa(JSON.stringify([])) });
    await readManifest();
    expect(clientMod.get).toHaveBeenCalledWith(
      `/files/content?path=${encodeURIComponent(NEW_PREFIX + "index.json")}`
    );
  });

  it("writeManifest uses .agent/Sessions/index.json", async () => {
    clientMod.post.mockResolvedValue({});
    await writeManifest([]);
    expect(clientMod.post).toHaveBeenCalledWith(
      "/files",
      expect.objectContaining({ body: expect.objectContaining({ path: NEW_PREFIX + "index.json" }) })
    );
  });
});

// ---------------------------------------------------------------------------
// api/sessions.js — listSessionFiles passes a scoped dir fetch (CP3: no full-tree call)
// ---------------------------------------------------------------------------
describe("api/sessions.js — listSessionFiles", () => {
  it("calls listFiles scoped to .agent/Sessions, not the full tree", async () => {
    clientMod.get.mockResolvedValue({ files: [] });
    await listSessionFiles();
    expect(clientMod.get).toHaveBeenCalledWith("/files?includeHidden=1&dir=.agent%2FSessions");
  });

  it("returns only .agent/Sessions/*.json files excluding index.json (regex is a shape guard over the scoped result)", async () => {
    clientMod.get.mockResolvedValue({
      files: [
        { path: ".agent/Sessions/abc.json" },
        { path: ".agent/Sessions/index.json" },  // must be excluded
        { path: ".agent/Sessions/def.json" },
      ],
    });
    const result = await listSessionFiles();
    expect(result.map((f) => f.path)).toEqual([
      ".agent/Sessions/abc.json",
      ".agent/Sessions/def.json",
    ]);
  });

  it("returns [] on forbidden", async () => {
    clientMod.get.mockResolvedValue({ forbidden: true });
    const result = await listSessionFiles();
    expect(result).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// api/sessions.js — migrateLegacySessions (CP3: two scoped calls, one per dir)
// ---------------------------------------------------------------------------
describe("migrateLegacySessions", () => {
  const LEGACY_URL = "/files?includeHidden=1&dir=Sessions";
  const NEW_URL = "/files?includeHidden=1&dir=.agent%2FSessions";

  function makeContentBase64(obj) {
    const json = JSON.stringify(obj);
    return btoa(String.fromCharCode(...new TextEncoder().encode(json)));
  }

  it("no-op when no legacy files exist", async () => {
    clientMod.get.mockImplementation((url) => {
      if (url === LEGACY_URL) return Promise.resolve({ files: [] });
      if (url === NEW_URL) return Promise.resolve({ files: [{ path: ".agent/Sessions/abc.json" }] });
      return Promise.resolve({ files: [] });
    });
    await migrateLegacySessions();
    // Both scoped listFiles GETs are always issued; no downloads/uploads/deletes follow.
    expect(clientMod.get).toHaveBeenCalledTimes(2);
    expect(clientMod.get).toHaveBeenCalledWith(LEGACY_URL);
    expect(clientMod.get).toHaveBeenCalledWith(NEW_URL);
    expect(clientMod.post).not.toHaveBeenCalled();
    expect(clientMod.del).not.toHaveBeenCalled();
  });

  it("migrates legacy Sessions/a.json and Sessions/index.json when no new files exist", async () => {
    const legacyContent = makeContentBase64({ id: "a", title: "Session A" });
    const indexContent = makeContentBase64([{ id: "a" }]);

    const contentByPath = {
      "Sessions/a.json": { contentBase64: legacyContent },
      "Sessions/index.json": { contentBase64: indexContent },
    };

    clientMod.get.mockImplementation((url) => {
      if (url === LEGACY_URL) {
        return Promise.resolve({
          files: [
            { path: "Sessions/a.json" },
            { path: "Sessions/index.json" },
          ],
        });
      }
      if (url === NEW_URL) return Promise.resolve({ files: [] });
      // download: url is /files/content?path=<encoded>
      const match = url.match(/path=(.+)$/);
      if (match) {
        const p = decodeURIComponent(match[1]);
        return Promise.resolve(contentByPath[p] ?? {});
      }
      return Promise.resolve({});
    });

    clientMod.post.mockResolvedValue({});
    clientMod.del.mockResolvedValue({});

    await migrateLegacySessions();

    // Both scoped dirs were fetched (legacy + new).
    expect(clientMod.get).toHaveBeenCalledWith(LEGACY_URL);
    expect(clientMod.get).toHaveBeenCalledWith(NEW_URL);

    // Should have uploaded two new files (order-independent)
    const uploadedPaths = clientMod.post.mock.calls.map((c) => c[1].body.path);
    expect(uploadedPaths).toContain(".agent/Sessions/a.json");
    expect(uploadedPaths).toContain(".agent/Sessions/index.json");

    // Should have deleted both legacy files (order-independent)
    const deletedPaths = clientMod.del.mock.calls.map((c) => c[0]);
    expect(deletedPaths).toContain(`/files?path=${encodeURIComponent("Sessions/a.json")}`);
    expect(deletedPaths).toContain(`/files?path=${encodeURIComponent("Sessions/index.json")}`);
  });

  it("skips copy but still deletes when new path already exists", async () => {
    clientMod.get.mockImplementation((url) => {
      if (url === LEGACY_URL) return Promise.resolve({ files: [{ path: "Sessions/a.json" }] });
      if (url === NEW_URL) return Promise.resolve({ files: [{ path: ".agent/Sessions/a.json" }] });
      return Promise.resolve({});
    });
    // download should NOT be called for Sessions/a.json since new exists
    clientMod.del.mockResolvedValue({});

    await migrateLegacySessions();

    // No uploads
    expect(clientMod.post).not.toHaveBeenCalled();
    // No downloads for the legacy file — only the two scoped listFiles GETs.
    expect(clientMod.get).toHaveBeenCalledTimes(2);
    // But should delete the legacy file
    const delCalls = clientMod.del.mock.calls.map((c) => c[0]);
    expect(delCalls).toContain(`/files?path=${encodeURIComponent("Sessions/a.json")}`);
  });

  it("is idempotent: second run with no legacy files does nothing", async () => {
    clientMod.get.mockImplementation((url) => {
      if (url === LEGACY_URL) return Promise.resolve({ files: [] });
      if (url === NEW_URL) {
        return Promise.resolve({
          files: [
            { path: ".agent/Sessions/a.json" },
            { path: ".agent/Sessions/index.json" },
          ],
        });
      }
      return Promise.resolve({});
    });

    await migrateLegacySessions();

    expect(clientMod.get).toHaveBeenCalledTimes(2);
    expect(clientMod.post).not.toHaveBeenCalled();
    expect(clientMod.del).not.toHaveBeenCalled();
  });

  it("swallows errors and does not throw", async () => {
    clientMod.get.mockRejectedValue(new Error("network failure"));
    await expect(migrateLegacySessions()).resolves.toBeUndefined();
  });

  it("does not delete legacy file when upload rejects (data-loss fix)", async () => {
    // Sessions/a.json download succeeds, upload rejects — legacy must NOT be deleted.
    const legacyContent = makeContentBase64({ id: "a", title: "Session A" });

    clientMod.get.mockImplementation((url) => {
      if (url === LEGACY_URL) return Promise.resolve({ files: [{ path: "Sessions/a.json" }] });
      if (url === NEW_URL) return Promise.resolve({ files: [] });
      return Promise.resolve({ contentBase64: legacyContent });
    });
    clientMod.post.mockRejectedValue(new Error("upload failed"));
    clientMod.del.mockResolvedValue({});

    await expect(migrateLegacySessions()).resolves.toBeUndefined();

    // deleteFile must NOT have been called for the failed-copy legacy path
    const deletedPaths = clientMod.del.mock.calls.map((c) => c[0]);
    expect(deletedPaths).not.toContain(
      `/files?path=${encodeURIComponent("Sessions/a.json")}`
    );
  });
});
