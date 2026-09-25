import { $, esc } from "./dom.js";

// Full SSE/run-detail records and compact state summaries share this cache.
// maxSeq is the protocol cursor common to all three paths, so one guard keeps
// an older state response or event from replacing a newer view of the run.
const runs = new Map();
const highestSeq = new Map();
const highestVersion = new Map();
let selected = null;
let selectedMeta = null;
let selectedReady = false;
let mode = "auto";

function hasOwn(o, key) { return Object.prototype.hasOwnProperty.call(o || {}, key); }

function accept(value, itemId) {
  const runId = String(value?.runId || "");
  if (!runId) return false;
  const maxSeq = Number(value?.maxSeq) || 0;
  const version = Number(value?.version) || maxSeq;
  const highest = highestSeq.get(runId);
  if (highest !== undefined && maxSeq < highest) return false;
  if (highest !== undefined && maxSeq === highest && version < (highestVersion.get(runId) || highest)) return false;

  const previous = runs.get(runId) || {};
  const next = { ...previous, ...value, runId, maxSeq, version };
  // Live is omitempty in the Go view: only true is sent, and absence on a
  // complete steering payload therefore means false rather than "unchanged".
  next.live = previous.live === false ? false : value.live === true;
  if (!hasOwn(value, "commands") && hasOwn(previous, "commands")) next.commands = previous.commands;
  if (itemId) next.itemId = itemId;
  runs.set(runId, next);
  highestSeq.set(runId, maxSeq);
  highestVersion.set(runId, version);
  return true;
}

function say(text) { $("#announcer").textContent = text; }

function panelFault(text) {
  const fault = $("#steering .steer-fault");
  if (!fault) return;
  fault.textContent = text;
  fault.hidden = false;
  say(text);
}

function clearPanelFault() {
  const fault = $("#steering .steer-fault");
  if (!fault) return;
  fault.textContent = "";
  fault.hidden = true;
}

function isSelectedLive(value) {
  if (!selectedMeta || selectedMeta.kind !== "stage" || selectedMeta.itemId !== selected?.itemId) return false;
  if (value && hasOwn(value, "live")) return value.live === true;
  return selectedMeta.outcome === "running";
}

function commandList(value) {
  const commands = Array.isArray(value?.commands) ? value.commands : [];
  if (!commands.length) return "";
  return `<div class="command-audit">
    <div class="steer-label">Command audit</div>
    <ol>${commands.map(c => {
      const reason = c?.reason ? `<span class="command-reason">${esc(c.reason)}</span>` : "";
      const who = c?.by ? ` by ${esc(c.by)}` : "";
      const at = c?.at ? ` · ${esc(c.at)}` : "";
      return `<li class="command ${esc(c?.state || "queued")}">
        <div class="command-head"><span class="command-kind">${esc(c?.kind || "command")}</span>
          <span class="command-state">${esc(c?.state || "queued")}</span></div>
        ${c?.text ? `<div class="command-text">${esc(c.text)}</div>` : ""}
        <div class="command-meta">#${esc(String(c?.seq ?? "?"))}${who}${at} · ${esc(c?.id || "")}</div>
        ${reason}</li>`;
    }).join("")}</ol>
  </div>`;
}

function render() {
  const box = $("#steering");
  if (!selected || !selectedReady) {
    box.innerHTML = "";
    box.hidden = true;
    return;
  }

  const value = runs.get(selected.runId);
  const commands = commandList(value);
  const accepts = Array.isArray(value?.accepts) ? value.accepts : [];
  const live = isSelectedLive(value);
  const mutable = mode !== "observe" && live;
  const instruction = mutable && accepts.includes("instruction");
  const pause = mutable && accepts.includes("pause");
  const cancel = mutable;
  const hasRecord = accepts.length || commands || Number(value?.malformed) > 0;
  if (!hasRecord && !cancel) {
    box.innerHTML = "";
    box.hidden = true;
    return;
  }

  const summary = value ? [
    ["queued", value.queued], ["consumed", value.consumed], ["rejected", value.rejected],
    ["carried", value.carried], ["dropped", value.dropped], ["malformed", value.malformed],
  ].filter(([, n]) => Number(n) > 0).map(([label, n]) => `${Number(n)} ${label}`).join(" · ") : "";
  box.innerHTML = `<section class="run-steering" aria-label="Selected run controls and command audit">
    <div class="steer-head"><h3>${live ? "Run in flight" : "Run steering"}</h3>
      <span>${esc(selected.runId)}</span></div>
    ${accepts.length ? `<div class="steer-capabilities">Accepts ${accepts.map(esc).join(", ")}</div>` : ""}
    ${mutable ? `<div class="steer-controls">
      ${instruction ? `<form class="steer-instruction" data-steer="instruction"><label for="steer-text-${esc(selected.runId)}">Add instruction</label>
        <textarea class="steer-text" id="steer-text-${esc(selected.runId)}" rows="3" maxlength="1024"
          placeholder="What should this run adjust at its next safe boundary?"></textarea>
        <button class="ctl primary" type="submit" data-send="instruction">Send instruction</button></form>` : ""}
      <div class="steer-buttons">
        ${pause ? `<button class="ctl" type="button" data-steer="pause">Pause after current step</button>` : ""}
        ${cancel ? `<button class="ctl danger" type="button" data-cancel>Cancel now</button>` : ""}
      </div>
    </div>` : ""}
    <div class="steer-fault" role="alert" hidden></div>
    ${summary ? `<div class="steer-summary">${esc(summary)}</div>` : ""}
    ${commands}
  </section>`;
  box.hidden = false;
  wireControls(value || { runId: selected.runId, session: "" });
}

function wireControls(value) {
  const form = $("#steering form.steer-instruction");
  if (form) form.onsubmit = async e => {
    e.preventDefault();
    const textBox = $("#steering .steer-text");
    const text = (textBox?.value || "").trim();
    if (!text) {
      panelFault("An instruction is required.");
      textBox?.focus();
      return;
    }
    const button = $("#steering [data-send=\"instruction\"]");
    if (await postSteer("instruction", text, value, button)) textBox.value = "";
  };

  const pause = $("#steering [data-steer=\"pause\"]");
  if (pause) pause.onclick = () => postSteer("pause", "", value, pause);
  const cancel = $("#steering [data-cancel]");
  if (cancel) cancel.onclick = () => openCancel(selected.itemId, selected.runId, selected.title);
}

async function postSteer(kind, text, value, button) {
  clearPanelFault();
  const label = button?.textContent || "";
  if (button) { button.disabled = true; button.textContent = kind === "pause" ? "Requesting pause…" : "Sending…"; }
  let res, message;
  try {
    res = await fetch(`/api/items/${encodeURIComponent(selected.itemId)}/steer`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ kind, text, runId: value.runId, session: value.session || "" }),
    });
  } catch { message = "Could not reach the server."; }
  if (res && !res.ok) message = (await res.text()).trim() || `Could not steer this run (HTTP ${res.status}).`;
  if (message) {
    if (button) { button.disabled = false; button.textContent = label; }
    panelFault(message);
    return false;
  }
  if (button) button.textContent = kind === "pause" ? "Pause requested" : "Sent";
  say(kind === "pause" ? "Pause requested for the selected run." : "Instruction queued for the selected run.");
  return true;
}

function openCancel(itemId, runId, title) {
  const dlg = $("#cancel");
  dlg.innerHTML = `<form class="cancelform" method="dialog">
    <div class="cancel-head"><b>Cancel now</b><span>${esc(title || itemId)}</span></div>
    <p>This stops the selected run immediately. Say why for its audit trail.</p>
    <label for="cancel-reason">Reason</label>
    <textarea class="cancel-reason" id="cancel-reason" rows="3" required></textarea>
    <div class="cancel-fault" role="alert" hidden></div>
    <div class="cancel-foot"><button class="ctl" type="button" data-close>Keep running</button>
      <span class="grow"></span><button class="ctl danger" type="submit">Cancel run</button></div>
  </form>`;
  const form = dlg.querySelector("form");
  const reasonBox = dlg.querySelector(".cancel-reason");
  const fault = dlg.querySelector(".cancel-fault");
  dlg.querySelector("[data-close]").onclick = () => dlg.close();
  form.onsubmit = async e => {
    e.preventDefault();
    const reason = (reasonBox.value || "").trim();
    if (!reason) {
      fault.textContent = "A cancellation reason is required.";
      fault.hidden = false;
      reasonBox.focus();
      return;
    }
    fault.hidden = true;
    const button = form.querySelector('button[type="submit"]');
    const label = button.textContent;
    button.disabled = true; button.textContent = "Cancelling…";
    let res, message;
    try {
      res = await fetch(`/api/items/${encodeURIComponent(itemId)}/cancel`, {
        method: "POST", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ reason, runId }),
      });
    } catch { message = "Could not reach the server."; }
    if (res && !res.ok) message = (await res.text()).trim() || `Could not cancel this run (HTTP ${res.status}).`;
    if (message) {
      button.disabled = false; button.textContent = label;
      fault.textContent = message; fault.hidden = false; say(message);
      return;
    }
    dlg.close();
    say(`Cancellation requested for ${title || itemId}.`);
  };
  dlg.showModal();
  reasonBox.focus();
}

// Selecting a history row is separate from applying its GET response. The
// empty interval between them offers no controls, avoiding a cached run being
// treated as live while its current record is still in flight.
export function selectPanelRun(runId, itemId, title, boardMode = "auto") {
  selected = runId ? { runId, itemId, title } : null;
  selectedMeta = null;
  selectedReady = false;
  mode = boardMode || "auto";
  render();
}

export function applySteeringSnapshot(run, boardMode = "auto") {
  if (!run?.id || run.id !== selected?.runId) return false;
  mode = boardMode || "auto";
  selectedMeta = { kind: run.kind, itemId: run.itemId, outcome: run.outcome };
  selectedReady = true;
  if (run.steering) accept({ ...run.steering, runId: run.id }, run.itemId);
  render();
  return true;
}

export function applySteeringEvent(event) {
  if (!event || event.kind !== "steering" || !event.runId || !event.steering) return false;
  if (!accept({ ...event.steering, runId: event.runId }, event.itemId)) return false;
  if (selected?.runId === event.runId && selected.itemId === event.itemId) {
    selectedMeta = { kind: "stage", itemId: event.itemId, outcome: event.steering.live ? "running" : "" };
    selectedReady = true;
    render();
  }
  return true;
}

export function applySteeringState(snapshot) {
  mode = snapshot?.mode || "auto";
  for (const [itemId, value] of Object.entries(snapshot?.steering || {})) {
    const applied = accept(value, itemId);
    if (applied && selected?.runId === value?.runId && selected.itemId === itemId) {
      selectedMeta = { kind: "stage", itemId, outcome: value.live ? "running" : "" };
      selectedReady = true;
    }
  }
  if (selected) render();
}
