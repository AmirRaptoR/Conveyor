// Exercises the board's fetches that change engine state or refresh it —
// load(), startItem(), saveOrder(), handBack(), sendAnswer(), #unblock-all,
// #tick, #refresh and startDoctor() — against a stubbed fetch and a minimal,
// hand-rolled DOM. No dependency, no package.json, no node_modules: run with
//
//   node --test internal/server/web/writes.test.mjs
//
// The page's real modules are imported, and page() gives each test its own
// copy of the whole module graph, so one test's element or module state can
// never bleed into the next. main.js — the live event stream, the duration
// ticker and the first poll — is deliberately not among them: it needs a real
// transport and is out of scope here.
//
// Two things this file used to do by swapping globals in a vm sandbox it now
// does by observing what the page actually did, because an ES module's
// bindings cannot be reassigned from outside it: faults(p) reads the messages
// fault() wrote into the masthead line, and p.el("#rail").writes counts the
// rebuilds draw() performed. See testutil.mjs.

import { test } from "node:test";
import assert from "node:assert/strict";
import { page, withState, faults } from "./testutil.mjs";

// ---- load() — GET /api/state -------------------------------------------

test("load: a 500 reports a server error, distinctly from a network failure", async () => {
  const p = await page({ fetch: async () => ({ ok: false, status: 500, headers: { get: () => null }, text: async () => "db down" }) });
  await p.mod.load();
  assert.equal(faults(p).length, 1);
  assert.doesNotMatch(faults(p)[0], /disconnected/);
  assert.equal(p.el("#rail").writes, 0, "a failed load must not draw from whatever state it had before");
});

test("load: an actual fetch rejection reports disconnected", async () => {
  const p = await page({ fetch: async () => { throw new Error("network down"); } });
  await p.mod.load();
  assert.equal(faults(p).length, 1);
  assert.match(faults(p)[0], /disconnected/);
  assert.equal(p.el("#rail").writes, 0);
});

test("load: a 2xx response draws", async () => {
  const p = await page({ fetch: async () => ({ ok: true, status: 200, headers: { get: () => null }, json: async () => ({ items: [] }) }) });
  await p.mod.load();
  assert.equal(faults(p).length, 0);
  assert.equal(p.el("#rail").writes, 1, "a successful load rebuilds the board exactly once");
});

// ---- startItem() — POST /api/items/{id}/start ---------------------------

test("startItem: a network rejection is caught and reported, not thrown", async () => {
  const p = await page({ fetch: async () => { throw new Error("offline"); } });
  await assert.doesNotReject(p.mod.startItem("issue-9", "refining"));
});

test("startItem: a 500 leaves the source and destination queues untouched (no optimistic move exists on this path)", async () => {
  const p = await page({ fetch: async () => ({ ok: false, status: 500, text: async () => "nope" }) });
  const backlog = p.el('.queue[data-stage="backlog"]');
  const refining = p.el('.queue[data-stage="refining"]');
  const card = { dataset: { id: "issue-9" } };
  backlog.children = [card];
  refining.children = [];
  backlog.insertBefore = refining.insertBefore = () => { throw new Error("startItem must not move the card itself"); };
  await p.mod.startItem("issue-9", "refining");
  assert.deepEqual(backlog.children, [card], "card stayed in the column state reports it in");
  assert.deepEqual(refining.children, [], "destination column's membership is unchanged");
});

// ---- saveOrder() — PUT /api/order ----------------------------------------

test("saveOrder: a 500 rolls back (re-draws from the last reported state)", async () => {
  const p = await page();
  // draw() is the rollback and it returns without touching the DOM until a
  // state has actually been reported, so one is seeded here to make the
  // rebuild observable.
  await withState(p, { items: [] });
  const drawn = p.el("#rail").writes;
  p.setFetch(async () => ({ ok: false, status: 500, text: async () => "conflict" }));
  await p.mod.saveOrder();
  assert.equal(p.el("#rail").writes - drawn, 1);
  assert.equal(faults(p).length, 1);
  assert.match(faults(p)[0], /conflict/);
});

test("saveOrder: a rejected fetch rolls back with no unhandled rejection", async () => {
  const p = await page();
  await withState(p, { items: [] });
  const drawn = p.el("#rail").writes;
  p.setFetch(async () => { throw new Error("timeout"); });
  await assert.doesNotReject(p.mod.saveOrder());
  assert.equal(p.el("#rail").writes - drawn, 1);
  assert.equal(faults(p).length, 1);
});

test("saveOrder: a 2xx response does not roll back", async () => {
  const p = await page();
  await withState(p, { items: [] });
  const drawn = p.el("#rail").writes;
  p.setFetch(async () => ({ ok: true, status: 200, text: async () => "" }));
  await p.mod.saveOrder();
  assert.equal(p.el("#rail").writes - drawn, 0);
  assert.equal(faults(p).length, 0);
});

// ---- handBack() — POST /api/items/{id}/unblock (hand-back path) ---------

function stopBtn(label) {
  return { disabled: false, textContent: label };
}

test("handBack: a 500 restores the button's own prior label and keeps the typed text", async () => {
  const p = await page({ fetch: async () => ({ ok: false, status: 500, text: async () => "locked" }) });
  const answerBox = p.el("#stop .answer");
  answerBox.value = "still typing this";
  const btn = stopBtn("Hand back");
  await p.mod.handBack("issue-9", btn);
  assert.equal(btn.disabled, false);
  assert.equal(btn.textContent, "Hand back");
  assert.equal(answerBox.value, "still typing this");
});

test("handBack: a rejected fetch restores the button's prior label (question tone)", async () => {
  const p = await page({ fetch: async () => { throw new Error("down"); } });
  const btn = stopBtn("Send reply");
  await assert.doesNotReject(p.mod.handBack("issue-9", btn));
  assert.equal(btn.disabled, false);
  assert.equal(btn.textContent, "Send reply");
});

test("handBack: the textarea is only cleared after a 2xx response", async () => {
  const failing = await page({ fetch: async () => ({ ok: false, status: 500, text: async () => "no" }) });
  const stopEl = failing.el("#stop");
  stopEl.innerHTML = "<div>still here</div>";
  await failing.mod.handBack("issue-9", stopBtn("Hand back"));
  assert.notEqual(stopEl.innerHTML, "");

  const passing = await page({ fetch: async () => ({ ok: true, status: 200, text: async () => "" }) });
  const stopEl2 = passing.el("#stop");
  stopEl2.innerHTML = "<div>still here</div>";
  await passing.mod.handBack("issue-9", stopBtn("Hand back"));
  assert.equal(stopEl2.innerHTML, "");
});

test("handBack: a failure is reported inside the open panel, not only on the card behind it", async () => {
  const p = await page({ fetch: async () => ({ ok: false, status: 500, text: async () => "db locked" }) });
  await p.mod.handBack("issue-9", stopBtn("Hand back"));
  const panelFault = p.el("#stop .fault");
  assert.match(panelFault.textContent, /db locked/);
  assert.equal(panelFault.hidden, false);
});

test("handBack: a 2xx response clears the whole stop notice, .hand included", async () => {
  const p = await page({ fetch: async () => ({ ok: true, status: 200, text: async () => "" }) });
  const btn = stopBtn("Hand back");
  const stopEl = p.el("#stop");
  stopEl.innerHTML = "<div>stop notice with .hand inside it</div>";
  await p.mod.handBack("issue-9", btn);
  assert.equal(stopEl.innerHTML, "", "the control is gone rather than merely re-enabled — a fresh redraw supplies the next one");
});

// ---- sendAnswer() — POST /api/items/{id}/unblock (question path) --------

test("sendAnswer: a 500 restores the ask-btn's own prior label, whatever it was", async () => {
  const p = await page({ fetch: async () => ({ ok: false, status: 500, text: async () => "no" }) });
  const btn = p.el("#stop .ask-btn");
  btn.textContent = "Answer 3 questions";
  await p.mod.sendAnswer("issue-9", "my answer");
  assert.equal(btn.disabled, false);
  assert.equal(btn.textContent, "Answer 3 questions");
});

test("sendAnswer: a rejected fetch restores the ask-btn's label too", async () => {
  const p = await page({ fetch: async () => { throw new Error("down"); } });
  const btn = p.el("#stop .ask-btn");
  btn.textContent = "Answer the question";
  await assert.doesNotReject(p.mod.sendAnswer("issue-9", "my answer"));
  assert.equal(btn.disabled, false);
  assert.equal(btn.textContent, "Answer the question");
});

test("sendAnswer: a failure is reported inside the open panel too", async () => {
  const p = await page({ fetch: async () => ({ ok: false, status: 500, text: async () => "rejected" }) });
  await p.mod.sendAnswer("issue-9", "my answer");
  const panelFault = p.el("#stop .fault");
  assert.match(panelFault.textContent, /rejected/);
});

test("sendAnswer: a 2xx response clears the whole stop notice, .ask-btn included", async () => {
  const p = await page({ fetch: async () => ({ ok: true, status: 200, text: async () => "" }) });
  const stopEl = p.el("#stop");
  stopEl.innerHTML = "<div>stop notice with .ask-btn inside it</div>";
  await p.mod.sendAnswer("issue-9", "my answer");
  assert.equal(stopEl.innerHTML, "");
});

// ---- #unblock-all — POST /api/unblock ------------------------------------

test("#unblock-all: a 500 re-enables with its original label and shows an error", async () => {
  const p = await page();
  await withState(p, { items: [{ id: "issue-9", blocked: true }] });
  p.setFetch(async () => ({ ok: false, status: 500, text: async () => "nope" }));
  const btn = p.el("#unblock-all");
  btn.textContent = "Unblock all (1)";
  await btn.onclick({ target: btn });
  assert.equal(btn.disabled, false);
  assert.equal(btn.textContent, "Unblock all (1)");
  assert.equal(faults(p).length, 1);
});

test("#unblock-all: a rejected fetch re-enables without waiting on the 2s timer", async () => {
  const p = await page();
  await withState(p, { items: [{ id: "issue-9", blocked: true }] });
  p.setFetch(async () => { throw new Error("down"); });
  const btn = p.el("#unblock-all");
  btn.textContent = "Unblock all (1)";
  await assert.doesNotReject(btn.onclick({ target: btn }));
  assert.equal(btn.disabled, false, "must not depend on the timer a rejection never reaches");
  assert.equal(btn.textContent, "Unblock all (1)");
});

test("#unblock-all: a 2xx response keeps the existing (timer-based) re-enable", async () => {
  const p = await page();
  await withState(p, { items: [{ id: "issue-9", blocked: true }] });
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
  const p = await page({ fetch: async () => ({ ok: false, status: 500, text: async () => "boom" }) });
  const btn = p.el("#tick");
  await btn.onclick({ target: btn });
  assert.equal(btn.disabled, false);
  assert.equal(faults(p).length, 1);
});

test("#tick: a rejected fetch re-enables with no unhandled rejection", async () => {
  const p = await page({ fetch: async () => { throw new Error("down"); } });
  const btn = p.el("#tick");
  await assert.doesNotReject(btn.onclick({ target: btn }));
  assert.equal(btn.disabled, false);
});

test("#tick: the existing 409 message is preserved verbatim", async () => {
  const p = await page({ fetch: async () => ({ ok: false, status: 409, text: async () => "" }) });
  const btn = p.el("#tick");
  await btn.onclick({ target: btn });
  assert.equal(p.el("#line1").textContent, "a tick is already in flight");
});

test("#tick: a 2xx response keeps the existing (timer-based) re-enable", async () => {
  const p = await page({ fetch: async () => ({ ok: true, status: 200, text: async () => "" }) });
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
  const p = await page({ fetch: async () => ({ ok: false, status: 500, text: async () => "boom" }) });
  const btn = p.el("#refresh");
  await assert.doesNotReject(btn.onclick());
  assert.equal(faults(p).length, 1);
});

test("#refresh: a rejected fetch surfaces an error with no unhandled rejection", async () => {
  const p = await page({ fetch: async () => { throw new Error("down"); } });
  const btn = p.el("#refresh");
  await assert.doesNotReject(btn.onclick());
  assert.equal(faults(p).length, 1);
});

// ---- startDoctor() — POST /api/doctor ------------------------------------

test("startDoctor: a non-409 failure surfaces an error", async () => {
  const p = await page({ fetch: async () => ({ ok: false, status: 500, text: async () => "sweep failed" }) });
  await p.mod.startDoctor(true);
  assert.equal(faults(p).length, 1);
});

test("startDoctor: the existing 409 alert is preserved", async () => {
  const p = await page({ fetch: async () => ({ ok: false, status: 409, text: async () => "" }) });
  await p.mod.startDoctor(true);
  assert.equal(p.calls.alert.length, 1);
  assert.equal(p.calls.alert[0], "A sweep is already running.");
  assert.equal(faults(p).length, 0);
});

test("startDoctor: a rejected sweep request produces no unhandled rejection", async () => {
  const p = await page({ fetch: async () => { throw new Error("down"); } });
  await assert.doesNotReject(p.mod.startDoctor(true));
  assert.equal(faults(p).length, 1);
});
