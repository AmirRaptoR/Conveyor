// Exercises panel.js's queue-order actions (Move up/down) under the source
// filter (#93): a visible item still moves among its own DOM siblings, and an
// item the filter hides — opened from #needs, a deep link, or the Inbox — is
// evaluated against, and moved within, its stage's full unfiltered order
// instead. Same harness as draw.test.mjs — a fake DOM, the page's real
// modules, no browser. Run with:
//
//   node --test internal/server/web/panel.test.mjs

import { test } from "node:test";
import assert from "node:assert/strict";
import { page, withState } from "./testutil.mjs";

function baseState(overrides = {}) {
  return Object.assign({
    stages: [
      { name: "backlog", next: "working" },
      { name: "working", next: "done" },
      { name: "done", terminal: true },
    ],
    sources: [{ name: "s1" }, { name: "s2" }],
    items: [],
    updatedAt: new Date().toISOString(),
  }, overrides);
}

// ---- moving a hidden item (no rail card at all) -----------------------------
// The fake DOM's querySelectorAll defaults to [] (testutil.mjs), which is
// exactly "this item has no rendered rail card" — the natural stand-in for an
// item the source filter hides, with no override needed to get there.

test("moveItem: a hidden item reorders the stage's full unfiltered order, not just what is visible", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s1:1", source: "s1", stage: "backlog", title: "first" },
      { id: "s2:1", source: "s2", stage: "backlog", title: "second" },
      { id: "s1:2", source: "s1", stage: "backlog", title: "third" },
    ],
  }));
  p.mod.setSourceFilter("s2"); // hides both s1 items
  const before = p.calls.fetch.length;
  p.mod.moveItem("s1:2", -1); // move "third" up, past "second", in the full order
  const [, opts] = p.calls.fetch[before];
  assert.deepEqual(JSON.parse(opts.body), ["s1:1", "s1:2", "s2:1"]);
  assert.equal(p.el("#announcer").textContent, "third moved to position 2 of 3 in backlog");
});

test("moveItem: a hidden item at the front of its stage cannot move up", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s1:1", source: "s1", stage: "backlog", title: "first" },
      { id: "s2:1", source: "s2", stage: "backlog", title: "second" },
    ],
  }));
  p.mod.setSourceFilter("s2");
  const before = p.calls.fetch.length;
  p.mod.moveItem("s1:1", -1);
  assert.equal(p.calls.fetch.length, before, "refused at the top, no request sent");
});

test("moveItem: an item in a terminal stage never moves, hidden or not", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s1:1", source: "s1", stage: "done", title: "shipped" },
      { id: "s2:1", source: "s2", stage: "done", title: "also shipped" },
    ],
  }));
  const before = p.calls.fetch.length;
  p.mod.moveItem("s1:1", 1);
  assert.equal(p.calls.fetch.length, before);
});

// ---- renderPanelActions reads state, not a rail card, for a hidden item ----

test("renderPanelActions: a hidden item still gets Move up/down, evaluated against its unfiltered position", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s1:1", source: "s1", stage: "backlog", title: "first" },
      { id: "s2:1", source: "s2", stage: "backlog", title: "second" },
      { id: "s1:2", source: "s1", stage: "backlog", title: "third" },
    ],
  }));
  p.mod.setSourceFilter("s2");
  p.mod.renderPanelActions("s1:2"); // the last of three: up enabled, down disabled
  const html = p.el("#pactions").innerHTML;
  assert.match(html, /data-act="up"(?! disabled)[^>]*>Move up/);
  assert.match(html, /data-act="down" disabled/);
});

test("renderPanelActions: an item with no rail card and no #needs/deep-link presence at all still renders its declared actions", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:1", source: "s1", stage: "working", title: "reviewing", blocked: false }],
    stages: [
      { name: "backlog", next: "working" },
      { name: "working", next: "done", actions: [{ name: "merge-now", label: "Merge now" }] },
      { name: "done", terminal: true },
    ],
  }));
  p.mod.setSourceFilter("s2"); // hides the only item
  p.mod.renderPanelActions("s1:1");
  assert.match(p.el("#pactions").innerHTML, /Merge now/);
});

test("renderPanelActions: an item absent from state.items entirely clears the actions box", async () => {
  const p = await page();
  await withState(p, baseState({ items: [] }));
  p.el("#pactions").innerHTML = "<button>stale</button>";
  p.mod.renderPanelActions("gone:1");
  assert.equal(p.el("#pactions").innerHTML, "");
});

test("renderPanelActions: operator-cancelled work has an explicit resume action", async () => {
  const p = await page();
  await withState(p, baseState({
    mode: "manual",
    items: [{ id: "s1:1", source: "s1", stage: "working", title: "cancelled" }],
    waiting: { "s1:1": { class: "operator", why: "cancelled by operator" } },
  }));
  p.mod.renderPanelActions("s1:1");
  assert.match(p.el("#pactions").innerHTML, /Resume cancelled work/);
});

test("renderPanelExecutionHold: quarantine reason and exact release are visible without hover", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:1", source: "s1", stage: "working", title: "failed" }],
    failures: { "s1:1": {
      quarantined: true,
      reason: "tests failed <again>",
      releaseCondition: "item.updatedAt must change from \"v7\"",
    } },
  }));
  p.mod.renderPanelExecutionHold("s1:1");
  const html = p.el("#execution-hold").innerHTML;
  assert.equal(p.el("#execution-hold").hidden, false);
  assert.match(html, /Quarantined/);
  assert.match(html, /Reason[\s\S]*tests failed &lt;again&gt;/);
  assert.match(html, /Release[\s\S]*item\.updatedAt must change from &quot;v7&quot;/);
  assert.doesNotMatch(html, /title=/);
});

// ---- relationship detail ---------------------------------------------------

test("renderPanelRelationships: family and execution dependencies are separate, linked safely, and status-labelled", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s1:1", source: "s1", stage: "backlog", title: "Parent <one>", url: "https://example.test/1" },
      { id: "s1:2", source: "s1", stage: "working", title: "Current", parent: "s1:1",
        children: ["s1:3", "s1:9"], dependsOn: ["s1:4", "s1:5", "s1:8"] },
      { id: "s1:3", source: "s1", stage: "done", title: "Done child", url: "javascript:alert(1)" },
      { id: "s1:4", source: "s1", stage: "done", title: "Done dependency", url: "http://example.test/4" },
      { id: "s1:5", source: "s1", stage: "working", title: "Live dependency" },
    ],
    held: { "s1:2": { by: "s1:5", stage: "working", target: "done", until: "done" } },
  }));
  p.mod.renderPanelRelationships("s1:2");
  const html = p.el("#relationships").innerHTML;
  assert.match(html, /<h4>Tracking family<\/h4>/);
  assert.match(html, /<h4>Execution dependencies<\/h4>/);
  assert.match(html, /href="https:\/\/example\.test\/1"/);
  assert.match(html, /Parent &lt;one&gt;/);
  assert.doesNotMatch(html, /href="javascript:/);
  assert.match(html, /Done child[\s\S]*complete/);
  assert.match(html, /9[\s\S]*off board/);
  assert.match(html, /aria-label="dependency 4: complete"/);
  assert.match(html, /aria-label="dependency 5: in working"/);
  assert.match(html, /aria-label="dependency 8: missing · error"/);
  assert.doesNotMatch(html, /missing<\/small>[\s\S]*missing · error/);
  assert.match(html, /Current hold[\s\S]*s1:5 is in working; must reach done before this item can enter done\./);
});

test("renderPanelRelationships: invalid dependency reason is verbatim and separate from a blocked mark", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:2", source: "s1", stage: "backlog", title: "Current", blocked: true,
      parent: "s1:99", dependsOn: ["s1:2"] }],
    blocks: { "s1:2": { kind: "decision", reason: "the blocked mark reason" } },
    held: { "s1:2": { by: "s1:2", invalid: true, reason: "dependency cycle: s1:2 -> s1:2" } },
  }));
  p.mod.renderPanelRelationships("s1:2");
  const html = p.el("#relationships").innerHTML;
  assert.match(html, /role="alert"[\s\S]*dependency cycle: s1:2 -&gt; s1:2/);
  assert.match(html, /Parent[\s\S]*99[\s\S]*off board/);
  assert.doesNotMatch(html, /the blocked mark reason/);
});

test("renderPanelRelationships: invalid tracking lifecycle is visible and a marked terminal child is unfinished", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s1:1", source: "s1", stage: "backlog", title: "Tracker", tracking: true, children: ["s1:2"] },
      { id: "s1:2", source: "s1", stage: "done", title: "Reopened child", blocked: true },
    ],
    tracking: { "s1:1": { state: "invalid", reason: "child status is contradictory" } },
  }));
  p.mod.renderPanelRelationships("s1:1");
  const html = p.el("#relationships").innerHTML;
  assert.match(html, /role="alert"[\s\S]*child status is contradictory/);
  assert.match(html, /Reopened child[\s\S]*done/);
  assert.doesNotMatch(html, /Reopened child[\s\S]*complete/);
});

test("draw: an open panel refreshes relationships from full state despite source filtering", async () => {
  const p = await page();
  const initial = baseState({
    items: [
      { id: "s1:2", source: "s1", stage: "backlog", title: "Current", children: ["s2:3"] },
      { id: "s2:3", source: "s2", stage: "working", title: "Other source child" },
    ],
  });
  await withState(p, initial);
  p.mod.setSourceFilter("s1");
  await p.mod.inspect("s1:2", "Current", "backlog");
  assert.match(p.el("#relationships").innerHTML, /Other source child[\s\S]*working/);

  await withState(p, baseState({ items: [
    { id: "s1:2", source: "s1", stage: "backlog", title: "Current", children: ["s2:3"] },
    { id: "s2:3", source: "s2", stage: "done", title: "Other source child" },
  ] }));
  assert.match(p.el("#relationships").innerHTML, /Other source child[\s\S]*complete/);
});
