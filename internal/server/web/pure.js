// ---- pure helpers -----------------------------------------------------------
// Grouped and marked (see the matching comment below) so
// internal/server/web/pure.test.mjs can pull this region's text out of the
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

// Which controls a given mode offers, as a function of the mode string alone
// — never of the DOM — so draw() can hide a control the server would only
// ever 403, rather than offering it and reporting the refusal after the fact.
// Every mutation route the engine actually gates refuses in observe and only
// observe (CONTRACTS: manual still takes the tick button), so one flag serves
// all five controls; they are named individually because that is what a
// reader checking this against the acceptance criteria wants to see.
export function controlsForMode(mode) {
  const enabled = mode !== "observe";
  return { tick: enabled, unblockAll: enabled, diagnose: enabled, handBack: enabled, dragStart: enabled };
}

// The same http(s)-only rule paraInline already applies to a report's own
// `[text](url)` links (report.go's identityLine emits only that scheme), used
// here for a card's provider link too: escaping markup is not validating a
// scheme, and a `javascript:` URL from a provider is markup either way.
export function isHttpUrl(u) { return /^https?:\/\//i.test(u || ""); }

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

// The inbox's (#40) own classification: one word for why a card belongs
// there at all. Built on the same facts board.js's tone()/WAITING already
// read — never a second vocabulary for the same mark. `dependency` and
// `limit` get their own category because the issue asks readers to tell a
// sequencing wait from a quota wait at a glance; every other WAITING kind
// (turns, unfinished, worktree) folds into the generic "waiting". A `pending`
// item is not blocked at all — it is a resting item (exit 10, e.g. approve's
// quiet-PR wait for CI to finish) with an entry in state.waiting, which is
// the one case worth surfacing here even though the engine never marked it.
export function attentionCategory(it, block, pendingWait) {
  if (!it) return null;
  if (it.blocked) {
    const b = block || {};
    if (b.asked) return "question";
    if (b.kind === "dependency") return "dependency";
    if (b.kind === "limit") return "limit";
    if (b.kind === "turns" || b.kind === "unfinished" || b.kind === "worktree") return "waiting";
    return "failure";
  }
  return pendingWait ? "pending" : null;
}

// The one primary action a category's row offers, in the vocabulary the issue
// asks for. A failed check (block.kind === "checks") is the one failure with
// somewhere else to look — GitHub's own check run — so it reads "View failed
// check" rather than the generic "Retry stage" every other failure gets.
// Every label here names a control that already exists (the panel's Answer/
// hand-back, or an external link) — this never invents a new one.
export function attentionAction(category, block) {
  if (category === "question") return "Answer question";
  if (category === "failure") return (block || {}).kind === "checks" ? "View failed check" : "Retry stage";
  if (category === "dependency") return "View dependency";
  if (category === "limit") return "View limit";
  if (category === "waiting") return "View status";
  if (category === "pending") return "View progress";
  return "View";
}

// A plain array filter — no ordering rule of its own, no fetch — so this can
// never be mistaken for the scheduler's own order (pipeline.Order) or a write
// to provider state. `source` empty matches every source; `query` matches the
// title or the id, case-insensitively, against a plain substring.
export function filterItems(items, source, query) {
  const q = (query || "").trim().toLowerCase();
  return (items || []).filter(it => {
    if (source && it.source !== source) return false;
    if (!q) return true;
    return (it.title || "").toLowerCase().includes(q) || (it.id || "").toLowerCase().includes(q);
  });
}

// Reordering touches only visible items (#93): a drag or a Move up/down inside
// a filtered column must leave every hidden item's place in the saved manual
// order untouched. `fullIds` is one stage's complete, unfiltered id order
// before the move; `visibleIds` is the same stage's visible subset, in the
// order it now has after the move. Every position in `fullIds` that held one
// of `visibleIds`' members is replaced, in order, with the next id off
// `visibleIds` — so a hidden id keeps the exact index it already had, and only
// the visible ones are permuted, among the indices they already held.
export function applyVisibleOrder(fullIds, visibleIds) {
  const visible = new Set(visibleIds);
  let i = 0;
  return (fullIds || []).map(id => (visible.has(id) ? visibleIds[i++] : id));
}

// The empty text a working station shows under the filter (#93). A degraded
// board always wins — "the picture is incomplete" is a fact about discovery,
// true regardless of what is selected — and only when nothing is degraded
// does an active filter get to say whose items are missing, rather than
// reading as "the pipeline has nothing here at all".
export function stationEmptyText(degraded, sourceFilter) {
  if (degraded) return "Picture incomplete — discovery is degraded";
  if (sourceFilter) return `Nothing here from ${sourceFilter}`;
  return "Nothing here";
}
// ---- end pure helpers -------------------------------------------------------
