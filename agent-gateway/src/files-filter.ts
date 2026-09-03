// Pure filter for the GET /files handler.
//
// A path has a dot-prefixed segment when any component produced by splitting on
// "/" starts with ".". Mid-segment dots ("foo.bar", "a/b.json") are NOT
// dot-prefixed — only a leading "." on a component counts.
export function hasDotSegment(path: string): boolean {
  return path.split("/").some((seg) => seg.startsWith("."));
}

// Filter a file-entry array based on the includeHidden flag from the request.
// When includeHidden is false, entries with a dot-prefixed path segment are
// excluded. When true, all entries are returned unchanged.
export function filterFiles<T extends { path: string }>(
  files: T[],
  includeHidden: boolean,
): T[] {
  if (includeHidden) return files;
  return files.filter((f) => !hasDotSegment(f.path));
}

// Shapes a broker workspace-file record into the wire JSON returned by the
// /files routes.
export interface WorkspaceFileJson {
  path: string;
  sizeBytes: number;
  modifiedAt?: Date;
  isDir?: boolean;
}

export function fileJson(f: WorkspaceFileJson) {
  return {
    path: f.path,
    size: Number(f.sizeBytes ?? 0),
    modified: f.modifiedAt ? f.modifiedAt.toISOString() : null,
    isDir: Boolean(f.isDir),
  };
}
