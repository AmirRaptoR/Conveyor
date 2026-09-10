import { $, esc } from "./dom.js";
import { draw } from "./board.js";

export let state = null;
export const clock = iso => { const d = new Date(iso);
  return isNaN(d) ? iso : d.toLocaleTimeString([], {hour:"2-digit", minute:"2-digit"}); };
// The run store's own size, in the units a person reads it in — the board
// says "2.3 GB", never a raw byte count.
export const fmtBytes = n => {
  if (!n) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${n.toFixed(i ? 1 : 0)} ${units[i]}`;
};

// The board is served to phones over the network, where a skewed device
// clock is an everyday thing. Every other timestamp here is absolute, so a
// skewed clock is merely a slightly wrong wall clock — but a duration is
// computed against *now*, and is wrong by the whole skew. skew is how far
// ahead the device is of the engine, taken from the Date header on every
// /api/state response; nowMs() is the device's clock corrected by it.
let skew = 0;
export const nowMs = () => Date.now() - skew;

export async function load() {
  let res;
  try {
    res = await fetch("/api/state");
  } catch { fault("disconnected"); return; }
  // A 500 still answered the request — it is the server saying something is
  // wrong, not the network failing to reach it, and the two must read
  // differently or an operator chases a cable for a bug on the box.
  if (!res.ok) { fault(`server error (HTTP ${res.status})`); return; }
  const served = Date.parse(res.headers.get("Date"));
  if (!isNaN(served)) skew = Date.now() - served;
  state = await res.json();
  draw();
}

export function fault(msg) { $("#lamp").className = "lamp down"; $("#line1").textContent = msg; }

// The doctor's own sweep, drawn separately from /api/state: it outlives any
// one poll and has to keep showing rows settle while a sweep is running,
// whether or not anything else on the board changed.
let doctorSweep = null;
export async function loadDoctor() {
  try { doctorSweep = await (await fetch("/api/doctor")).json(); } catch { return; }
  drawDoctor();
}

function drawDoctor() {
  const box = $("#doctor");
  const sw = doctorSweep;
  if (!sw || !(sw.results || []).length) { box.innerHTML = ""; return; }
  // A dry run is never worded as a change: "would clear/leave", not "cleared",
  // and the head says outright that nothing happened yet.
  const verb = { cleared: sw.apply ? "cleared" : "would clear", left: sw.apply ? "left" : "would leave",
    failed: "failed", skipped: "skipped", pending: "pending" };
  box.innerHTML = `<div class="doctor-sweep">
      <div class="dhead">
        <b>Diagnose</b>
        <span class="${sw.apply ? "dapply" : "dstate"}">${sw.apply ? "applying" : "dry run — nothing changed yet"}</span>
        <span class="dstate">${sw.running ? "running…" : "finished"}</span>
        ${(!sw.running && !sw.apply) ? `<button class="ctl" id="doctor-apply">Apply</button>` : ""}
      </div>
      ${sw.results.map(r => `<div class="drow ${esc(r.status)}">
        <span class="ditem">${esc(r.item)}</span>
        <span class="dstatus">${esc(verb[r.status] || r.status)}</span>
        <span class="dwhy">${esc(r.why || "")}</span>
      </div>`).join("")}
    </div>`;
  const applyBtn = $("#doctor-apply");
  if (applyBtn) applyBtn.onclick = () => startDoctor(true);
}

export async function startDoctor(apply) {
  let res;
  try {
    res = await fetch("/api/doctor", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ apply }),
    });
  } catch { fault("could not start the sweep: disconnected"); return; }
  if (res.status === 409) { alert("A sweep is already running."); return; }
  if (!res.ok) { fault((await res.text()).trim() || `could not start the sweep (HTTP ${res.status})`); return; }
  loadDoctor();
}

// The identity of a focused card/`.more`/`.need` button, or of an agent's open
// `<details>`, in a shape `findFocusTarget`/a `data-agent` lookup can find
// again after the subtree it lives in is rebuilt. `el.closest` rather than a
// straight `el.matches` because the actual event target inside a card can be
// the `.open` link, and that distinction is the "which control" `restoreFocus`
// needs.
export function focusDescriptor(el) {
  if (!el) return null;
  const card = el.closest?.(".item[data-id]");
  if (card) return { kind: "card", id: card.dataset.id, control: el.closest(".open") ? "open" : "card" };
  const more = el.closest?.(".more[data-stage]");
  if (more) return { kind: "more", stage: more.dataset.stage };
  const need = el.closest?.(".need[data-id]");
  if (need) return { kind: "need", id: need.dataset.id };
  return null;
}

// The element `focusDescriptor` would describe the same way, looked up again
// after a rebuild — null when the thing it named is no longer on the board
// (an item that moved out of view, a `.more` button a column no longer
// shows), which is what tells the caller to fall back instead.
export function findFocusTarget(desc) {
  if (!desc) return null;
  if (desc.kind === "card") {
    // The inbox (#40) can render the very same item as its own `.item`, in
    // parallel with the rail's — plain `querySelector` would always hand
    // back whichever sits first in document order (the rail's, since it is
    // written before #inbox), even while that one sits under a `[hidden]`
    // ancestor and .focus() on it silently does nothing. Every match is
    // walked so the one actually on screen wins; the first at all is still
    // the fallback, since a hidden one is better than none for callers that
    // only care whether *a* card was found (see draw()'s own use of this).
    const matches = [...document.querySelectorAll(`.item[data-id="${CSS.escape(desc.id)}"]`)];
    const card = matches.find(c => !c.closest("[hidden]")) || matches[0];
    if (!card) return null;
    return desc.control === "open" ? (card.querySelector(".open") || card) : card;
  }
  if (desc.kind === "more") return document.querySelector(`.more[data-stage="${CSS.escape(desc.stage)}"]`);
  if (desc.kind === "need") return document.querySelector(`.need[data-id="${CSS.escape(desc.id)}"]`);
  return null;
}
