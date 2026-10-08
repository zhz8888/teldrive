// Per-session counter; `>>> 0` keeps it a 32-bit unsigned value that wraps at 2^32
// instead of growing without bound.
let sequence = 0;

/**
 * Mints an id that only has to be unique within this browser session (React keys, upload
 * task/batch ids). The timestamp plus counter keep ids ordered and collision-free in one
 * session; the random suffix guards against two tabs generating the same value.
 */
export function newClientId() {
  sequence = (sequence + 1) >>> 0;
  return `${Date.now().toString(36)}-${sequence.toString(36)}-${Math.random().toString(36).slice(2)}`;
}
