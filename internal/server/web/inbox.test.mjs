// Exercises the inbox (#40): the attention-first list beside the pipeline.
// Same harness as draw.test.mjs — a fake DOM, the page's real modules, no
// browser. Run with:
//
//   node --test internal/server/web/inbox.test.mjs

import { test } from "node:test";
import assert from "node:assert/strict";
import { page, withState } from "./testutil.mjs";

function baseState(overrides = {}) {
  return {
    stages: [
      { name: "backlog", next: "working" },
      { name: "working", next: "done" },
      { name: "done", terminal: true },
    ],
    sources: [{ name: "s1", lastListedAt: new Date().toISOString() }, { name: "s2", lastListedAt: new Date().toISOString() }],
    items: [],
    blocks: {},
    waiting: {},
    updatedAt: new Date().toISOString(),
    ...overrides,
  };
}

test("inbox: a question row shows 'Needs you' and an Answer question button", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:1", source: "s1", title: "Pick a migration strategy", stage: "working", blocked: true }],
    blocks: { "s1:1": { kind: "human-review", asked: true, reason: "which approach?", questions: [{ header: "Which", question: "Which approach?", options: [{ label: "A" }, { label: "B" }] }] } },
  }));
  const html = p.el("#inbox-list").innerHTML;
  assert.match(html, /Needs you/);
  assert.match(html, /Answer question/);
  assert.match(html, /which approach\?/);
});

test("inbox: a failed check renders 'View failed check' as a link, not the generic Retry label", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:2", source: "s1", title: "Ship the release", stage: "working", blocked: true, url: "https://github.com/x/y/pull/2" }],
    blocks: { "s1:2": { kind: "checks", reason: "a status check failed" } },
  }));
  const html = p.el("#inbox-list").innerHTML;
  assert.match(html, /View failed check/);
  assert.match(html, /href="https:\/\/github\.com\/x\/y\/pull\/2"/);
  assert.doesNotMatch(html, /Retry stage/);
});

test("inbox: a plain failure (not checks) renders the generic 'Retry stage' label", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:3", source: "s1", title: "Fix the build", stage: "working", blocked: true }],
    blocks: { "s1:3": { kind: "error", reason: "the agent crashed" } },
  }));
  assert.match(p.el("#inbox-list").innerHTML, /Retry stage/);
});

test("inbox: dependency and limit are their own labels, not folded into a generic wait", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s1:4", source: "s1", title: "Waits on #9", stage: "working", blocked: true },
      { id: "s1:5", source: "s1", title: "Out of tokens", stage: "working", blocked: true },
    ],
    blocks: {
      "s1:4": { kind: "dependency", reason: "blocked on #9" },
      "s1:5": { kind: "limit", reason: "quota exhausted" },
    },
  }));
  const html = p.el("#inbox-list").innerHTML;
  assert.match(html, /Blocked on a dependency/);
  assert.match(html, /Out of quota/);
});

test("inbox: a generic wait (turns/unfinished/worktree) reads 'Waiting', distinct from a failure and from a question", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:4b", source: "s1", title: "Ran out of turns", stage: "working", blocked: true }],
    blocks: { "s1:4b": { kind: "turns", reason: "hit the turn budget mid-review" } },
  }));
  const html = p.el("#inbox-list").innerHTML;
  assert.match(html, /Waiting/);
  assert.match(html, /View status/);
  assert.doesNotMatch(html, /Failed/);
  assert.doesNotMatch(html, /Needs you/);
});

test("inbox: an unblocked resting item (pending CI) is distinct from a failure or a block", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:6", source: "s1", title: "Quiet PR", stage: "working", blocked: false }],
    waiting: { "s1:6": { why: "waiting for checks to finish" } },
  }));
  const html = p.el("#inbox-list").innerHTML;
  assert.match(html, /In progress/);
  assert.match(html, /waiting for checks to finish/);
  assert.doesNotMatch(html, /Failed/);
});

test("inbox: a source that has not been listed recently marks its items stale, distinctly from failure/waiting", async () => {
  const p = await page();
  await withState(p, baseState({
    sources: [{ name: "s1" /* never listed */ }],
    items: [{ id: "s1:7", source: "s1", title: "Old data", stage: "working", blocked: true }],
    blocks: { "s1:7": { kind: "limit", reason: "quota" } },
  }));
  assert.match(p.el("#inbox-list").innerHTML, /class="stale"/);
});

test("inbox: an item needing nothing (not blocked, not waiting) never appears in the attention tab", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:8", source: "s1", title: "Cruising along", stage: "working", blocked: false }],
  }));
  const html = p.el("#inbox-list").innerHTML;
  assert.doesNotMatch(html, /Cruising along/);
  assert.match(html, /Nothing needs your attention right now\./);
});

test("inbox: the done tab lists terminal items and the attention tab does not carry them over", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s1:9", source: "s1", title: "Shipped it", stage: "done", finishedAt: new Date().toISOString() },
      { id: "s1:10", source: "s1", title: "Still needs you", stage: "working", blocked: true },
    ],
    blocks: { "s1:10": { kind: "error", reason: "boom" } },
  }));
  assert.match(p.el("#inbox-list").innerHTML, /Still needs you/);
  assert.doesNotMatch(p.el("#inbox-list").innerHTML, /Shipped it/);

  p.el("#inbox-tab-done").onclick();
  assert.match(p.el("#inbox-list").innerHTML, /Shipped it/);
  assert.doesNotMatch(p.el("#inbox-list").innerHTML, /Still needs you/);
});

test("inbox: the done tab's own empty state is worded for it, not reused from the attention tab", async () => {
  const p = await page();
  await withState(p, baseState({ items: [] }));
  p.el("#inbox-tab-done").onclick();
  assert.match(p.el("#inbox-list").innerHTML, /Nothing finished yet\./);
});

// ---- filtering: never a fetch, never the scheduler's own order --------------

test("inbox: search and source filters narrow the list with no network call at all", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s1:11", source: "s1", title: "Rotate the deploy key", stage: "working", blocked: true },
      { id: "s2:12", source: "s2", title: "Fix the flaky test", stage: "working", blocked: true },
    ],
    blocks: { "s1:11": { kind: "error" }, "s2:12": { kind: "error" } },
  }));
  const before = p.calls.fetch.length;
  p.el("#inbox-search").value = "deploy";
  p.el("#inbox-search").oninput();
  const html = p.el("#inbox-list").innerHTML;
  assert.match(html, /Rotate the deploy key/);
  assert.doesNotMatch(html, /Fix the flaky test/);
  assert.equal(p.calls.fetch.length, before, "filtering must never itself call fetch");
});

test("inbox: an unmatched filter says so, distinct from genuinely having nothing", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:13", source: "s1", title: "Rotate the deploy key", stage: "working", blocked: true }],
    blocks: { "s1:13": { kind: "error" } },
  }));
  p.el("#inbox-search").value = "nothing matches this";
  p.el("#inbox-search").oninput();
  assert.match(p.el("#inbox-list").innerHTML, /No items match this filter\./);
});

// ---- Answer question wires straight into the existing panel/ask dialog -----

test("inbox: pressing Answer question opens the same item panel the pipeline card would", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:14", source: "s1", title: "Pick one", stage: "working", blocked: true }],
    blocks: { "s1:14": { kind: "human-review", asked: true, reason: "which?", questions: [{ header: "Which", question: "Which?", options: [{ label: "A" }, { label: "B" }] }] } },
  }));
  // Reached the same way panel.js's own stop notice reaches "#stop .hand":
  // one fixed selector, cached by the harness, not a live querySelectorAll.
  // inspect() sets #ptitle synchronously, before its own await — the DOM
  // effect a test can observe without a real <dialog> (openAsk's own
  // showModal() is outside this harness's fake elements, and untested at
  // every other layer of this suite too; not this test's job to add it).
  let stopped = false;
  const askBtn = p.el(`.item[data-id="s1:14"] .ask`);
  askBtn.onclick({ stopPropagation: () => stopped = true, target: askBtn });
  assert.equal(stopped, true, "must not also trigger the row's own open() via bubbling");
  assert.equal(p.el("#ptitle").textContent, "Pick one");
  assert.equal(p.el("#psub").textContent, "s1:14 · working");
});
