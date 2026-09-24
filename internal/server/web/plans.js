import { $, esc } from "./dom.js";
import { state } from "./shared.js";

// Card summary: a persisted plan is live only when its run is the transition
// currently in flight. Everything else is explicitly historical.
export function cardPlan(it, active) {
  const p = state?.plans?.[it.id];
  if (!p) return "";
  const current = (active || []).find(a => a.itemId === it.id);
  const live = !!current?.runId && current.runId === p.runId;
  return `<span class="plan-progress ${live ? "live" : "last"}">
    <span class="plan-count">${live ? "plan" : "last plan"} ${esc(String(p.completed))}/${esc(String(p.total))}</span>
    ${p.inProgress ? `<span class="plan-step">${esc(p.inProgress)}</span>` : ""}
    ${p.rejected > 0 ? `<span class="plan-rejected">${esc(String(p.rejected))} rejected</span>` : ""}
  </span>`;
}

let panelRun = null;
let panelGeneration = 0;
let panelVersion = 0;

function renderPanelPlan(value) {
  const box = $("#todos");
  const revision = value?.revision;
  const rejected = Number(value?.rejected) || 0;
  if (!revision && !rejected) {
    box.innerHTML = "";
    box.hidden = true;
    return;
  }
  const accepted = Number(value?.accepted) || 0;
  const todos = Array.isArray(revision?.todos) ? revision.todos : [];
  const meta = [
    revision ? `revision ${revision.rev}` : "no accepted plan",
    `${accepted} accepted`,
    ...(rejected ? [`${rejected} rejected`] : []),
  ];
  box.innerHTML = `<div class="plan-meta">${meta.map(esc).join(" · ")}</div>${todos.map(t => {
    const status = String(t?.status || "");
    const mark = status === "completed" ? "✓" : status === "in_progress" ? "▶" : "○";
    const text = status === "in_progress" ? (t?.active || t?.text || "") : (t?.text || "");
    return `<div class="todo ${esc(status)}"><span class="mark">${mark}</span>` +
      `<span class="todo-status">${esc(status)}</span><span class="todo-text">${esc(text)}</span></div>`;
  }).join("")}`;
  box.hidden = false;
}

// Starts one panel snapshot request. The returned token records whether a
// newer SSE revision arrived while that request was pending.
export function beginPanelPlan(runId) {
  panelRun = runId || null;
  panelGeneration++;
  panelVersion = 0;
  renderPanelPlan(null);
  return { runId: panelRun, generation: panelGeneration, version: panelVersion };
}

export function applyPlanSnapshot(token, value) {
  if (!token || token.runId !== panelRun || token.generation !== panelGeneration || token.version !== panelVersion) return false;
  renderPanelPlan(value);
  return true;
}

// Applies the full SSE revision to the selected panel run and its compact
// summary to the card, when that run is known to be the item's active stage
// run (or is already the persisted summary for that card).
export function applyPlanEvent(e) {
  if (!e || e.kind !== "plan") return false;
  if (e.runId === panelRun) {
    panelVersion++;
    renderPanelPlan({ revision: e.plan, accepted: e.accepted, rejected: e.rejected });
  }

  const existing = state?.plans?.[e.itemId];
  const active = (state?.active || []).find(a => a.itemId === e.itemId && a.runId === e.runId);
  if (!active && existing?.runId !== e.runId) return false;
  if (!e.plan) {
    if (existing) existing.rejected = Number(e.rejected) || 0;
    return !!existing;
  }
  const todos = Array.isArray(e.plan.todos) ? e.plan.todos : [];
  const completed = todos.filter(t => t?.status === "completed").length;
  const current = todos.find(t => t?.status === "in_progress");
  if (!state.plans) state.plans = {};
  state.plans[e.itemId] = {
    runId: e.runId,
    stage: active?.stage || existing?.stage || "",
    rev: e.plan.rev,
    at: e.plan.at,
    total: todos.length,
    completed,
    inProgress: current ? (current.active || current.text || "") : "",
    rejected: Number(e.rejected) || 0,
  };
  return true;
}
