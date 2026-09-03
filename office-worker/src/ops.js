// The 15 tool ops the worker exposes over HTTP as POST /v1/<format>/<op>.
// Checkpoints 2-3 fill in real handlers via registerHandler; an op that is
// known but unregistered returns 501 (not 404) — the route table is complete
// from checkpoint 1 on, only the implementations trail behind.
export const OPS_BY_FORMAT = {
  docx: ['create', 'edit', 'extract'],
  xlsx: ['create', 'edit', 'extract', 'recalc'],
  pptx: ['create', 'edit', 'extract', 'thumbnail'],
  pdf: ['create', 'transform', 'extract'],
  office: ['convert'],
};

export const KNOWN_OPS = new Set(
  Object.entries(OPS_BY_FORMAT).flatMap(([format, ops]) => ops.map((op) => `${format}/${op}`))
);

// Ops that shell out to LibreOffice (checkpoint 3) get the longer default
// timeout (design doc: "soffice ops 180000").
export const SOFFICE_OPS = new Set(['xlsx/recalc', 'pptx/thumbnail', 'office/convert']);

// Mutable handler registry — a plain object keyed by "<format>/<op>".
export const HANDLERS = {};

export function registerHandler(opKey, handler) {
  if (!KNOWN_OPS.has(opKey)) {
    throw new Error(`cannot register handler for unknown op "${opKey}"`);
  }
  HANDLERS[opKey] = handler;
}

export function unregisterHandler(opKey) {
  delete HANDLERS[opKey];
}
