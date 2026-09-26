import { $, esc } from "./dom.js";
import { state } from "./shared.js";

export function budgetChip(it) {
  const b = state.budgets && state.budgets[it.id];
  if (!b?.modelRun || !state.budgetMaxRunsPerItem) return "";
  const dayRemaining = state.budgetMaxRunsPerDay > 0 && Number.isFinite(state.budgetDayRemaining)
    ? state.budgetDayRemaining
    : b.remaining;
  const remaining = Math.min(b.remaining, dayRemaining);
  const grant = b.override && b.override.remaining > 0 ? " · one-run override armed" : "";
  return `<span class="budget" title="${esc(`${b.runs} model runs used by this item${grant}`)}">${remaining} run${remaining === 1 ? "" : "s"} left</span>`;
}

export function failureChip(it) {
  const f = state.failures && state.failures[it.id];
  if (!f) return "";
  const label = f.quarantined ? "quarantined" : "retry held";
  return `<span class="quarantine" title="${esc(`${f.reason}; ${f.releaseCondition}`)}">${label} · ${f.count}</span>`;
}

export function renderPanelExecutionHold(id) {
  const box = $("#execution-hold");
  const failure = state?.failures?.[id];
  box.hidden = !failure;
  if (!failure) { box.innerHTML = ""; return; }
  const label = failure.quarantined ? "Quarantined" : "Automatic retry held";
  box.innerHTML = `<h3>${label}</h3>
    <dl><dt>Reason</dt><dd>${esc(failure.reason)}</dd>
    <dt>Release</dt><dd>${esc(failure.releaseCondition)}</dd></dl>`;
}
