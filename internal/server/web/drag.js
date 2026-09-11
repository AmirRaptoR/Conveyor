import { $ } from "./dom.js";
import { startableRule, controlsForMode, applyVisibleOrder } from "./pure.js";
import { state, fault, startDoctor } from "./shared.js";
import { draw, blocks, flushPendingRedraw, bucketByStage } from "./board.js";

// Dragging ends in a click; without this, dropping a card also opens it.
export let dragging = null, justDragged = false;

// --- dragging --------------------------------------------------------------
// Two gestures, and they are different things. Within a column a drag reorders,
// and a card's position is what the scheduler reads. Out of the backlog and
// into the next column it *starts* the work: backlog is the absence of a state,
// and deciding something leaves it is the whole of v1's control lever.
//
// Nowhere else. A card dropped straight onto a deploy stage would be a deploy
// nobody reviewed; the pipeline is human-authored ahead of time, not steered
// card by card.
export const queueOf = el => el.closest(".queue[data-stage]")?.dataset.stage;
export const stageBy = name => (state?.stages || []).find(st => st.name === name);
export const startable = (from, into) =>
  startableRule(from, into, state?.stages || []) && controlsForMode(state?.mode || "auto").dragStart;

// A refusal belongs on the card that was dropped, not in a console: an operator
// who sees nothing happen concludes the board is broken.
export const refusals = new Map();
export function refuse(id, why) {
  refusals.set(id, why);
  draw();
  setTimeout(() => { if (refusals.delete(id)) draw(); }, 8000);
}

export async function startItem(id, stage) {
  refusals.delete(id);
  let res;
  try {
    res = await fetch(`/api/items/${encodeURIComponent(id)}/start`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ stage }),
    });
  } catch { refuse(id, `could not start ${id}`); return; }
  if (!res.ok) refuse(id, (await res.text()).trim() || `could not start ${id}`);
}

export function wireQueue(q) {
  const into = q.dataset.stage;
  q.ondragover = e => {
    if (!dragging || !startable(queueOf(dragging), into)) return;
    e.preventDefault();
    clearMarks();
    q.classList.add("over-stage");
  };
  q.ondragleave = () => q.classList.remove("over-stage");
  q.ondrop = e => {
    if (!dragging || !startable(queueOf(dragging), into)) return;
    e.preventDefault();
    clearMarks();
    startItem(dragging.dataset.id, into);
  };
}

export function wireDrag(el) {
  el.ondragstart = e => {
    dragging = el;
    el.classList.add("dragging");
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("text/plain", el.dataset.id);
  };
  el.ondragend = () => {
    el.classList.remove("dragging");
    clearMarks();
    dragging = null;
    justDragged = true;
    setTimeout(() => justDragged = false, 0);
    // The one redraw owed for everything a draw() during the drag deferred
    // (see shouldDeferDraw) — `state` is already whatever the last of those
    // calls set it to, so this catches the board up in a single rebuild.
    flushPendingRedraw();
  };
  // A card in another column is not a drop target of its own: the column is.
  // Letting the event through to it is what makes dropping onto a full backlog's
  // neighbour work the same as dropping onto an empty one.
  const reorders = () => dragging && dragging !== el && queueOf(el) === queueOf(dragging);
  el.ondragover = e => {
    if (!reorders()) return;
    e.preventDefault();
    e.stopPropagation();
    clearMarks();
    el.classList.add(below(e, el) ? "over-after" : "over-before");
  };
  el.ondrop = e => {
    if (!reorders()) return;
    e.preventDefault();
    e.stopPropagation();
    el.parentNode.insertBefore(dragging, below(e, el) ? el.nextSibling : el);
    clearMarks();
    saveOrder();
  };
}

const below = (e, el) => {
  const r = el.getBoundingClientRect();
  return e.clientY > r.top + r.height / 2;
};
const clearMarks = () =>
  document.querySelectorAll(".over-before,.over-after,.over-stage")
    .forEach(n => n.classList.remove("over-before", "over-after", "over-stage"));

// Read the board back as one list. Stage order dominates and the drag decides
// within a stage — an item further along the line should not be overtaken by
// one that has not started, so the queues are read from the end of the line
// back towards the start. Reading them left to right says the opposite: it
// ranks the whole backlog above work already under way.
//
// The source filter (#93) only ever hides cards, never re-renders them
// out of order, so `.queue[data-stage] .item` is exactly the *visible*
// subset's new order after a drag. `bucketByStage` gives each stage's full,
// unfiltered order (the same one station()/terminus() rank against);
// `applyVisibleOrder` merges the two, so a hidden item keeps the exact index
// it already held and only the ids that were actually shown get permuted.
export async function saveOrder() {
  const stages = state?.stages || [];
  const flow = stages.filter(st => !st.terminal).reverse();
  const bucket = bucketByStage(stages, state?.items || [], state?.active || []);
  const ids = flow.flatMap(st => {
    const fullIds = (bucket[st.name] || []).map(it => it.id);
    const visibleIds = [...document.querySelectorAll(`.queue[data-stage="${CSS.escape(st.name)}"] .item`)]
      .map(el => el.dataset.id);
    return applyVisibleOrder(fullIds, visibleIds);
  });
  // Never save an empty order while the board holds (unfiltered) cards — a
  // filter that hides every visible one is not the same as the board being
  // empty, and this counts the unfiltered flow columns, never the DOM, to
  // tell those apart.
  const totalUnfiltered = flow.reduce((n, st) => n + (bucket[st.name]?.length || 0), 0);
  if (!ids.length && totalUnfiltered) {
    console.warn("refusing to save an empty order");
    return;
  }
  let res;
  try {
    res = await fetch("/api/order", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(ids),
    });
  } catch {
    // The drag already moved the card in the DOM (el.ondrop's insertBefore);
    // a save that never reached the server has to be undone the same way a
    // rejected one is — draw() is the rollback, state is still what the
    // server last confirmed.
    fault("could not save the new order: disconnected");
    draw();
    return;
  }
  if (!res.ok) {
    fault(`could not save the new order: ${(await res.text()).trim() || `HTTP ${res.status}`}`);
    draw();
  }
}

// refuse() draws its message onto the board card, which #panel and #scrim
// occlude while the panel is open — the one channel a panel-borne failure
// needs beside it. The box is part of stopNotice()'s own markup (see the
// `.fault` div there), so it survives exactly as long as the stop notice
// that owns it: cleared by the next real redraw of #stop, never on a timer.
export function panelFault(msg) {
  const box = document.querySelector("#stop .fault");
  if (box) { box.textContent = msg; box.hidden = false; }
}

// Clearing a mark is a provider write, so the control says so while it lands:
// a card that keeps its red for another second reads as a button that did
// nothing, and the second press is the one that races.
export async function handBack(id, btn) {
  const box = document.querySelector("#stop .answer");
  const answer = box ? box.value.trim() : "";
  const label = btn ? btn.textContent : "";
  if (btn) { btn.disabled = true; btn.textContent = answer ? "answering" : "handing back"; }
  let res, msg;
  try {
    res = await fetch(`/api/items/${encodeURIComponent(id)}/unblock`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ answer }),
    });
  } catch { msg = `could not unblock ${id}: disconnected`; }
  if (res && !res.ok) msg = (await res.text()).trim() || `could not unblock ${id}`;
  if (msg) {
    if (btn) { btn.disabled = false; btn.textContent = label; }
    refuse(id, msg);
    panelFault(msg);
    return;
  }
  // The panel was showing a stop that no longer exists.
  $("#stop").innerHTML = "";
}

$("#unblock-all").onclick = async e => {
  // Questions are not cleared in bulk and the button must not imply they are:
  // a count that includes them promises something the server will not do.
  const marked = (state?.items || []).filter(it => it.blocked);
  const asking = marked.filter(it => (blocks[it.id] || {}).asked).length;
  const n = marked.length - asking;
  if (!n) {
    alert(asking
      ? `Nothing to retry. ${asking} item${asking === 1 ? " is" : "s are"} waiting on an answer from you — open the card and reply.`
      : "Nothing is marked.");
    return;
  }
  if (!confirm(`Clear the mark on ${n} item${n === 1 ? "" : "s"}? Each goes straight back into the line.`
    + (asking ? `\n\n${asking} waiting on an answer from you ${asking === 1 ? "is" : "are"} left alone.` : ""))) return;
  const btn = e.target, label = btn.textContent;
  btn.disabled = true;
  btn.textContent = "handing back…";
  let res, msg;
  try {
    res = await fetch("/api/unblock", { method:"POST" });
  } catch { msg = "could not clear marks: disconnected"; }
  if (res && !res.ok) msg = (await res.text()).trim() || "could not clear marks";
  if (msg) {
    // A rejection never reaches the timer below, so the re-enable that
    // matters here cannot depend on it — restored at once instead.
    btn.disabled = false;
    btn.textContent = label;
    fault(msg);
    return;
  }
  setTimeout(() => { btn.disabled = false; btn.textContent = label; }, 2000);
};

$("#diagnose").onclick = () => {
  // Dry run is the only thing a click starts — apply is a second, explicit
  // press on the Apply button the result itself offers, never implied by this
  // one. The blast radius is comments and clears across every marked item at
  // once, and that is not a decision to make with one gesture.
  const marked = (state?.items || []).filter(it => it.blocked).length;
  if (!marked) { alert("Nothing is marked."); return; }
  startDoctor(false);
};

$("#refresh").onclick = async () => {
  let res;
  try {
    res = await fetch("/api/refresh", { method:"POST" });
  } catch { fault("could not refresh: disconnected"); return; }
  if (!res.ok) fault((await res.text()).trim() || `could not refresh (HTTP ${res.status})`);
};
