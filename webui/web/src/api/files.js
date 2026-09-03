import { get, post, del } from "./client.js";

export function listFiles({ includeHidden, dir, recursive } = {}) {
  const params = [];
  if (includeHidden) params.push("includeHidden=1");
  if (dir) params.push(`dir=${encodeURIComponent(dir)}`);
  if (recursive) params.push("recursive=1");
  return get(params.length ? `/files?${params.join("&")}` : "/files");
}

export function uploadFile(path, contentBase64) {
  return post("/files", { body: { path, contentBase64 } });
}

export function downloadFile(path) {
  return get(`/files/content?path=${encodeURIComponent(path)}`);
}

export function deleteFile(path) {
  return del(`/files?path=${encodeURIComponent(path)}`);
}

export function moveFile(from, to) {
  return post("/files/move", { body: { from, to } });
}

export function createDir(path) {
  return post("/files/dir", { body: { path } });
}
