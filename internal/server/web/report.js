import { $, esc } from "./dom.js";
import { blocks, questionsOf } from "./board.js";
import { inline } from "./panel.js";
import { refuse, panelFault } from "./drag.js";

// The final report, in a modal: rendered, with the markdown one press away as
// a copy, a file, or the browser's own print-to-PDF. Derived fresh on every
// open, never cached, because it thins as retention sweeps the runs it is
// built from.
export async function openReport(id, title, btn) {
  // The dialog opens at once and says it is loading: the report is derived
  // by walking run history, which on a busy board is seconds, and a button
  // that does nothing for seconds gets pressed again.
  const dlg = $("#reportdlg");
  const head = (ready) => `<div class="rhead"><b>${esc(title || id)}</b><span class="grow"></span>
      ${ready ? `<button class="ctl" data-act="copy">Copy markdown</button>
      <button class="ctl" data-act="md">Download .md</button>
      <button class="ctl" data-act="print">Print / PDF</button>` : ""}
      <button class="ctl" data-act="close">Close</button></div>`;
  dlg.innerHTML = `${head(false)}<div class="report rbody"><div class="slot loading">Building the report from run history…</div></div>`;
  dlg.onclick = e => { if (e.target.dataset?.act === "close" || e.target === dlg) dlg.close(); };
  if (!dlg.open) dlg.showModal();
  if (btn) { btn.disabled = true; btn.textContent = "Loading…"; }
  let md;
  try {
    const res = await fetch(`/api/items/${encodeURIComponent(id)}/report`);
    if (!res.ok) {
      dlg.querySelector(".rbody").innerHTML = `<div class="slot err">${esc((await res.text()).trim() || "no report")}</div>`;
      return;
    }
    md = await res.text();
  } catch (err) {
    dlg.querySelector(".rbody").innerHTML = `<div class="slot err">could not load the report: ${esc(String(err))}</div>`;
    return;
  } finally {
    if (btn) { btn.disabled = false; btn.textContent = btn.dataset.label || "Report"; }
  }
  if (!dlg.open) return; // closed while it loaded
  dlg.innerHTML = `${head(true)}<div class="report rbody">${mdToHTML(md)}</div>`;
  dlg.onclick = async e => {
    const act = e.target.dataset && e.target.dataset.act;
    if (!act) { if (e.target === dlg) dlg.close(); return; }
    if (act === "close") dlg.close();
    if (act === "copy") {
      try { await navigator.clipboard.writeText(md); e.target.textContent = "Copied"; }
      catch { e.target.textContent = "Could not copy"; }
      setTimeout(() => { e.target.textContent = "Copy markdown"; }, 1500);
    }
    if (act === "md") {
      const url = URL.createObjectURL(new Blob([md], { type: "text/markdown" }));
      const a = document.createElement("a");
      a.href = url; a.download = `${id.replace(/[^\w.-]+/g, "-")}-report.md`; a.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
    }
    if (act === "print") {
      // Printed from a plain block in the page, never from the dialog: a
      // modal is fixed to the viewport and clipped to it by the browser's
      // own stylesheet, which prints as one page with a scrollbar. The class
      // hides everything else, and the browser's dialog offers "Save as PDF".
      const out = $("#printout");
      out.innerHTML = mdToHTML(md);
      document.body.classList.add("print-report");
      addEventListener("afterprint", () => {
        document.body.classList.remove("print-report");
        out.innerHTML = "";
      }, { once: true });
      window.print();
    }
  };
  dlg.showModal();
}

// The questions, one at a time, exactly as AskUserQuestion would have put
// them to you: a header, the question, options with what each means, and a
// line for anything else. The answers go back as one reply into the
// conversation that asked, via the same endpoint the textarea uses.

// A native modal <dialog> keeps *outside* elements from taking focus, but
// verified against a real browser it does not cycle Tab back to its own
// first control — Tab off the last focusable inside it lands on <body>
// instead of wrapping, which is indistinguishable from "escaped the dialog"
// for a keyboard user. Trapped explicitly instead, on every Tab press while
// the dialog is open, so it also holds across `show()`'s own re-renders
// between questions (a fresh set of focusables each time, read fresh here).
function trapTabIn(dlg) {
  return e => {
    if (e.key !== "Tab") return;
    const focusable = [...dlg.querySelectorAll(
      'input:not(:disabled), button:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])')];
    if (!focusable.length) return;
    const first = focusable[0], last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  };
}
$("#ask").addEventListener("keydown", trapTabIn($("#ask")));

// The `.ask-btn` that opened the dialog, restored on close — captured as
// whatever currently holds focus, which is that button itself for a real
// click or a keyboard activation. The `close` event (not a `dlg.close()`
// call site) is what catches every way the dialog can end: Cancel, Send
// answers, Escape's native dismissal, and the scrim.
let askOpener = null;
export function openAsk(id, title) {
  const qs = questionsOf(blocks[id]);
  if (!qs) return;
  askOpener = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  const dlg = $("#ask");
  // `{ once: true }`: exactly one of these per open/close cycle, added fresh
  // right before this dialog opens, so nothing accumulates across repeated
  // openAsk() calls on the same shared `#ask` element.
  dlg.addEventListener("close", () => {
    if (askOpener && document.contains(askOpener)) askOpener.focus();
    askOpener = null;
  }, { once: true });
  const answers = [];
  let i = 0;
  const show = () => {
    const q = qs[i], opts = Array.isArray(q.options) ? q.options : [];
    const type = q.multiSelect ? "checkbox" : "radio";
    dlg.innerHTML = `<form class="askform" method="dialog">
      <div class="askhead">
        <span class="askstep">${i + 1} / ${qs.length}</span>
        <span class="askhdr">${esc(q.header || "")}</span>
        <span class="grow"></span>
        <span class="askstep">${esc(title || id)}</span>
      </div>
      <p class="askq">${esc(q.question || q.header || "")}</p>
      <div class="askopts">${opts.map((o, j) => `<label class="askopt">
          <input type="${type}" name="o" value="${j}">
          <span><b>${esc(o.label || "")}</b>${o.description ? `<small>${esc(o.description)}</small>` : ""}</span>
        </label>`).join("")}
        <input class="askother" type="text" placeholder="${opts.length ? "Or say it in your own words" : "Your answer"}">
      </div>
      <div class="askfoot">
        <button type="button" class="ctl" data-act="cancel">Cancel</button>
        <span class="grow"></span>
        ${i ? `<button type="button" class="ctl" data-act="back">Back</button>` : ""}
        <button type="submit" class="ctl send">${i + 1 < qs.length ? "Next" : "Send answers"}</button>
      </div></form>`;
    const form = dlg.querySelector("form");
    form.onclick = e => {
      const act = e.target.dataset && e.target.dataset.act;
      if (act === "cancel") dlg.close();
      if (act === "back") { i--; show(); }
    };
    form.onsubmit = e => {
      e.preventDefault();
      const picked = [...form.querySelectorAll("input[name=o]:checked")].map(x => opts[+x.value].label);
      const other = form.querySelector(".askother").value.trim();
      if (!picked.length && !other) { form.querySelector(".askother").focus(); return; }
      answers[i] = `${q.header || `Q${i + 1}`}: ${[...picked, other].filter(Boolean).join(" — ")}`;
      if (++i < qs.length) { show(); return; }
      dlg.close();
      const box = document.querySelector("#stop .answer");
      const note = box ? box.value.trim() : "";
      sendAnswer(id, answers.join("\n") + (note ? `\n\n${note}` : ""));
    };
    // The dialog replaces its own innerHTML between questions (this is that
    // re-render), which drops whatever inside it held focus — a native modal
    // <dialog> keeps Tab from reaching anything *outside* it, but nothing
    // stops focus from landing nowhere in particular after its own content is
    // swapped out from under it. Put it back on the first real control every
    // time this runs, not only on the first question.
    (form.querySelector('input[name="o"]') || form.querySelector(".askother"))?.focus();
  };
  show();
  dlg.showModal();
}

export async function sendAnswer(id, answer) {
  const btn = document.querySelector("#stop .ask-btn");
  const label = btn ? btn.textContent : "";
  if (btn) { btn.disabled = true; btn.textContent = "Sending…"; }
  let res, msg;
  try {
    res = await fetch(`/api/items/${encodeURIComponent(id)}/unblock`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ answer }),
    });
  } catch { msg = `could not answer ${id}: disconnected`; }
  if (res && !res.ok) msg = (await res.text()).trim() || `could not answer ${id}`;
  if (msg) {
    if (btn) { btn.disabled = false; btn.textContent = label; }
    refuse(id, msg);
    panelFault(msg);
    return;
  }
  $("#stop").innerHTML = "";
}

// A small markdown-to-elements reader for the one document this board
// generates itself: headings, tables, bullets and blockquotes, each drawn as
// its own element rather than one preformatted blob — reusing inline() for
// the `code` and `**bold**` inside them, so nothing in a repository's title
// or a script's summary can inject markup into the page.
export function mdToHTML(md) {
  const lines = md.replace(/\r\n/g, "\n").split("\n");
  let html = "", i = 0;
  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) { i++; continue; }

    const h = line.match(/^(#{1,6})\s+(.*)$/);
    if (h) { html += `<h${h[1].length}>${paraInline(h[2])}</h${h[1].length}>`; i++; continue; }

    if (line.startsWith("|")) {
      const rows = [];
      while (i < lines.length && lines[i].startsWith("|")) { rows.push(lines[i]); i++; }
      html += tableHTML(rows);
      continue;
    }

    if (line.startsWith(">")) {
      const quote = [];
      while (i < lines.length && lines[i].startsWith(">")) { quote.push(lines[i].replace(/^>\s?/, "")); i++; }
      html += `<blockquote>${quote.map(inline).join("<br>")}</blockquote>`;
      continue;
    }

    if (/^-\s+/.test(line)) {
      const items = [];
      while (i < lines.length && /^-\s+/.test(lines[i])) { items.push(lines[i].replace(/^-\s+/, "")); i++; }
      html += `<ul>${items.map(it => `<li>${paraInline(it)}</li>`).join("")}</ul>`;
      continue;
    }

    html += `<p>${paraInline(line)}</p>`;
    i++;
  }
  return html;
}

// A GFM pipe table, cells split on `|` while honouring the report's own
// escaping (`\|` for a literal pipe) so an item title containing one cannot
// widen or narrow a row.
function tableHTML(rows) {
  const split = row => {
    const inner = row.trim().replace(/^\||\|$/g, "");
    const out = [];
    let cur = "";
    for (let j = 0; j < inner.length; j++) {
      if (inner[j] === "\\" && inner[j + 1] === "|") { cur += "|"; j++; continue; }
      if (inner[j] === "|") { out.push(cur.trim()); cur = ""; continue; }
      cur += inner[j];
    }
    out.push(cur.trim());
    return out;
  };
  if (rows.length < 2) return "";
  const header = split(rows[0]);
  const body = rows.slice(2).map(split);
  return `<table><thead><tr>${header.map(c => `<th>${inline(c)}</th>`).join("")}</tr></thead>
    <tbody>${body.map(r => `<tr>${r.map(c => `<td>${inline(c)}</td>`).join("")}</tr>`).join("")}</tbody></table>`;
}

// inline(), plus the one piece of syntax the report adds beyond it: an
// item's own `[text](url)` link, on the identity line. The server only ever
// emits that syntax for an http(s) URL (identityLine, report.go), but this
// reader runs over every heading and paragraph, not just that one line — so
// it re-checks the scheme itself before linking, the same rule the server
// applied, rather than trusting that no other text node can ever contain a
// bracket pair. A title or summary containing literal `[x](javascript:…)`
// therefore prints as text, not as a link that runs on click.
function paraInline(s) {
  const m = s.match(/^(.*)\[([^\]]+)\]\(([^)]+)\)(.*)$/);
  if (!m) return inline(s);
  const safe = /^https?:\/\//i.test(m[3]);
  const mid = safe
    ? `<a href="${esc(m[3])}" target="_blank" rel="noopener">${inline(m[2])}</a>`
    : inline(`[${m[2]}](${m[3]})`);
  return paraInline(m[1]) + mid + paraInline(m[4]);
}
