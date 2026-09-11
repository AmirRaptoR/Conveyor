// The inbox (#40): an attention-first list beside the pipeline, for a phone
// user with one short path from a notification to a question, a failed
// check or another actionable stop — never a second source of truth. It
// reads the same `state`/`blocks` the rail already draws from and opens the
// very same `#panel` `inspect()` does; the only things genuinely new here are
// the classification (pure.js's attentionCategory/attentionAction) and the
// list itself. Source/search filtering is a plain array transform
// (filterItems) with no fetch and no ordering rule of its own, so it can
// never be mistaken for the scheduler's own order or a provider write.
import { $, esc } from "./dom.js";
import { attentionCategory, attentionAction, filterItems, sourceDegraded, isHttpUrl } from "./pure.js";
import { state, nowMs, sourceFilter, setSourceFilter } from "./shared.js";
import { blocks, tone, questionsOf, durSpan } from "./board.js";
import { openAsk } from "./report.js";
import { inspect } from "./panel.js";

// Which of the two tabs is showing, and the live filter values — module
// state because #inbox-list is rebuilt on every draw() while these must
// survive it, the same reason board.js keeps `shown` (the done-column
// expansion) outside the DOM it replaces.
let subtab = "attention";

function switchView(view) {
  $("#rail-wrap").hidden = view !== "pipeline";
  $("#inbox").hidden = view !== "inbox";
  $("#tab-pipeline").setAttribute("aria-selected", String(view === "pipeline"));
  $("#tab-inbox").setAttribute("aria-selected", String(view === "inbox"));
}
$("#tab-pipeline").onclick = () => switchView("pipeline");
$("#tab-inbox").onclick = () => switchView("inbox");

// A phone opens straight into the inbox rather than the wide rail — the one
// difference load-time width makes; the toggle above still reaches either
// view from the other at any width. Guarded: the test harness (testutil.mjs)
// defines no `matchMedia`, and a browser too old for it gets the pipeline,
// which was always there.
if (typeof matchMedia === "function") {
  switchView(matchMedia("(max-width: 640px)").matches ? "inbox" : "pipeline");
}

function switchSubtab(next) {
  subtab = next;
  $("#inbox-tab-attention").setAttribute("aria-selected", String(subtab === "attention"));
  $("#inbox-tab-done").setAttribute("aria-selected", String(subtab === "done"));
  renderInbox();
}
$("#inbox-tab-attention").onclick = () => switchSubtab("attention");
$("#inbox-tab-done").onclick = () => switchSubtab("done");
$("#inbox-search").oninput = () => renderInbox();
// The source filter is shared with the Pipeline view's chips (#93): choosing
// a source here reaches them through the one value both read (shared.js's
// sourceFilter), rather than this select owning a filter of its own.
$("#inbox-source").onchange = () => setSourceFilter($("#inbox-source").value);

// The set of source names whose data is not to be trusted as current right
// now — the same rule board.js applies per source chip, read here per item
// instead so a stale item is marked distinctly from one that is genuinely
// failing (#40: "stale data cannot look current").
function staleSourceNames() {
  const updatedAtMs = state?.updatedAt ? new Date(state.updatedAt).getTime() : 0;
  const names = new Set();
  for (const s of state?.sources || []) {
    if (sourceDegraded(s, updatedAtMs, state.pollNs, nowMs())) names.add(s.name);
  }
  return names;
}

function stageOf(name) { return (state?.stages || []).find(st => st.name === name); }

// One line of "why", in the same words the category action carries — never a
// second vocabulary for the same mark.
const CATEGORY_LABEL = {
  question: "Needs you", failure: "Failed", dependency: "Blocked on a dependency",
  limit: "Out of quota", waiting: "Waiting", pending: "In progress",
};

function row(it, category, block, wait, stale) {
  const reason = (block && block.reason) || (wait && wait.why) || "";
  const action = attentionAction(category, block);
  const qs = category === "question" ? questionsOf(block) : null;
  const isCheckLink = category === "failure" && block?.kind === "checks" && isHttpUrl(it.url);
  return `<article class="item inbox-row" tabindex="0" role="button"
      data-id="${esc(it.id)}" data-title="${esc(it.title)}" data-stage="${esc(it.stage)}">
    <span class="title">${esc(it.title)}</span>
    <span class="foot">
      <span class="why ${esc(tone(block))}">${esc(CATEGORY_LABEL[category] || category)}</span>
      ${stale ? `<span class="stale" title="${esc(it.source)} has not been listed recently; this may be out of date">stale</span>` : ""}
      <span class="src">${esc(it.source)} &middot; ${esc(it.stage)}</span>
    </span>
    ${reason ? `<p class="reason">${esc(reason)}</p>` : ""}
    <div class="inbox-actions">
      ${category === "question" && qs
        ? `<button class="ctl act ask" data-id="${esc(it.id)}" data-title="${esc(it.title)}" data-stage="${esc(it.stage)}">Answer question</button>`
        : `<span class="act-label">${esc(action)}</span>`}
      ${isCheckLink ? `<a class="ctl act" href="${esc(it.url)}" target="_blank" rel="noopener">View failed check</a>` : ""}
      ${isHttpUrl(it.url) ? `<a class="ctl act openpr" href="${esc(it.url)}" target="_blank" rel="noopener">Open PR</a>` : ""}
    </div>
  </article>`;
}

function doneRow(it) {
  const finishedAt = it.finishedAt ? new Date(it.finishedAt).getTime() : NaN;
  return `<article class="item inbox-row" tabindex="0" role="button"
      data-id="${esc(it.id)}" data-title="${esc(it.title)}" data-stage="${esc(it.stage)}">
    <span class="title">${esc(it.title)}</span>
    <span class="foot">
      <span class="why ok">${esc(it.stage)}</span>
      <span class="src">${esc(it.source)}</span>
      ${!isNaN(finishedAt) ? durSpan(finishedAt, true, `finished ${esc(new Date(finishedAt).toLocaleString())}`) : ""}
    </span>
    <div class="inbox-actions">
      ${isHttpUrl(it.url) ? `<a class="ctl act openpr" href="${esc(it.url)}" target="_blank" rel="noopener">Open PR</a>` : ""}
    </div>
  </article>`;
}

// Rebuilds #inbox-source's own options from the sources on the board, then
// sets its value from the shared filter (#93) — never read back off the
// select itself, which is what makes the chips and this one control agree
// even when a poll rebuilds these options out from under it. A source that
// has vanished has already been reconciled to "" by draw() before this runs
// (see shared.js's reconcileSourceFilter), so there is nothing left to do
// here for that case.
function renderSourceOptions() {
  const sel = $("#inbox-source");
  const names = (state?.sources || []).map(s => s.name);
  sel.innerHTML = `<option value="">All sources</option>` +
    names.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join("");
  sel.value = sourceFilter;
}

// Rebuilds only #inbox-list — never the search box, the select or the tabs
// around it, so a redraw mid-keystroke or mid-selection does not undo it.
export function renderInbox() {
  if (!state) return;
  renderSourceOptions();
  const stale = staleSourceNames();
  const wait = state.waiting || {};
  const source = sourceFilter;
  const query = $("#inbox-search").value;

  let list;
  if (subtab === "done") {
    list = (state.items || []).filter(it => stageOf(it.stage)?.terminal);
  } else {
    list = (state.items || []).filter(it => attentionCategory(it, blocks[it.id], wait[it.id]) !== null);
  }
  const total = list.length;
  const filtered = filterItems(list, source, query);

  const box = $("#inbox-list");
  if (!filtered.length) {
    box.innerHTML = `<div class="slot">${total === 0
      ? (subtab === "done" ? "Nothing finished yet." : "Nothing needs your attention right now.")
      : "No items match this filter."}</div>`;
    return;
  }
  box.innerHTML = subtab === "done"
    ? filtered.map(doneRow).join("")
    : filtered.map(it => row(it, attentionCategory(it, blocks[it.id], wait[it.id]), blocks[it.id], wait[it.id], stale.has(it.source))).join("");

  // One `document.querySelector` per question row rather than a `querySelectorAll`
  // over the freshly-written innerHTML: `.item[data-id="…"] .ask` names the one
  // button that row can have, the same way panel.js reaches `#stop .hand` — a
  // fixed selector, not structural traversal.
  if (subtab !== "done") {
    for (const it of filtered) {
      const category = attentionCategory(it, blocks[it.id], wait[it.id]);
      if (category !== "question" || !questionsOf(blocks[it.id])) continue;
      const btn = document.querySelector(`.item[data-id="${CSS.escape(it.id)}"] .ask`);
      if (!btn) continue;
      btn.onclick = e => {
        e.stopPropagation();
        inspect(it.id, it.title, it.stage).then(() => openAsk(it.id, it.title));
      };
    }
  }
}
