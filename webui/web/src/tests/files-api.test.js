// Tests for api/files.js in isolation (no view rendering).
import { describe, it, expect, vi, beforeEach } from "vitest";

vi.mock("../api/client.js", () => ({
  get:   vi.fn(),
  post:  vi.fn(),
  del:   vi.fn(),
  patch: vi.fn(),
}));

import * as clientMod from "../api/client.js";
import { listFiles, uploadFile, downloadFile, deleteFile, moveFile, createDir } from "../api/files.js";

describe("api/files.js", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("listFiles calls GET /files", async () => {
    clientMod.get.mockResolvedValue({ files: [] });
    await listFiles();
    expect(clientMod.get).toHaveBeenCalledWith("/files");
  });

  it("listFiles({ dir }) calls GET /files?dir=<encoded>", async () => {
    clientMod.get.mockResolvedValue({ files: [] });
    await listFiles({ dir: "reports/q1" });
    expect(clientMod.get).toHaveBeenCalledWith("/files?dir=reports%2Fq1");
  });

  it("listFiles({ recursive: true }) calls GET /files?recursive=1", async () => {
    clientMod.get.mockResolvedValue({ files: [] });
    await listFiles({ recursive: true });
    expect(clientMod.get).toHaveBeenCalledWith("/files?recursive=1");
  });

  it("listFiles({ dir, includeHidden, recursive }) combines all three params", async () => {
    clientMod.get.mockResolvedValue({ files: [] });
    await listFiles({ dir: ".agent/Sessions", includeHidden: true, recursive: true });
    expect(clientMod.get).toHaveBeenCalledWith(
      "/files?includeHidden=1&dir=.agent%2FSessions&recursive=1"
    );
  });

  it("uploadFile posts path and contentBase64 to /files", async () => {
    clientMod.post.mockResolvedValue({ file: { path: "hello.txt", size: 5, modified: null } });
    await uploadFile("hello.txt", "aGVsbG8=");
    expect(clientMod.post).toHaveBeenCalledWith("/files", { body: { path: "hello.txt", contentBase64: "aGVsbG8=" } });
  });

  it("downloadFile calls GET /files/content?path=encoded", async () => {
    clientMod.get.mockResolvedValue({ path: "hello.txt", mime: "text/plain", contentBase64: "aGVsbG8=" });
    await downloadFile("hello.txt");
    expect(clientMod.get).toHaveBeenCalledWith("/files/content?path=hello.txt");
  });

  it("downloadFile encodes path with special chars", async () => {
    clientMod.get.mockResolvedValue({ path: "a b.txt", mime: "text/plain", contentBase64: "" });
    await downloadFile("a b.txt");
    expect(clientMod.get).toHaveBeenCalledWith("/files/content?path=a%20b.txt");
  });

  it("deleteFile calls DELETE /files?path=encoded", async () => {
    clientMod.del.mockResolvedValue({ success: true });
    await deleteFile("hello.txt");
    expect(clientMod.del).toHaveBeenCalledWith("/files?path=hello.txt");
  });

  it("moveFile posts {from,to} to /files/move", async () => {
    clientMod.post.mockResolvedValue({ file: { path: "b.txt", size: 5, modified: null, isDir: false } });
    await moveFile("a.txt", "b.txt");
    expect(clientMod.post).toHaveBeenCalledWith("/files/move", { body: { from: "a.txt", to: "b.txt" } });
  });

  it("createDir posts {path} to /files/dir", async () => {
    clientMod.post.mockResolvedValue({ success: true });
    await createDir("newdir");
    expect(clientMod.post).toHaveBeenCalledWith("/files/dir", { body: { path: "newdir" } });
  });
});
