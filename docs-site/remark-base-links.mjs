// Astro's `base` config rewrites its own generated nav (sidebar, pagination,
// breadcrumbs, search) automatically, but literal root-relative links written
// in markdown/MDX body content are just strings — Astro never touches them.
// This hand-rolled mdast walker prefixes those on build, so subpath
// deployments (DOCS_BASE=/docs) don't produce dead root-relative links.
// No-op when base is falsy (the default root build).
export default function remarkBaseLinks(base) {
  return (tree) => {
    if (!base) return;
    walk(tree, base);
  };
}

function walk(node, base) {
  if (!node || typeof node !== 'object') return;
  if ((node.type === 'link' || node.type === 'image' || node.type === 'definition') && typeof node.url === 'string') {
    node.url = prefixed(node.url, base);
  }
  if (Array.isArray(node.children)) {
    for (const child of node.children) walk(child, base);
  }
}

function prefixed(url, base) {
  // Only root-relative URLs need prefixing — relative paths, http(s)/mailto:
  // links, and fragment-only anchors are untouched.
  if (!url.startsWith('/') || url.startsWith('//')) return url;
  // Already base-prefixed (e.g. hand-written `/docs/...`) — don't double it.
  if (url === base || url.startsWith(`${base}/`) || url.startsWith(`${base}?`) || url.startsWith(`${base}#`)) {
    return url;
  }
  return base + url;
}
