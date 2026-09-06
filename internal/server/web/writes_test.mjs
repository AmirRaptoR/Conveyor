// Exercises the fetches in index.html that change engine state or refresh
// the board — load(), startItem(), saveOrder(), handBack(), sendAnswer(),
// #unblock-all, #tick, #refresh and startDoctor() — against a stubbed fetch
// and a minimal, hand-rolled DOM. No dependency, no package.json, no
// node_modules: run with
//
//   node --test internal/server/web/writes_test.mjs
//
// The script is pulled out of the live file (everything from the opening
// <script> tag up to the EventSource wiring, which needs a real transport
// and is out of scope here) and evaluated fresh, under node:vm, for every
// test — so one test's element state can never bleed into the next, and the
// test fails the moment the shipped code changes shape instead of silently
// testing a stale copy.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import vm from "node:vm";

const here = path.dirname(fileURLToPath(import.meta.url));
const html = readFileSync(path.join(here, "index.html"), "utf8");

const scriptStart = html.indexOf("<script>") + "<script>".length;
const scriptEnd = html.indexOf("const es = new EventSource(");
if (scriptStart < 0 || scriptEnd < 0 || scriptEnd <= scriptStart) {
  throw new Error("could not find the board script in index.html");
}
const source = html.slice(scriptStart, scriptEnd);

// A response good enough for the handful of calls this file does not
// exercise directly (loadHistory, drawNotify's push checks, ...): success,
// empty body, no interesting headers.
function okResponse() {
  return { ok: true, status: 200, headers: { get: () => null }, text: async () => "", json: async () => ({}) };
}

// One fake element per selector, created lazily and cached for the lifetime
// of a single page() — so `$("#stop").querySelector(".hand")` and a test's
// own `page.el("#stop .hand")` see the same object, the way a real DOM's
// element identity works. Generic rather than a real parent/child tree: every
// function under test in this file either takes its element by reference
// (handBack(id, btn)) or reaches it through one of a handful of fixed
// selectors, never through structural traversal a tree would be needed for.
function makeElement(sel) {
  const el = {
    id: "", className: "", textContent: "", innerHTML: "",
    hidden: false, disabled: false, value: "",
    dataset: {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    style: {},
    onclick: null,
    addEventListener() {}, removeEventListener() {},
    querySelectorAll() { return []; },
    closest() { return null; },
    focus() {}, click() {},
  };
  el.querySelector = sub => elementFor(`${sel} ${sub}`);
  return el;
}

function elementFor(cache, sel) {
  if (!cache.has(sel)) cache.set(sel, makeElement(sel));
  return cache.get(sel);
}

// A fresh vm context running the extracted script, with fetch routed through
// `fetchImpl` and every DOM access backed by the element cache above. `draw`
// and `fault` are stubbed *after* the script runs: both are declared with
// `function` at the top level of a classic (non-module) script, so they are
// plain properties of the vm's global object, and every unqualified call to
// either name from inside the script resolves against that object at call
// time — the swap is visible to code that was already parsed before it ran.
function page(fetchImpl) {
  const cache = new Map();
  const el = sel => elementFor(cache, sel);
  const calls = { fetch: [], draw: 0, fault: [], alert: [], timers: [] };
  let fetchStub = fetchImpl || (async () => okResponse());
  const sandbox = {
    document: {
      querySelector: sel => el(sel),
      querySelectorAll: () => [],
      addEventListener() {},
    },
    $: sel => el(sel),
    fetch: async (...args) => { calls.fetch.push(args); return fetchStub(...args); },
    console,
    setTimeout: (fn, ms) => { calls.timers.push({ fn, ms }); return calls.timers.length; },
    clearTimeout() {},
    alert: msg => calls.alert.push(msg),
    confirm: () => true,
    navigator: {},
    location: { hash: "", pathname: "/" },
    history: { replaceState() {} },
    addEventListener() {},
    removeEventListener() {},
    CSS: { escape: s => String(s) },
    Date, JSON, Math, Set, Map, Array, encodeURIComponent, decodeURIComponent,
    // Board chrome this file never exercises (the rail's scrollbar, agent
    // strip, ...) still runs its top-level wiring when the script is
    // evaluated, so these need to exist even though no test ever looks at them.
    ResizeObserver: class { observe() {} disconnect() {} unobserve() {} },
  };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  sandbox.draw = () => { calls.draw++; };
  sandbox.fault = msg => { calls.fault.push(msg); };
  return { sandbox, el, calls, setFetch: fn => { fetchStub = fn; } };
}

// `state` lives behind a `let` at the top of the script, invisible from
// outside a vm context the way a real module's private scope would be — so
// the one way to seed it for a test is the same way the page itself does: a
// real load() against a stubbed successful response.
async function withState(p, items) {
  p.setFetch(async () => ({ ok: true, status: 200, headers: { get: () => null }, json: async () => ({ items }) }));
  await p.sandbox.load();
}

// ---- load() — GET /api/state -------------------------------------------

test("load: a 500 reports a server error, distinctly from a network failure", async () => {
  const p = page(async () => ({ ok: false, status: 500, headers: { get: () => null }, text: async () => "db down" }));
  await p.sandbox.load();
  assert.equal(p.calls.fault.length, 1);
  assert.doesNotMatch(p.calls.fault[0], /disconnected/);
  assert.equal(p.calls.draw, 0, "a failed load must not draw from whatever state it had before");
});

test("load: an actual fetch rejection reports disconnected", async () => {
  const p = page(async () => { throw new Error("network down"); });
  await p.sandbox.load();
  assert.equal(p.calls.fault.length, 1);
  assert.match(p.calls.fault[0], /disconnected/);
  assert.equal(p.calls.draw, 0);
});

test("load: a 2xx response draws", async () => {
  const p = page(async () => ({ ok: true, status: 200, headers: { get: () => null }, json: async () => ({ items: [] }) }));
  await p.sandbox.load();
  assert.equal(p.calls.fault.length, 0);
  assert.equal(p.calls.draw, 1);
});

// ---- startItem() — POST /api/items/{id}/start ---------------------------

test("startItem: a network rejection is caught and reported, not thrown", async () => {
  const p = page(async () => { throw new Error("offline"); });
  await assert.doesNotReject(p.sandbox.startItem("issue-9", "refining"));
});

test("startItem: a 500 leaves the source and destination queues untouched (no optimistic move exists on this path)", async () => {
  const p = page(async () => ({ ok: false, status: 500, text: async () => "nope" }));
  const backlog = p.el('.queue[data-stage="backlog"]');
  const refining = p.el('.queue[data-stage="refining"]');
  const card = { dataset: { id: "issue-9" } };
  backlog.children = [card];
  refining.children = [];
  backlog.insertBefore = refining.insertBefore = () => { throw new Error("startItem must not move the card itself"); };
  await p.sandbox.startItem("issue-9", "refining");
  assert.deepEqual(backlog.children, [card], "card stayed in the column state reports it in");
  assert.deepEqual(refining.children, [], "destination column's membership is unchanged");
});

// ---- saveOrder() — PUT /api/order ----------------------------------------

test("saveOrder: a 500 rolls back (re-draws from the last reported state)", async () => {
  const p = page(async () => ({ ok: false, status: 500, text: async () => "conflict" }));
  await p.sandbox.saveOrder();
  assert.equal(p.calls.draw, 1);
  assert.equal(p.calls.fault.length, 1);
  assert.match(p.calls.fault[0], /conflict/);
});

test("saveOrder: a rejected fetch rolls back with no unhandled rejection", async () => {
  const p = page(async () => { throw new Error("timeout"); });
  await assert.doesNotReject(p.sandbox.saveOrder());
  assert.equal(p.calls.draw, 1);
  assert.equal(p.calls.fault.length, 1);
});

test("saveOrder: a 2xx response does not roll back", async () => {
  const p = page(async () => ({ ok: true, status: 200, text: async () => "" }));
  await p.sandbox.saveOrder();
  assert.equal(p.calls.draw, 0);
  assert.equal(p.calls.fault.length, 0);
});

// ---- handBack() — POST /api/items/{id}/unblock (hand-back path) ---------

function stopBtn(label) {
  return { disabled: false, textContent: label };
}

test("handBack: a 500 restores the button's own prior label and keeps the typed text", async () => {
  const p = page(async () => ({ ok: false, status: 500, text: async () => "locked" }));
  const answerBox = p.el("#stop .answer");
  answerBox.value = "still typing this";
  const btn = stopBtn("Hand back");
  await p.sandbox.handBack("issue-9", btn);
  assert.equal(btn.disabled, false);
  assert.equal(btn.textContent, "Hand back");
  assert.equal(answerBox.value, "still typing this");
});

test("handBack: a rejected fetch restores the button's prior label (question tone)", async () => {
  const p = page(async () => { throw new Error("down"); });
  const btn = stopBtn("Send reply");
  await assert.doesNotReject(p.sandbox.handBack("issue-9", btn));
  assert.equal(btn.disabled, false);
  assert.equal(btn.textContent, "Send reply");
});

test("handBack: the textarea is only cleared after a 2xx response", async () => {
  const failing = page(async () => ({ ok: false, status: 500, text: async () => "no" }));
  const stopEl = failing.el("#stop");
  stopEl.innerHTML = "<div>still here</div>";
  await failing.sandbox.handBack("issue-9", stopBtn("Hand back"));
  assert.notEqual(stopEl.innerHTML, "");

  const passing = page(async () => ({ ok: true, status: 200, text: async () => "" }));
  const stopEl2 = passing.el("#stop");
  stopEl2.innerHTML = "<div>still here</div>";
  await passing.sandbox.handBack("issue-9", stopBtn("Hand back"));
  assert.equal(stopEl2.innerHTML, "");
});

test("handBack: a failure is reported inside the open panel, not only on the card behind it", async () => {
  const p = page(async () => ({ ok: false, status: 500, text: async () => "db locked" }));
  await p.sandbox.handBack("issue-9", stopBtn("Hand back"));
  const panelFault = p.el("#stop .fault");
  assert.match(panelFault.textContent, /db locked/);
  assert.equal(panelFault.hidden, false);
});

test("handBack: a 2xx response clears the whole stop notice, .hand included", async () => {
  const p = page(async () => ({ ok: true, status: 200, text: async () => "" }));
  const btn = stopBtn("Hand back");
  const stopEl = p.el("#stop");
  stopEl.innerHTML = "<div>stop notice with .hand inside it</div>";
  await p.sandbox.handBack("issue-9", btn);
  assert.equal(stopEl.innerHTML, "", "the control is gone rather than merely re-enabled — a fresh redraw supplies the next one");
});

// ---- sendAnswer() — POST /api/items/{id}/unblock (question path) --------

test("sendAnswer: a 500 restores the ask-btn's own prior label, whatever it was", async () => {
  const p = page(async () => ({ ok: false, status: 500, text: async () => "no" }));
  const btn = p.el("#stop .ask-btn");
  btn.textContent = "Answer 3 questions";
  await p.sandbox.sendAnswer("issue-9", "my answer");
  assert.equal(btn.disabled, false);
  assert.equal(btn.textContent, "Answer 3 questions");
});

test("sendAnswer: a rejected fetch restores the ask-btn's label too", async () => {
  const p = page(async () => { throw new Error("down"); });
  const btn = p.el("#stop .ask-btn");
  btn.textContent = "Answer the question";
  await assert.doesNotReject(p.sandbox.sendAnswer("issue-9", "my answer"));
  assert.equal(btn.disabled, false);
  assert.equal(btn.textContent, "Answer the question");
});

test("sendAnswer: a failure is reported inside the open panel too", async () => {
  const p = page(async () => ({ ok: false, status: 500, text: async () => "rejected" }));
  await p.sandbox.sendAnswer("issue-9", "my answer");
  const panelFault = p.el("#stop .fault");
  assert.match(panelFault.textContent, /rejected/);
});

test("sendAnswer: a 2xx response clears the whole stop notice, .ask-btn included", async () => {
  const p = page(async () => ({ ok: true, status: 200, text: async () => "" }));
  const stopEl = p.el("#stop");
  stopEl.innerHTML = "<div>stop notice with .ask-btn inside it</div>";
  await p.sandbox.sendAnswer("issue-9", "my answer");
  assert.equal(stopEl.innerHTML, "");
});

// ---- #unblock-all — POST /api/unblock ------------------------------------

test("#unblock-all: a 500 re-enables with its original label and shows an error", async () => {
  const p = page();
  await withState(p, [{ id: "issue-9", blocked: true }]);
  p.setFetch(async () => ({ ok: false, status: 500, text: async () => "nope" }));
  const btn = p.el("#unblock-all");
  btn.textContent = "Unblock all (1)";
  await btn.onclick({ target: btn });
  assert.equal(btn.disabled, false);
  assert.equal(btn.textContent, "Unblock all (1)");
  assert.equal(p.calls.fault.length, 1);
});

test("#unblock-all: a rejected fetch re-enables without waiting on the 2s timer", async () => {
  const p = page();
  await withState(p, [{ id: "issue-9", blocked: true }]);
  p.setFetch(async () => { throw new Error("down"); });
  const btn = p.el("#unblock-all");
  btn.textContent = "Unblock all (1)";
  await assert.doesNotReject(btn.onclick({ target: btn }));
  assert.equal(btn.disabled, false, "must not depend on the timer a rejection never reaches");
  assert.equal(btn.textContent, "Unblock all (1)");
});

test("#unblock-all: a 2xx response keeps the existing (timer-based) re-enable", async () => {
  const p = page();
  await withState(p, [{ id: "issue-9", blocked: true }]);
  p.setFetch(async () => ({ ok: true, status: 200, text: async () => "" }));
  const btn = p.el("#unblock-all");
  btn.textContent = "Unblock all (1)";
  await btn.onclick({ target: btn });
  assert.equal(btn.disabled, true, "still disabled until the timer fires, as before");
  assert.equal(p.calls.timers.length, 1);
  assert.equal(p.calls.timers[0].ms, 2000);
  p.calls.timers[0].fn(); // the timer itself is what re-enables it
  assert.equal(btn.disabled, false);
  assert.equal(btn.textContent, "Unblock all (1)");
});

// ---- #tick — POST /api/tick ------------------------------------------

test("#tick: a 500 re-enables and shows an error, not only via the 1.5s timer", async () => {
  const p = page(async () => ({ ok: false, status: 500, text: async () => "boom" }));
  const btn = p.el("#tick");
  await btn.onclick({ target: btn });
  assert.equal(btn.disabled, false);
  assert.equal(p.calls.fault.length, 1);
});

test("#tick: a rejected fetch re-enables with no unhandled rejection", async () => {
  const p = page(async () => { throw new Error("down"); });
  const btn = p.el("#tick");
  await assert.doesNotReject(btn.onclick({ target: btn }));
  assert.equal(btn.disabled, false);
});

test("#tick: the existing 409 message is preserved verbatim", async () => {
  const p = page(async () => ({ ok: false, status: 409, text: async () => "" }));
  const btn = p.el("#tick");
  await btn.onclick({ target: btn });
  assert.equal(p.el("#line1").textContent, "a tick is already in flight");
});

test("#tick: a 2xx response keeps the existing (timer-based) re-enable", async () => {
  const p = page(async () => ({ ok: true, status: 200, text: async () => "" }));
  const btn = p.el("#tick");
  await btn.onclick({ target: btn });
  assert.equal(btn.disabled, true, "still disabled until the timer fires, as before");
  assert.equal(p.calls.timers.length, 1);
  assert.equal(p.calls.timers[0].ms, 1500);
  p.calls.timers[0].fn();
  assert.equal(btn.disabled, false);
});

// ---- #refresh — POST /api/refresh ----------------------------------------

test("#refresh: a 500 surfaces an error with no unhandled rejection", async () => {
  const p = page(async () => ({ ok: false, status: 500, text: async () => "boom" }));
  const btn = p.el("#refresh");
  await assert.doesNotReject(btn.onclick());
  assert.equal(p.calls.fault.length, 1);
});

test("#refresh: a rejected fetch surfaces an error with no unhandled rejection", async () => {
  const p = page(async () => { throw new Error("down"); });
  const btn = p.el("#refresh");
  await assert.doesNotReject(btn.onclick());
  assert.equal(p.calls.fault.length, 1);
});

// ---- startDoctor() — POST /api/doctor ------------------------------------

test("startDoctor: a non-409 failure surfaces an error", async () => {
  const p = page(async () => ({ ok: false, status: 500, text: async () => "sweep failed" }));
  await p.sandbox.startDoctor(true);
  assert.equal(p.calls.fault.length, 1);
});

test("startDoctor: the existing 409 alert is preserved", async () => {
  const p = page(async () => ({ ok: false, status: 409, text: async () => "" }));
  await p.sandbox.startDoctor(true);
  assert.equal(p.calls.alert.length, 1);
  assert.equal(p.calls.alert[0], "A sweep is already running.");
  assert.equal(p.calls.fault.length, 0);
});

test("startDoctor: a rejected sweep request produces no unhandled rejection", async () => {
  const p = page(async () => { throw new Error("down"); });
  await assert.doesNotReject(p.sandbox.startDoctor(true));
  assert.equal(p.calls.fault.length, 1);
});
