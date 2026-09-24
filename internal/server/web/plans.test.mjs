import { test } from "node:test";
import assert from "node:assert/strict";
import { page, withState } from "./testutil.mjs";

const item = { id: "s1:1", source: "s1", stage: "working", title: "Build it" };
const revision = (rev, todos) => ({ v: 1, rev, at: "2026-09-24T12:00:00Z", todos });
const base = overrides => Object.assign({
  stages: [{ name: "working", next: "done" }, { name: "done", terminal: true }],
  sources: [{ name: "s1" }], items: [item], active: [], updatedAt: "2026-09-24T12:00:00Z",
}, overrides);

test("card plan is live only when its run matches the active transition", async () => {
  const p = await page();
  await withState(p, base({
    active: [{ itemId: item.id, stage: "working", runId: "live", startedAt: "2026-09-24T12:00:00Z" }],
    plans: { [item.id]: { runId: "live", completed: 1, total: 3, inProgress: "Run tests", rejected: 0 } },
  }));
  assert.match(p.el("#rail").innerHTML, /plan-progress live/);
  assert.match(p.el("#rail").innerHTML, /plan 1\/3/);
  assert.match(p.el("#rail").innerHTML, /Run tests/);

  await withState(p, base({
    active: [{ itemId: item.id, stage: "working", runId: "new-run", startedAt: "2026-09-24T12:00:00Z" }],
    plans: { [item.id]: { runId: "old-run", completed: 2, total: 3, inProgress: "Old step", rejected: 0 } },
  }));
  assert.match(p.el("#rail").innerHTML, /plan-progress last/);
  assert.match(p.el("#rail").innerHTML, /last plan 2\/3/);
});

test("card plan treats an active entry without runId as historical", async () => {
  const p = await page();
  await withState(p, base({
    active: [{ itemId: item.id, stage: "working", startedAt: "2026-09-24T12:00:00Z" }],
    plans: { [item.id]: { runId: "r1", completed: 0, total: 1, inProgress: "Step", rejected: 0 } },
  }));
  assert.match(p.el("#rail").innerHTML, /last plan 0\/1/);
  assert.doesNotMatch(p.el("#rail").innerHTML, /plan-progress live/);
});

test("card with no plan is unchanged", async () => {
  const p = await page({ now: Date.parse("2026-09-24T12:00:05Z") });
  await withState(p, base({ plans: undefined }));
  const without = p.el("#rail").innerHTML;
  await withState(p, base({ plans: {} }));
  assert.equal(p.el("#rail").innerHTML, without);
  assert.doesNotMatch(without, /plan-progress/);
});

test("card escapes the current step and shows nonzero rejections", async () => {
  const p = await page();
  await withState(p, base({ plans: {
    [item.id]: { runId: "r1", completed: 0, total: 1, inProgress: "<img onerror=boom>", rejected: 4 },
  } }));
  const html = p.el("#rail").innerHTML;
  assert.match(html, /&lt;img onerror=boom&gt;/);
  assert.doesNotMatch(html, /<img/);
  assert.match(html, /4 rejected/);
});

test("panel snapshot renders the full revision, active fallback, counts, and escaping", async () => {
  const p = await page();
  const token = p.mod.beginPanelPlan("r1");
  p.mod.applyPlanSnapshot(token, { accepted: 3, rejected: 2, revision: revision(5, [
    { id: "a", text: "Done <one>", status: "completed" },
    { id: "b", text: "Fallback & safe", status: "in_progress" },
    { id: "c", text: "Later", status: "pending" },
  ]) });
  const html = p.el("#todos").innerHTML;
  assert.match(html, /revision 5 · 3 accepted · 2 rejected/);
  assert.match(html, /completed[\s\S]*Done &lt;one&gt;/);
  assert.match(html, /in_progress[\s\S]*Fallback &amp; safe/);
  assert.match(html, /pending[\s\S]*Later/);
  assert.equal(p.el("#todos").hidden, false);
});

test("panel live revisions replace the list and ignore another run", async () => {
  const p = await page();
  p.mod.beginPanelPlan("followed");
  p.mod.applyPlanEvent({ kind: "plan", runId: "followed", itemId: item.id, accepted: 1, rejected: 0,
    plan: revision(1, [{ id: "a", text: "Old", status: "pending" }]) });
  p.mod.applyPlanEvent({ kind: "plan", runId: "other", itemId: item.id, accepted: 9, rejected: 0,
    plan: revision(9, [{ id: "x", text: "Wrong run", status: "completed" }]) });
  assert.match(p.el("#todos").innerHTML, /Old/);
  assert.doesNotMatch(p.el("#todos").innerHTML, /Wrong run/);

  p.mod.applyPlanEvent({ kind: "plan", runId: "followed", itemId: item.id, accepted: 2, rejected: 1,
    plan: revision(3, [{ id: "b", text: "Static", active: "Actively & now", status: "in_progress" }]) });
  const html = p.el("#todos").innerHTML;
  assert.doesNotMatch(html, /Old/);
  assert.match(html, /Actively &amp; now/);
  assert.match(html, /2 accepted · 1 rejected/);
});

test("pending snapshot cannot overwrite a newer SSE revision", async () => {
  const p = await page();
  const token = p.mod.beginPanelPlan("r1");
  p.mod.applyPlanEvent({ kind: "plan", runId: "r1", itemId: item.id, accepted: 2, rejected: 0,
    plan: revision(2, [{ id: "new", text: "New live", status: "pending" }]) });
  assert.equal(p.mod.applyPlanSnapshot(token, { accepted: 1, rejected: 0,
    revision: revision(1, [{ id: "old", text: "Old snapshot", status: "pending" }]) }), false);
  assert.match(p.el("#todos").innerHTML, /New live/);
  assert.doesNotMatch(p.el("#todos").innerHTML, /Old snapshot/);
});

test("TodoWrite log text is ordinary tool output and never changes the panel plan", async () => {
  const p = await page();
  const token = p.mod.beginPanelPlan("r1");
  p.mod.applyPlanSnapshot(token, { accepted: 1, rejected: 0,
    revision: revision(1, [{ id: "a", text: "Structured", status: "pending" }]) });
  const before = p.el("#todos").innerHTML;
  const line = p.mod.renderLine({ stream: "stdout", text: "  · TodoWrite: [{\"content\":\"From log\",\"status\":\"completed\"}]" });
  assert.equal(p.el("#todos").innerHTML, before);
  assert.match(line.innerHTML, /TodoWrite/);
  assert.match(line.innerHTML, /From log/);
  assert.doesNotMatch(before, /From log/);
});
