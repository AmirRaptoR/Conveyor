// ---- pure helpers -----------------------------------------------------------
// Grouped and marked (see the matching comment below) so
// internal/server/web/pure_test.mjs can pull this region's text out of the
// file and evaluate it without a DOM library or a build step. Every function
// here takes plain values, never a DOM node, on purpose: it is what makes
// them testable under stock Node.

// The identity of a focusable thing inside a subtree draw() replaces,
// reduced to one comparable string. `desc` is `{kind, id, stage, control}` —
// whichever of `id`/`stage` applies to that kind, `control` only for a card
// (its own body versus the `.open` link). The same shape is built fresh by
// looking at a live element (see focusDescriptor below) and by looking one
// back up after a rebuild (findFocusTarget) — this is the thing the two
// agree on.
export function focusKeyOf(desc) {
  if (!desc) return "";
  return `${desc.kind}:${desc.id ?? desc.stage ?? ""}:${desc.control ?? ""}`;
}

// One step of a keyboard/touch queue reorder: the index a card would land on
// after moving `dir` (-1 up, +1 down) among `length` cards, or null when that
// runs off either end. Refused there, never wrapped — matching the drag,
// which cannot drop a card past the ends of its own queue either.
export function nextQueueIndex(index, length, dir) {
  const target = index + dir;
  if (target < 0 || target >= length) return null;
  return target;
}

// A draw() requested while a card is mid-drag must leave the DOM alone: the
// dragged node has to stay the live element the drop lands on (see wireDrag's
// dragstart/dragend and the draw() guard that calls this). Its own function
// so that decision is one line to test without simulating a real drag.
export function shouldDeferDraw(isDragging) { return !!isDragging; }

// startable()'s whole rule, as a function of the stage list rather than the
// module-level `state` global — `startable` below is a thin wrapper over
// this so the rule itself carries no DOM or global-state dependency and can
// be exercised with a plain array of `{name, next}`.
export function startableRule(from, into, stages) {
  const first = stages[0];
  if (!first || from !== first.name) return false;
  const stageDef = stages.find(s => s.name === from);
  return !!into && stageDef?.next === into;
}

// The stale threshold a source's own lastListedAt is measured against: twice
// the configured poll interval, floored at 60s so a poll under a minute
// (this repo's own test configs set poll: 100ms) does not call every source
// stale between polls. pollNs absent or zero — an old server, or one that
// never set it — falls back to twice the loader's own 5m default
// (internal/config/config.go), never a bare 2× of nothing.
export function staleThresholdMs(pollNs) {
  if (!pollNs) return 10 * 60 * 1000;
  return Math.max(2 * (pollNs / 1e6), 60000);
}

// Whether one source is degraded, as of `nowMsVal` — every input a plain
// value, so this is exercised with injected timestamps and a frozen clock
// rather than by sleeping. A source carrying configuration problems is never
// degraded: it is not being listed by design, and #faults already says so.
// Before the first refresh completes (updatedAtMs falsy), nothing is
// degraded — a board that has not polled yet is starting up, not broken.
export function sourceDegraded(s, updatedAtMs, pollNs, nowMsVal) {
  if ((s.problems || []).length) return false;
  if (!updatedAtMs) return false;
  if (s.listError) return true;
  if (!s.lastListedAt) return true;
  const at = new Date(s.lastListedAt).getTime();
  if (isNaN(at)) return true;
  return nowMsVal - at > staleThresholdMs(pollNs);
}
// ---- end pure helpers -------------------------------------------------------
