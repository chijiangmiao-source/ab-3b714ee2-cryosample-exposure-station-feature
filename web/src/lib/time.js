// Canonical event-time format shared with the API:
// RFC3339, whole seconds, UTC with a "Z" suffix — e.g. 2026-09-13T10:00:00Z.

export const Z_TIME_RE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/

/** Format a Date as RFC3339 whole seconds with Z. */
export function formatZ(d) {
  return d.toISOString().slice(0, 19) + 'Z'
}

/** Current time in the canonical format. */
export function nowZ() {
  return formatZ(new Date())
}

/**
 * Strict validation: shape must match YYYY-MM-DDTHH:MM:SSZ and the value
 * must be a real calendar time (round-trip checked to reject e.g. month 13).
 */
export function isValidZ(s) {
  if (typeof s !== 'string' || !Z_TIME_RE.test(s)) return false
  const t = Date.parse(s)
  if (!Number.isFinite(t)) return false
  return formatZ(new Date(t)) === s
}
