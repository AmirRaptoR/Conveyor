import { $, esc } from "./dom.js";
import { isHttpUrl } from "./pure.js";
import { state } from "./shared.js";

const refOf = id => String(id || "").split(":").pop();

function relatedItem(id, byId, relation) {
  const it = byId.get(id);
  const ref = refOf(id);
  const text = it ? `${ref} · ${it.title || id}` : ref || id;
  const body = it && isHttpUrl(it.url)
    ? `<a href="${esc(it.url)}" target="_blank" rel="noopener">${esc(text)}</a>`
    : `<span>${esc(text)}</span>`;
  return `<span class="relation-item${it ? "" : " absent"}">${body}${!it ? ` <small>${relation === "dependency" ? "missing" : "off board"}</small>` : ""}</span>`;
}

// Tracking family and execution dependencies are deliberately separate: the
// first is informational structure, while the second can prevent dispatch.
// This always reads the complete state, never the source-filtered rail DOM.
export function renderPanelRelationships(id) {
  const box = $("#relationships");
  const items = state?.items || [];
  const it = items.find(x => x.id === id);
  if (!it) { box.innerHTML = ""; box.hidden = true; return; }

  const byId = new Map(items.map(x => [x.id, x]));
  const terminal = new Set((state?.stages || []).filter(s => s.terminal).map(s => s.name));
  const children = Array.isArray(it.children) ? it.children : [];
  const deps = Array.isArray(it.dependsOn) ? it.dependsOn : [];
  const hold = state?.held?.[id];
  const family = it.parent || children.length;

  const familyHTML = family ? `<div class="family-tree" aria-label="Tracking family">
      ${it.parent ? `<div class="relation-row parent"><b>Parent</b>${relatedItem(it.parent, byId, "parent")}</div>` : ""}
      <div class="relation-row current"><b>This item</b><span>${esc(`${refOf(it.id)} · ${it.title || it.id}`)}</span></div>
      ${children.length ? `<div class="relation-row children"><b>Children</b><div role="list">${children.map(childId => {
        const child = byId.get(childId);
        const done = child && terminal.has(child.stage);
        return `<div role="listitem" class="relation-child${done ? " complete" : ""}">${relatedItem(childId, byId, "child")}${child ? `<small>${done ? "complete" : esc(child.stage)}</small>` : ""}</div>`;
      }).join("")}</div></div>` : ""}
    </div>` : `<p class="none">No tracking family.</p>`;

  let holdHTML = "";
  if (hold) {
    if (hold.invalid) {
      holdHTML = `<div class="dependency-hold invalid" role="alert"><b>Dependency error</b><span>${esc(hold.reason || "invalid dependency graph")}</span></div>`;
    } else {
      const reason = hold.reason || `${hold.by} is in ${hold.stage}${hold.blocked ? " and marked" : ""}; must reach ${hold.until} before this item can enter ${hold.target}.`;
      holdHTML = `<div class="dependency-hold"><b>Current hold</b><span>${esc(reason)}</span></div>`;
    }
  }
  const dependenciesHTML = deps.length ? `<div class="dependency-list" role="list" aria-label="Execution dependencies">${deps.map(depId => {
    const dep = byId.get(depId);
    const done = dep && terminal.has(dep.stage);
    const invalid = !dep || (hold?.invalid && hold.by === depId);
    const status = !dep ? "missing · error" : done ? "complete" : `in ${dep.stage}`;
    return `<div role="listitem" class="dependency-row${done ? " complete" : ""}${invalid ? " invalid" : ""}"
        aria-label="dependency ${esc(refOf(depId))}: ${esc(status)}">${relatedItem(depId, byId, "dependency")}<small>${esc(status)}</small></div>`;
  }).join("")}</div>` : `<p class="none">No execution dependencies.</p>`;

  box.innerHTML = `<section aria-labelledby="relationships-title">
    <h3 id="relationships-title">Relationships</h3>
    <h4>Tracking family</h4>${familyHTML}
    <h4>Execution dependencies</h4>${holdHTML}${dependenciesHTML}
  </section>`;
  box.hidden = false;
}
