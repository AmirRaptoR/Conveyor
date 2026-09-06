import { $, esc } from "./dom.js";
import { nextQueueIndex, controlsForMode } from "./pure.js";
import { state, focusDescriptor, findFocusTarget } from "./shared.js";
import { blocks, tone, questionsOf, formatDuration, durSpan } from "./board.js";
import { queueOf, stageBy, startable, startItem, saveOrder, handBack } from "./drag.js";
import { openAsk, openReport } from "./report.js";

export let openItemId = null, openItemTitle = null, followRun = null;

// The card or `.need` button that opened the panel, as a focus descriptor
// rather than a node reference — the node itself is routinely replaced by a
// draw() while the panel sits open (see focusDescriptor/findFocusTarget), so
// looking it up again by identity when the panel closes is what makes
// "restore focus to the opener" mean anything after even one state update.
let panelOpenerDesc = null;
// One entry per item ever opened this session, keyed by id: the last block
// signature `refreshOpenStop` rendered for it, so a draw() that changes
// nothing about the mark does not blow away a reply someone is mid-typing —
// see refreshOpenStop.
const lastBlockSig = new Map();
const blockSig = b => b ? `${b.kind}|${b.reason}|${b.at}` : "";

export async function inspect(id, title, stage) {
  panelOpenerDesc = focusDescriptor(document.activeElement);
  openItemId = id; openItemTitle = title;
  // No run is selected yet: an SSE line for whatever was open before this
  // must not paint into a panel that has moved on to a different item.
  followRun = null; logPending = false; logBuffer = [];
  $("#ptitle").textContent = title || id;
  $("#psub").textContent = `${id} · ${stage}`;
  $("#panel").classList.add("open"); $("#panel").setAttribute("aria-hidden", "false");
  $("#scrim").classList.add("open");
  // Moves focus into the panel; the title itself, since it says what was just
  // opened rather than leading with the button that closes it again.
  $("#ptitle").focus();
  setLogStatus("loading", "Loading…");
  $("#todos").innerHTML = ""; $("#todos").hidden = true;
  lastBlockSig.delete(id);
  refreshOpenStop(id, true);
  renderPanelActions(id);
  // The report is derived from run history, so it exists for every item;
  // on one still moving it is the passage so far.
  const label = stageBy(stage)?.terminal ? "Final report" : "Report so far";
  $("#report").innerHTML = `<div class="report-head"><button class="ctl rbtn" data-label="${label}">${label}</button></div>`;
  $("#report .rbtn").onclick = e => openReport(id, title, e.target);
  await loadHistory(id);
}

// Redraws `#stop` only when the mark actually changed — a poll that finds the
// same mark still in place must not reset a reply someone is mid-typing in
// the textarea `stopNotice` renders. `isInitialOpen` skips the "just
// appeared" announcement on the render `inspect()` itself triggers; that one
// is the panel opening, not a stop appearing on an item already open.
export function refreshOpenStop(id, isInitialOpen) {
  const b = blocks[id];
  const sig = blockSig(b);
  if (!isInitialOpen && lastBlockSig.get(id) === sig) return;
  const hadMarkBefore = !!lastBlockSig.get(id);
  lastBlockSig.set(id, sig);
  $("#stop").innerHTML = stopNotice(id);
  const hand = $("#stop").querySelector(".hand");
  if (hand) hand.onclick = () => handBack(id, hand);
  const askBtn = $("#stop").querySelector(".ask-btn");
  if (askBtn) askBtn.onclick = () => openAsk(id, openItemTitle);
  if (!isInitialOpen && sig && !hadMarkBefore) {
    announce(`${openItemTitle || id} stopped: ${b.kind || "blocked"}`);
  }
}

// The keyboard/touch equivalents of the two drag gestures (#44), as ordinary
// buttons in the panel rather than chrome on every card — the panel is
// already the per-item surface a tap or a Tab reaches, and has the room a
// 236px card does not. Re-rendered from draw() as well as from inspect() so
// the buttons track the item's actual position and stage rather than the
// snapshot from whenever the panel opened.
export function renderPanelActions(id) {
  const box = $("#pactions");
  const card = document.querySelector(`.item[data-id="${CSS.escape(id)}"]`);
  if (!card) { box.innerHTML = ""; return; }
  const title = card.dataset.title, stage = card.dataset.stage;
  const parts = [];
  const q = queueOf(card);
  if (q) {
    const items = queueEls(q);
    const idx = items.indexOf(card);
    parts.push(`<button class="ctl" data-act="up" ${idx <= 0 ? "disabled" : ""}
        aria-label="Move ${esc(title)} up in ${esc(q)}">Move up</button>`);
    parts.push(`<button class="ctl" data-act="down" ${idx < 0 || idx >= items.length - 1 ? "disabled" : ""}
        aria-label="Move ${esc(title)} down in ${esc(q)}">Move down</button>`);
  }
  const first = (state?.stages || [])[0];
  const into = stageBy(stage)?.next;
  if (first && stage === first.name && startable(stage, into)) {
    parts.push(`<button class="ctl" data-act="start" aria-label="Start ${esc(title)}">Start</button>`);
  }
  box.innerHTML = parts.join("");
  box.querySelectorAll("[data-act]").forEach(btn => {
    btn.onclick = () => {
      if (btn.dataset.act === "start") startItem(id, into);
      else moveItem(id, btn.dataset.act === "up" ? -1 : 1);
    };
  });
}

const queueEls = stage => [...document.querySelectorAll(`.queue[data-stage="${CSS.escape(stage)}"] .item`)];

// Mutates the DOM the same way `ondrop`'s `insertBefore` does, then goes
// through the same `saveOrder()` the drag uses — never `justDragged`, which
// exists only to suppress the click a drop generates and would otherwise
// swallow the very next Enter/Space on this card.
function moveItem(id, dir) {
  const card = document.querySelector(`.item[data-id="${CSS.escape(id)}"]`);
  if (!card) return;
  const stage = queueOf(card);
  if (!stage) return;
  const items = queueEls(stage);
  const from = items.indexOf(card);
  const to = nextQueueIndex(from, items.length, dir);
  if (to === null) return;
  card.parentNode.insertBefore(card, dir < 0 ? items[to] : items[to].nextSibling);
  saveOrder();
  renderPanelActions(id);
  announce(`${card.dataset.title || id} moved to position ${to + 1} of ${items.length} in ${stage}`);
}

// The one polite live region left once #log and #history stop being
// `aria-live` (see #44): a keyboard/touch reorder, a run-level status change,
// and a stop appearing on the item already open all land here instead.
export function announce(text) { $("#announcer").textContent = text; }

// The whole reason, where there is room for it, and the control that answers
// it. Shown only for a marked item: everything else on the board is moving.
function stopNotice(id) {
  const b = blocks[id];
  if (!b) return "";
  const at = b.at ? new Date(b.at) : null;
  const qs = questionsOf(b);
  const t = tone(b);
  // Answering is unblocking (CONTRACTS §6), which observe refuses at the
  // route like every other mutation — so neither control is offered there.
  const canHandBack = controlsForMode(state?.mode || "auto").handBack;
  return `<div class="stop ${t}">
    <h3>${t === "asks" ? "needs you · " : t === "waiting" ? "waiting · " : ""}${esc(b.kind || "blocked")}${b.stage ? ` in ${esc(b.stage)}` : ""}</h3>
    <p>${esc(b.reason || "no reason was recorded")}</p>
    <div class="meta">
      ${at && !isNaN(at) ? `stopped ${esc(at.toLocaleString())}` : ""}
      ${b.runId ? ` &middot; run ${esc(b.runId)}` : ""}
    </div>
    <div class="fault" hidden></div>
    ${canHandBack && qs ? `<button class="ask-btn">Answer ${qs.length === 1 ? "the question" : `${qs.length} questions`}</button>` : ""}
    ${canHandBack ? `<textarea class="answer" rows="3" spellcheck="true" aria-label="Reply"
      placeholder="${qs ? "Anything to add beside the answers above, or reply here instead."
                       : t === "asks" ? "Your answer. The agent takes it up in the conversation that stopped."
                       : "Reply if there is something to say; leave it empty to just hand the item back."}"></textarea>
    <button class="hand"
      title="Clear the mark; the item goes straight back into the line">${t === "asks" ? "Send reply" : "Hand back"}</button>` : ""}
  </div>`;
}

function setHistoryStatus(kind, text) {
  $("#history").innerHTML =
    `<div class="slot${kind === "error" ? " err" : kind === "loading" ? " loading" : ""}">${esc(text)}</div>`;
  announce(text);
}

// A failed history fetch leaves no run to select — the log would otherwise
// sit on "Loading…" forever, an artifact with nothing left to resolve it.
function historyFailed(id, msg) {
  setHistoryStatus("error", msg);
  if (followRun === null) setLogStatus("idle", "");
}

// `openItemId === id` alone cannot tell apart the first open of an item from
// a later re-open of the same item — opening A, then B, then A again leaves
// two in-flight fetches that both satisfy that check, so the id equality
// used to matter but is not enough on its own to make the second one "the
// current one". `historyGen` is a call counter: only the fetch started by
// the most recent loadHistory() for the current item is allowed to render.
let historyGen = 0;

export async function loadHistory(id) {
  const gen = ++historyGen;
  const stale = () => openItemId !== id || gen !== historyGen;
  setHistoryStatus("loading", "Loading…");
  let runs;
  try {
    const res = await fetch(`/api/runs?item=${encodeURIComponent(id)}`);
    // The panel may have moved on to another item, or a newer call for this
    // same item may already be in flight, while this was in flight.
    if (stale()) return;
    if (!res.ok) { historyFailed(id, `Could not load run history (HTTP ${res.status}).`); return; }
    runs = await res.json();
  } catch {
    if (stale()) return;
    historyFailed(id, "Could not load run history."); return;
  }
  if (stale()) return;
  if (!Array.isArray(runs)) { historyFailed(id, "Could not load run history."); return; }
  $("#history").innerHTML = runs.length ? runs.map(r => `
    <button class="tr ${esc(r.outcome)}" data-run="${esc(r.id)}">
      <span class="when">${esc((r.startedAt || "").slice(11, 19))}</span>
      <span class="kind">${esc(r.kind)}</span>
      <span class="path">${transition(r)}</span>
      ${runDuration(r)}
      <span class="verdict">${esc(r.outcome)}</span>
    </button>`).join("")
    : `<div class="slot">No runs yet.</div>`;
  document.querySelectorAll("#history [data-run]").forEach(b => b.onclick = () => showRun(b.dataset.run));
  if (runs.length) showRun(runs[0].id);
  // Never leave the log claiming "No output." for a run that was never
  // selected — an item that genuinely has no history reads as exactly that
  // in both regions.
  else { followRun = null; setLogStatus("idle", "No runs yet."); }
}

// A run whose from and to are the same stage is not a move. For a stage script
// it is a re-run of work that did not finish; for a provider write it is the
// engine confirming the item is where it already says it is. Printing
// "refining → refining" for both reads like a bug.
function transition(r) {
  const from = esc(r.from || ""), to = esc(r.to || "");
  if (!from && !to) return `<span class="arrow">poll</span>`;
  if (from === to) {
    return r.kind === "stage"
      ? `<span class="arrow">&#8635;</span> re-ran ${to}`
      : `<span class="arrow">&#8801;</span> confirmed ${to}`;
  }
  return `${from} <span class="arrow">&rarr;</span> ${to}`;
}

// A finished run's own cost, from durationNs; a still-running one has no
// duration yet, so it counts up from its own startedAt instead, the same way
// the working tag does.
function runDuration(r) {
  if (r.outcome === "running") return durSpan(new Date(r.startedAt).getTime(), false);
  return `<span class="dur">${esc(formatDuration((r.durationNs || 0) / 1e6))}</span>`;
}

export function setLogStatus(kind, text) {
  const log = $("#log");
  log.innerHTML = `<div class="status${kind === "error" ? " err" : kind === "loading" ? " loading" : ""}">${esc(text)}</div>`;
  announce(text);
}

// SSE lines for the followed run that arrive while its own fetch is still in
// flight cannot just be appended — the snapshot they will land after has not
// rendered yet, and it may itself already carry the very same line. They are
// buffered here and joined once the snapshot is in, in appendBuffered().
// `logGen` scopes logPending/logBuffer to whichever showRun() call is current,
// so a superseded call finishing late cannot clear a newer call's buffer.
export let logGen = 0, logPending = false, logBuffer = [];

// A live run can emit far more than a browser should keep as DOM nodes.
// MAX_LOG_LINES bounds how many stay rendered; the oldest are discarded as
// new ones arrive, and a single marker node (never one per discard) says how
// many. The full log is never lost — it is still on disk, and reloading this
// run through the paged GET /api/runs/{id} API reaches it.
const MAX_LOG_LINES = 5000;
let logDiscarded = 0, logMarker = null;

export function trimLog(log) {
  const floor = logMarker ? 1 : 0;
  while (log.childElementCount > MAX_LOG_LINES + floor) {
    const victim = log.children[logMarker ? 1 : 0];
    if (!victim) break;
    log.removeChild(victim);
    logDiscarded++;
  }
  if (logDiscarded > 0) {
    if (!logMarker) {
      logMarker = document.createElement("div");
      logMarker.className = "ln status";
      log.insertBefore(logMarker, log.firstChild);
    }
    // Not "reload to see them all": the default GET /api/runs/{id} a reload
    // makes returns only the last 2000 lines, which is fewer than what a DOM
    // already past MAX_LOG_LINES is showing — a plain reload would lose more
    // than it recovers. The lines are not gone (log.txt has them, and the
    // paged API can reach them with ?offset=), but nothing here pages for
    // them yet, so the marker must not promise that a reload does.
    logMarker.textContent = `… ${logDiscarded} earlier line(s) dropped from this view to bound memory; still complete in the run's own log …`;
  }
}

async function showRun(id) {
  followRun = id;
  const gen = ++logGen;
  // `followRun === id` alone cannot tell apart the first click on a run from
  // a later re-click on the same run — A, then B, then A again leaves two
  // in-flight fetches that both satisfy that check. `gen` is what actually
  // orders the calls: only the fetch started by the most recent showRun()
  // is allowed to render or touch logPending/logBuffer.
  const stale = () => followRun !== id || gen !== logGen;
  logPending = true;
  logBuffer = [];
  document.querySelectorAll("#history [data-run]").forEach(b => b.classList.toggle("sel", b.dataset.run === id));
  setLogStatus("loading", "Loading…");
  let lines;
  try {
    const res = await fetch(`/api/runs/${id}`);
    // The reader moved to another run, closed the panel, or re-clicked this
    // same run again while this was in flight — nothing of this response
    // belongs on screen any more.
    if (stale()) return;
    if (res.status === 410) {
      // Genuinely honest but coarse (CONTRACTS §6): this cannot prove *this*
      // run was swept rather than never existing, only that runs before the
      // horizon are gone — never to be confused with the old 500-run lookup
      // cutoff, which just meant "we didn't look far enough".
      const body = await res.json().catch(() => ({}));
      setLogStatus("error", `Removed by retention on ${body.horizon || "an earlier date"}.`);
      logPending = false;
      return;
    }
    if (!res.ok) {
      setLogStatus("error", `Could not load this run (HTTP ${res.status}).`);
      logPending = false;
      return;
    }
    const r = await res.json();
    lines = r.lines || [];
  } catch {
    if (stale()) return;
    setLogStatus("error", "Could not load this run.");
    logPending = false;
    return;
  }
  if (stale()) return;
  const log = $("#log");
  log.innerHTML = "";
  // A newly opened run starts its DOM cap fresh — the marker and count both
  // belonged to whichever run was open before.
  logDiscarded = 0; logMarker = null;
  // Reset before replaying: a run with no TodoWrite call of its own should
  // not keep showing whichever run was open before it. renderLine repopulates
  // this as it walks the lines (and any buffered lines appendBuffered adds
  // after), so it ends on that run's own latest state.
  $("#todos").innerHTML = ""; $("#todos").hidden = true;
  for (const line of lines) { log.appendChild(renderLine(line)); trimLog(log); }
  logPending = false;
  appendBuffered(lines);
  if (!log.childElementCount) setLogStatus("empty", "No output.");
  else log.scrollTop = log.scrollHeight;
}

// Join the snapshot with whatever arrived over SSE while it was in flight.
// `LogLine` carries no sequence number, so a buffered line can only be told
// apart from one the snapshot already contains by comparing it, position by
// position, against the snapshot's tail — dropping the longest run of
// buffered lines that matches there and keeping the rest. A global
// de-duplication would be wrong: agent output legitimately repeats identical
// lines.
function appendBuffered(snapshotLines) {
  const log = $("#log");
  const buf = logBuffer;
  logBuffer = [];
  const same = (a, b) => a && b && a.stream === b.stream && a.text === b.text && a.timestamp === b.timestamp;
  let overlap = 0;
  for (let m = Math.min(buf.length, snapshotLines.length); m > 0; m--) {
    let match = true;
    for (let i = 0; i < m; i++) {
      if (!same(buf[i], snapshotLines[snapshotLines.length - m + i])) { match = false; break; }
    }
    if (match) { overlap = m; break; }
  }
  for (let i = overlap; i < buf.length; i++) { log.appendChild(renderLine(buf[i])); trimLog(log); }
}

// An agent writes markdown and calls tools; showing that as one monospace blob
// throws away everything it told you about shape. Presentation only — the
// engine still reads no data out of a log, it comes back in $CONVEYOR_RESULT.
export function renderLine(line) {
  const el = document.createElement("div");
  const text = line.text ?? "";
  el.className = "ln " + (line.stream === "stderr" ? "stderr"
                        : line.stream === "engine" ? "engine" : "");

  if (!text.trim()) { el.classList.add("blank"); return el; }

  // "  · Bash: git status" — the shape agents/claude/_stream emits.
  const tool = text.match(/^\s*·\s*([A-Za-z_]+):\s?([\s\S]*)$/);
  if (tool) {
    el.classList.add("tool");
    if (tool[1] === "TodoWrite") {
      const list = parseTodos(tool[2]);
      if (list) {
        updateTodos(list);
        const done = list.filter(t => t.status === "completed").length;
        const active = list.find(t => t.status === "in_progress");
        const gist = `${done}/${list.length} done` + (active ? ` · ${active.activeForm || active.content}` : "");
        el.innerHTML = `<span class="tname">TodoWrite</span> <span class="targ">${esc(gist)}</span>`;
        return el;
      }
      // Falls through to the generic rendering below on a shape this does
      // not recognise — the sticky panel just misses this update.
    }
    el.innerHTML = `<span class="tname">${esc(tool[1])}</span> <span class="targ">${esc(tool[2])}</span>`;
    return el;
  }
  if (/^\s*(-{3,}|={3,})\s*$/.test(text)) { el.className = "ln rule"; return el; }
  if (/^#{1,6}\s/.test(text)) {
    el.classList.add("head");
    el.innerHTML = inline(text.replace(/^#{1,6}\s/, ""));
    return el;
  }
  const bullet = text.match(/^(\s*)([-*·])\s+([\s\S]*)$/);
  if (bullet) {
    el.innerHTML = `${bullet[1]}<span class="bullet">${esc(bullet[2])}</span> ${inline(bullet[3])}`;
    return el;
  }
  el.innerHTML = inline(text);
  return el;
}

// The little markdown that actually appears in agent output. Escaped first, so
// nothing a repository contains can inject markup into the board.
export function inline(s) {
  return esc(s)
    .replace(/`([^`]+)`/g, (_, c) => `<code>${c}</code>`)
    .replace(/\*\*([^*]+)\*\*/g, (_, b) => `<strong>${b}</strong>`);
}

// TodoWrite's arg is the plan itself, sent whole (see agents/claude/_stream),
// not a path or a command truncated to a preview — so it is the one tool line
// here worth parsing back into structure rather than just displaying. Defensive
// by construction: any shape this does not recognise returns null and the
// caller falls back to the plain tool-line rendering every other tool gets.
function parseTodos(raw) {
  let list;
  try { list = JSON.parse(raw); } catch { return null; }
  if (!Array.isArray(list)) return null;
  if (!list.every(t => t && typeof t.content === "string" && typeof t.status === "string")) return null;
  return list;
}

// The agent's own plan, kept current in the sticky panel above the log.
// Presentation only, like everything else renderLine does — this reads
// nothing back into $CONVEYOR_RESULT or the engine.
function updateTodos(list) {
  const box = $("#todos");
  if (!list.length) { box.innerHTML = ""; box.hidden = true; return; }
  box.innerHTML = list.map(t => {
    const mark = t.status === "completed" ? "✔" : t.status === "in_progress" ? "▶" : "○";
    const label = t.status === "in_progress" ? (t.activeForm || t.content) : t.content;
    return `<div class="todo ${esc(t.status)}"><span class="mark">${mark}</span><span>${esc(label)}</span></div>`;
  }).join("");
  box.hidden = false;
}

export function closePanel() {
  $("#panel").classList.remove("open"); $("#panel").setAttribute("aria-hidden", "true");
  $("#scrim").classList.remove("open");
  if (openItemId) lastBlockSig.delete(openItemId);
  openItemId = null; openItemTitle = null; followRun = null;
  logPending = false; logBuffer = [];
  // Back to whichever card or `.need` button opened the panel, by identity —
  // looked up fresh rather than a stored node, since a draw() while the panel
  // was open has very likely already replaced it (see focusDescriptor).
  // Nothing named if that element is no longer on the board at all.
  const opener = findFocusTarget(panelOpenerDesc);
  panelOpenerDesc = null;
  if (opener) opener.focus();
}
$("#close").onclick = closePanel;
$("#scrim").onclick = closePanel;
// The `#ask` dialog sits in front of the panel and closes itself on Escape
// (native <dialog> behaviour) — this listener must leave that alone, or the
// panel behind it closes too and the criterion "Escape closes only the
// dialog" breaks the moment this handler also fires for the same keypress.
addEventListener("keydown", e => {
  if (e.key !== "Escape") return;
  if ($("#ask").open) return;
  closePanel();
});
