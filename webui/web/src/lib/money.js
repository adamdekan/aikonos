// money.js — major-unit <-> integer micro-unit conversion (1 major unit =
// 1_000_000 micros). Currency-agnostic: the tenant bills in whatever currency
// it bills in, and the UI never names one.
// Shared by SpendCaps.vue (cap/spend amounts) and Providers.vue (per-1M-token
// pricing) — both surfaces store money as an integer micro-unit count and let
// admins enter/read plain major units.

// toMicros rounds to the nearest micro-unit, guarding float imprecision on
// fractional-cent input (e.g. 0.1 + 0.2 style drift). Negative/NaN -> 0.
export function toMicros(amount) {
  const n = Number(amount);
  if (!isFinite(n) || n <= 0) return 0;
  return Math.round(n * 1_000_000);
}

export function fromMicros(micros) {
  const n = Number(micros);
  return isFinite(n) ? n / 1_000_000 : 0;
}

// fmtAmount renders a micro-unit integer as a fixed 2-decimal string, with no
// currency symbol.
export function fmtAmount(micros) {
  return fromMicros(micros).toFixed(2);
}

// fmtAmountPrecise renders small amounts without collapsing them to 0.00. A
// single chat session typically costs a few hundredths of a major unit, so the
// 2-decimal fmtAmount used for caps and per-1M pricing would show most sessions
// as "0.00" — true to the rounding, useless to the reader. Scales decimals to
// the magnitude instead of picking one width for every case.
export function fmtAmountPrecise(micros) {
  const n = fromMicros(micros);
  if (n === 0) return "0";
  if (n < 0.01) return n.toFixed(4);
  if (n < 1) return n.toFixed(3);
  return n.toFixed(2);
}
