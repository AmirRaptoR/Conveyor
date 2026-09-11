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
