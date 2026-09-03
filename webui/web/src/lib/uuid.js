// crypto.randomUUID is only defined in a secure context (HTTPS or localhost).
// When the app is served over plain HTTP on a LAN IP, it is undefined, so fall
// back to a Math.random-based v4 generator. Thread ids are not security-sensitive.
export function uuid() {
  if (typeof crypto !== "undefined" && typeof crypto.randomUUID === "function") {
    return crypto.randomUUID();
  }
  return "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(/[xy]/g, (c) => {
    const r = (Math.random() * 16) | 0;
    const v = c === "x" ? r : (r & 0x3) | 0x8;
    return v.toString(16);
  });
}
