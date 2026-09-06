import { $, esc } from "./dom.js";
import { shouldDeferDraw, sourceDegraded } from "./pure.js";
import { state, clock, fmtBytes, nowMs, focusDescriptor, findFocusTarget } from "./shared.js";
import { updateRailCtl } from "./rail.js";
import { openFromHash } from "./device.js";
import { openItemId, inspect, renderPanelActions, refreshOpenStop } from "./panel.js";
import { dragging, justDragged, refusals, stageBy, wireDrag, wireQueue } from "./drag.js";

// A draw() that lands mid-drag defers instead of touching #rail (see draw()
// and shouldDeferDraw above); the drag's own dragend runs the one redraw that
// was owed once it is safe to rebuild again.
let pendingRedraw = false;
// Why each marked item is marked, keyed by item id — the engine's note, not the
// provider's. See State.Blocks.
export let blocks = {};

let firstDraw = true;
export function draw() {
  if (!state) return;
  // A draw() mid-drag would tear out the very node the drag is holding onto
  // (see wireDrag) — deferred here, and run once, exactly, from dragend.
  // `state` above is already current; only the rebuild waits.
  if (shouldDeferDraw(dragging)) { pendingRedraw = true; return; }
  if (firstDraw) { firstDraw = false; queueMicrotask(openFromHash); }

  // Interaction state that lives in a subtree #rail/#needs are about to throw
  // away by innerHTML assignment: what had focus (a card, its `.open` link, a
  // `.more` button, a `.need` button), and which agents' own `<details>` are
  // open. `shown` (below) and the rail's own scrollLeft already survive this
  // for the same reason — neither lives in the DOM this replaces.
  const active0 = document.activeElement;
  const focusDesc = active0 && active0.closest?.("#rail, #needs") ? focusDescriptor(active0) : null;
  const openAgents = new Set(
    [...document.querySelectorAll("#agents details[open]")].map(d => d.dataset.agent));

  const stages = state.stages || [], items = state.items || [];
  indexSources(state.sources);
  // A list, because stages run in parallel: one entry per transition in flight.
  const active = state.active || [];
  const workingOn = id => active.find(a => a.itemId === id);
  blocks = state.blocks || {};

  // Degraded describes a source, not the board: at least one refresh has
  // completed and that source's own listing has failed, never happened, or
  // is older than the stale threshold — see sourceDegraded above. Computed
  // fresh from `state` on every draw, never accumulated, so a clean state
  // right after a degraded one leaves nothing behind.
  const updatedAtMs = state.updatedAt ? new Date(state.updatedAt).getTime() : 0;
  const degradedSources = (state.sources || []).filter(s => sourceDegraded(s, updatedAtMs, state.pollNs, nowMs()));

  // Items arrive in the order the scheduler will work them, so the board does
  // not sort: it draws them in the order it is given. A card's position is the
  // claim it makes, and re-deriving that here would be a second copy of rules
  // that live in the engine, free to disagree with it.
  const bucket = {};
  for (const st of stages) bucket[st.name] = [];
  for (const it of items) {
    // The item list is from the last poll; `active` is from now. An item being
    // worked has already been moved, so show it in the stage it is being worked
    // in — otherwise it sits in its old column flagged as working, which is
    // true of two different moments and reads as a bug.
    const a = workingOn(it.id);
    (bucket[a ? a.stage : it.stage] ||= []).push(it);
  }
  // The line runs through the working stages; terminal ones leave it.
  const flow = stages.filter(s => !s.terminal);
  const ends = stages.filter(s => s.terminal);

  $("#rail").innerHTML =
    flow.map(st => station(st, bucket[st.name] || [], active, degradedSources.length > 0)).join("") +
    ends.map(st => terminus(st, bucket[st.name] || [])).join("");

  // What each agent says about itself. The engine passes this through — which
  // limits an agent has and what counts against them is the agent's business —
  // so the page renders whatever labels came back, in the order they came.
  // Being limited and being paused are two different facts and the strip says
  // both: the first is what the agent reports about itself, the second is what
  // the scheduler is doing about it. They usually agree, but the first refused
  // run pauses an agent a whole poll before its status script catches up — and
  // a paused line and an idle line look identical without this.
  const paused = Object.fromEntries((state.paused || []).map(p => [p.agent, p]));
  $("#agents").innerHTML = !(state.agents || []).length ? "" :
    `<div class="agents">${state.agents.map(a => {
      const facts = (a.detail || []).map(d =>
        `<span><b>${esc(d.label)}</b> ${esc(d.value)}</span>`).join("");
      const say = a.error ? `cannot say — ${esc(a.error)}` : esc(a.summary || "");
      const hold = paused[a.name];
      return `<span class="agent ${esc(a.state || "unknown")}${hold ? " paused" : ""}">
        <span class="lamp ${a.state === "limited" || hold ? "down" : a.state === "ok" ? "idle" : ""}"></span>
        <span class="who">${esc(a.name)}</span>
        <span class="say">${say}</span>
        ${hold ? `<span class="held">holding work back${hold.until ? ` until ${esc(clock(hold.until))}` : ""}</span>` : ""}
        ${a.resetsAt ? `<span class="resets">resets ${esc(clock(a.resetsAt))}</span>` : ""}
        ${facts ? `<details data-agent="${esc(a.name)}"><summary>details</summary><span class="facts">${facts}</span></details>` : ""}
      </span>`;
    }).join("")}</div>`;

  // Handing the whole board back is the answer to one situation — the thing
  // that stopped everything is fixed — so the control only exists then.
  const marked = items.filter(it => it.blocked).length;
  const asking = items.filter(it => it.blocked && (blocks[it.id] || {}).asked);
  $("#needs").innerHTML = !asking.length ? "" :
    `<span class="nlabel">${asking.length} need${asking.length === 1 ? "s" : ""} your decision</span>` +
    asking.map(it => `<button class="need" data-id="${esc(it.id)}" data-title="${esc(it.title)}"
      data-stage="${esc(it.stage)}" title="${esc(it.title)}">${esc(it.title)}</button>`).join("");
  const all = $("#unblock-all");
  all.hidden = !marked;
  all.textContent = `Unblock all (${marked})`;
  // Diagnose reads a reason before acting, so it is offered wherever a mark
  // exists to read — the same condition as the blunter Unblock all.
  $("#diagnose").hidden = !marked;

  // A key for the edge colours, and the list of what is enrolled: without it
  // the colours are decoration, because nothing says which repo is which.
  const counts = {};
  for (const it of items) counts[it.source] = (counts[it.source] || 0) + 1;
  // How much the run store holds and how far back retention still reaches —
  // one figure, in the same strip as what is enrolled, because both answer
  // "what does this board's disk footprint look like right now".
  const storage = state.storage;
  const storageChip = storage && storage.runs ? `<span class="src-chip storage" title="run store on disk">
        <span class="n">${fmtBytes(storage.bytes)}</span>
        <span class="repo">${storage.runs} run${storage.runs === 1 ? "" : "s"}${
          storage.oldestDay ? ` retained since ${esc(storage.oldestDay)}` : ""}</span>
      </span>` : "";
  $("#sources").innerHTML = `<div class="sources">${
    (state.sources || []).map(s => {
      const broken = (s.problems || []).length > 0;
      const degraded = !broken && degradedSources.includes(s);
      // A broken source shows only its cannot-run badge — never additionally
      // reported as degraded, stale or not-listing; that is what its own
      // configuration problems already mean.
      const freshness = broken ? "" : s.lastListedAt
        ? durSpan(new Date(s.lastListedAt).getTime(), true, esc(`listed ${new Date(s.lastListedAt).toLocaleString()}`))
        : `<span class="never">not listed yet</span>`;
      const chipTitle = esc((s.workdir || "") + (degraded && s.listError ? ` — ${s.listError}` : ""));
      return `<span class="src-chip${broken ? " broken" : ""}${degraded ? " degraded" : ""}" style="--src:${sourceColour(s.name)}"
          title="${chipTitle}">
        <span class="swatch"></span>
        <span class="name">${esc(s.name)}</span>
        <span class="n">${degraded ? "?" : (counts[s.name] || 0)}</span>
        ${broken ? `<span class="repo">cannot run</span>` : freshness ? `<span class="repo">${freshness}</span>` : ""}
      </span>`;
    }).join("") + storageChip
  }</div>`;

  const bad = (state.sources || []).filter(s => (s.problems || []).length);
  // A persistence fault is not a source problem — it is a fact about the run
  // store itself — but it is the same tone (red, sticky, needs a person) and
  // belongs beside them rather than inventing a second strip for one line.
  const pf = state.persistFault;
  $("#faults").innerHTML = (bad.length || pf) ? `<div class="faults">
      ${bad.length ? `<h2>${bad.length} source${bad.length > 1 ? "s" : ""} cannot run</h2>
      ${bad.map(s => `<div><b>${esc(s.name)}</b> — ${s.problems.map(esc).join("<br>")}</div>`).join("")}` : ""}
      ${pf ? `<h2>run store cannot record itself</h2>
      <div><b>${esc(pf.runId)}</b>${pf.itemId ? ` (${esc(pf.itemId)})` : ""} — ${esc(pf.message)}</div>` : ""}
    </div>` : "";

  // Runtime conditions the engine noticed while discovering work — a listing
  // that errored, or a warning the provider itself returned — never a
  // configuration error (that is .faults, above). Rendered whole: the
  // engine's own `<source>: ` prefix is what attributes each one, so the
  // page does not split, parse or re-key the string.
  $("#warnings").innerHTML = (state.warnings || []).length ? `<div class="warnings">
      ${state.warnings.map(w => `<div>${esc(w)}</div>`).join("")}
    </div>` : "";

  $("#lamp").className = "lamp " + (active.length || state.polling ? "busy" : "idle");
  const when = state.updatedAt ? new Date(state.updatedAt).toLocaleTimeString([], {hour:"2-digit", minute:"2-digit"}) : "—";
  const held = marked;
  // Named beside updated <time> so the line does not read fresh while
  // discovery is broken — appended to whichever branch below renders, since
  // that is the same masthead line regardless of what else is happening.
  const degradedNote = degradedSources.length
    ? ` &nbsp;·&nbsp; <span class="degraded">${degradedSources.length} source${degradedSources.length === 1 ? "" : "s"} not listing</span>`
    : "";
  $("#line1").innerHTML = (active.length === 1
    ? `<b>${esc(active[0].stage)}</b> running &middot; ${esc(active[0].itemId)}`
    : active.length
      ? `<b>${active.length}</b> running &middot; ${active.map(a => esc(a.stage)).join(", ")}`
      : `<b>${items.length}</b> items${asking.length ? `, <b>${asking.length}</b> need${asking.length === 1 ? "s" : ""} you`
          : held ? `, <b>${held}</b> stopped` : ""} &nbsp;·&nbsp; updated ${when}`) + degradedNote;

  document.querySelectorAll(".item").forEach(el => {
    const open = () => { if (!justDragged) inspect(el.dataset.id, el.dataset.title, el.dataset.stage); };
    el.onclick = e => { if (!e.target.closest("a")) open(); };
    el.onkeydown = e => {
      if ((e.key === "Enter" || e.key === " ") && !e.target.closest("a")) { e.preventDefault(); open(); }
    };
    wireDrag(el);
  });
  document.querySelectorAll(".queue[data-stage]").forEach(wireQueue);
  updateRailCtl();

  // Put open `<details>` back the way they were before #agents was replaced.
  for (const name of openAgents) {
    const d = document.querySelector(`#agents details[data-agent="${CSS.escape(name)}"]`);
    if (d) d.open = true;
  }
  // Put focus back on the same logical element, by identity rather than by
  // node reference — the node itself is gone, #rail/#needs were just rebuilt
  // wholesale. Fallback (when the element named is no longer on the board,
  // e.g. an item that changed stage or a `.more`/`.need` button that
  // disappeared): the rail itself, so focus lands somewhere still on the
  // board rather than resetting to the top of the document.
  if (focusDesc) {
    const target = findFocusTarget(focusDesc);
    if (target) target.focus();
    else if (!$("#rail").contains(document.activeElement)) $("#rail").focus();
  }

  // The panel's own action controls (move up/down, start) and stop notice
  // read the same rebuilt #rail, so they are kept in step with every draw()
  // rather than only when the panel first opens.
  if (openItemId) { renderPanelActions(openItemId); refreshOpenStop(openItemId, false); }
}

// draw()'s own deferral flag lives here with it; the drag's dragend (drag.js)
// runs the redraw that was owed through this, because one module cannot assign
// to another module's binding.
export function flushPendingRedraw() { if (pendingRedraw) { pendingRedraw = false; draw(); } }

// `degraded` is whether any source is degraded right now (draw()'s
// board-wide count, not this station's own items) — a column can be empty
// because its source's items vanished from discovery rather than because
// there is genuinely no work, and a reader needs to be able to tell those
// apart. Terminal columns (terminus(), below) never take this: they are a
// ledger of closed work, not a claim about what is in flight.
function station(st, items, active, degraded) {
  const live = active.some(a => a.stage === st.name);
  return `<section class="station${live ? " live" : ""}">
    <div class="plate">
      <div class="name">${esc(st.name)}${tally(items)}</div>
      <div class="runs-label">${st.script ? `runs <b>${esc(st.script)}</b>` : st.runs ? "runs a script" : "waits"}</div>
    </div>
    <div class="queue" data-stage="${esc(st.name)}">${
      items.length ? items.map((it, i) => card(it, active, i)).join("")
                   : `<div class="slot">${degraded ? "Picture incomplete — discovery is degraded" : "Nothing here"}</div>`}</div>
  </section>`;
}

// Ten at a time: a done column is a ledger, and the last ten entries are the
// ones anyone reads. The rest are one press away. Per column, kept across
// redraws so an SSE update does not fold the list back up mid-read.
const shown = {};
function terminus(st, items) {
  const n = shown[st.name] || 10;
  const left = items.length - n;
  return `<section class="terminus">
    <div class="plate">
      <div class="name">${esc(st.name)}${tally(items)}</div>
      <div class="runs-label">Finished</div>
    </div>
    <div class="queue">${items.slice(0, n).map(it => card(it, [], null)).join("")}${
      left > 0 ? `<button class="more" data-stage="${esc(st.name)}">Show ${Math.min(10, left)} more · ${left} older</button>` : ""}</div>
  </section>`;
}
document.addEventListener("click", e => {
  const more = e.target.closest(".more");
  if (more) { shown[more.dataset.stage] = (shown[more.dataset.stage] || 10) + 10; draw(); return; }
  const need = e.target.closest(".need");
  if (need) inspect(need.dataset.id, need.dataset.title, need.dataset.stage);
});

// How many are here, and how many of those are not moving. Blocked is no longer
// a column to look at, so the count is where a stage says it is holding
// something a person has to deal with.
function tally(items) {
  const asks = items.filter(it => it.blocked && (blocks[it.id] || {}).asked).length;
  const held = items.filter(it => it.blocked).length - asks;
  return `<span class="count">${items.length}${
    asks ? ` &middot; <span class="asks">${asks} need${asks === 1 ? "s" : ""} you</span>` : ""}${
    held ? ` &middot; <span class="held">${held} stopped</span>` : ""}</span>`;
}

// Mirrors report.go's formatDuration exactly: at most two units, the
// smaller dropped when it is zero. Never negative — clock skew or a
// timestamp from the future reads as "<1s", not as a negative number.
export function formatDuration(ms) {
  if (!(ms > 0)) return "<1s";
  let s = Math.floor(ms / 1000);
  if (s < 1) return "<1s";
  const days = Math.floor(s / 86400); s -= days * 86400;
  const hours = Math.floor(s / 3600); s -= hours * 3600;
  const mins = Math.floor(s / 60); s -= mins * 60;
  if (days > 0) return hours > 0 ? `${days}d ${hours}h` : `${days}d`;
  if (hours > 0) return mins > 0 ? `${hours}h ${mins}m` : `${hours}h`;
  if (mins > 0) return `${mins}m`;
  return `${s}s`;
}

// A span carrying a start instant, ticked in place by tickDurations() — never
// by draw(), so a second of drift cannot interrupt a drag, collapse an
// expanded done column, or reset the rail's scroll position. `titleAttr` is
// already escaped by the caller, since it is assembled from more than one
// piece of free text.
export function durSpan(startMs, ago, titleAttr) {
  if (!(startMs > 0) || isNaN(startMs)) return "";
  const text = formatDuration(nowMs() - startMs) + (ago ? " ago" : "");
  return `<span class="dur"${ago ? ' data-ago="1"' : ""} data-start="${startMs}"${
    titleAttr ? ` title="${titleAttr}"` : ""}>${esc(text)}</span>`;
}

// How long an item has been in the stage it is in — the one duration a card
// has room for. Nothing is drawn when there is no entry: a swept or
// never-recorded arrival is "no chip", never a placeholder, a dash or a zero.
function ageChip(it) {
  const t = state.times && state.times[it.id];
  if (!t) return "";
  const startMs = new Date(t.enteredStage).getTime();
  if (isNaN(startMs)) return "";
  const terminal = !!stageBy(it.stage)?.terminal;
  const title = `in ${esc(t.stage)} since ${esc(new Date(t.enteredStage).toLocaleString())}`;
  return `<span class="age">${durSpan(startMs, terminal, title)}</span>`;
}

function card(it, active, place) {
  // Number, or nothing. `undefined !== null` is true, so a missing argument
  // used to render "NaN" in the rank badge rather than omitting it.
  const ranked = Number.isInteger(place);
  const hasPrio = it.priority !== null && it.priority !== undefined;
  const inHand = active.find(a => a.itemId === it.id);
  const working = !!inHand;
  const cls = ["item", hasPrio ? "p" + it.priority : "", working ? "working" : "",
               it.blocked ? "blocked " + tone(blocks[it.id]) : "", ranked ? "ranked" : ""].filter(Boolean).join(" ");
  return `<article class="${cls}" draggable="true" tabindex="0" role="button"
      style="--src:${sourceColour(it.source)}"
      data-id="${esc(it.id)}" data-title="${esc(it.title)}" data-stage="${esc(it.stage)}">
    <span class="title">${esc(it.title)}</span>
    <span class="foot">
      ${working ? `<span class="working-tag">working ${durSpan(new Date(inHand.startedAt).getTime(), false)}</span>` : ""}
      ${it.blocked ? why(it) : ""}
      ${ranked && !working && !it.blocked ? `<span class="rank">${place + 1}</span>` : ""}
      ${hasPrio ? `<span class="prio">p${it.priority}</span>` : ""}
      ${ageChip(it)}
      <span class="src">${esc(it.id)}</span>
      ${it.url ? `<a class="open" href="${esc(it.url)}" target="_blank" rel="noopener"
           title="Open on GitHub" aria-label="Open ${esc(it.id)} on GitHub">&#8599;</a>` : ""}
    </span>
    ${refusals.has(it.id) ? `<span class="refused">${esc(refusals.get(it.id))}</span>` : ""}
  </article>`;
}

// What kind of stop it was, in one word, and nothing else. The reason is a
// paragraph and belongs in the panel: a column of paragraphs is a column you
// cannot read across, and reading across is what a board is for. Telling "out
// of quota" from "someone has to decide something" at a glance is the whole job
// here — the click is for the rest.
function why(it) {
  const b = blocks[it.id] || {};
  return `<span class="why ${tone(b)}">${b.asked ? "needs you · " : ""}${esc(b.kind || "blocked")}</span>`;
}

// Three tones, from the two facts the engine carries: a question is `asked`,
// and these kinds are conditions that pass on their own (agents/_blocked's
// vocabulary — presentation only, the engine never reads the word). Anything
// else stopped because something went wrong.
const WAITING = new Set(["limit", "turns", "unfinished", "dependency", "worktree"]);
export function tone(b) {
  b = b || {};
  return b.asked ? "asks" : WAITING.has(b.kind) ? "waiting" : "fault";
}

// The questions an agent left beside its reason, if they are the shape the
// modal can walk; anything else is shown as the paragraph it always was.
export function questionsOf(b) {
  const qs = b && b.questions;
  return Array.isArray(qs) && qs.length && qs.every(q => q && typeof q === "object") ? qs : null;
}

// One hue per source, spread evenly around the wheel rather than hashed from
// the name: hashing put midgame, quesshi and caravan-v2 within 21 degrees of
// each other, which reads as one colour. Position gives maximal separation for
// however many sources there are.
//
// The span starts past the reds and ambers so a repository's identity can never
// be mistaken for a failure or for work in progress.
let hueOf = new Map();
function indexSources(sources) {
  hueOf = new Map((sources || []).map((s, i, all) =>
    [s.name, `hsl(${Math.round(95 + (i * 260) / Math.max(all.length, 1))} 55% 60%)`]));
}
const sourceColour = name => hueOf.get(name) || "var(--faint)";

// Every displayed duration is a span carrying its own start instant, and this
// is the only thing that ever changes its text — draw() rebuilds the whole
// board from state, which a tick must never trigger: it would interrupt a
// drag, collapse an expanded done column, or reset the rail's scroll
// position, once a second, forever.
export function tickDurations() {
  const n = nowMs();
  document.querySelectorAll(".dur[data-start]").forEach(el => {
    const start = Number(el.dataset.start);
    if (!(start > 0)) return;
    el.textContent = formatDuration(n - start) + (el.dataset.ago ? " ago" : "");
  });
}
