// isSvcPrincipal returns true when a FGA subject/object string refers to a
// service principal (`user:svc-…`) or service group (`group:svc-…`).
//
// Why filter these from the Roles view: a service principal exists only to
// represent a named agent running unattended via an API key. Hand-assigning
// it through the admin UI would bypass the key-based identity seam, and an
// admin must not add a human into `group:svc-<agentId>` (which would grant the
// agent's entire skill bundle to that human).
export function isSvcPrincipal(s) {
  return s.startsWith("user:svc-") || s.startsWith("group:svc-");
}

// filterTuples removes any FGA tuples where the user (subject) or object
// refers to a service principal. Callers may pre-filter by section; this
// function only strips the svc- rows.
export function filterTuples(list) {
  return list.filter(
    (t) => !isSvcPrincipal(t.user ?? "") && !isSvcPrincipal(t.object ?? ""),
  );
}

// filterPrincipals removes any principals whose id refers to a service
// principal. Used to exclude svc- entries from assign-dialog dropdowns.
export function filterPrincipals(list) {
  return list.filter((p) => !isSvcPrincipal(p.id ?? ""));
}
