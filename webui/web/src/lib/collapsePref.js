export function readCollapsed(key, defaultVal = false) {
  try {
    const v = localStorage.getItem(key);
    if (v === null) return defaultVal;
    return JSON.parse(v) === true;
  } catch {
    return defaultVal;
  }
}

export function writeCollapsed(key, val) {
  try { localStorage.setItem(key, JSON.stringify(val)); } catch { /* swallow */ }
}
