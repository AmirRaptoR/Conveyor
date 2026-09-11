// Exercises the source-chip additions to shared.js's focusDescriptor/
// findFocusTarget (#93): a focused chip survives a draw(), and lands on "All"
// once the chip it named is gone. Same harness as draw.test.mjs — a fake DOM,
// the page's real modules, no browser. Run with:
//
//   node --test internal/server/web/focus.test.mjs

import { test } from "node:test";
import assert from "node:assert/strict";
import { page, withState } from "./testutil.mjs";

function baseState(overrides = {}) {
  return Object.assign({
    stages: [{ name: "backlog", next: "working" }, { name: "working", terminal: false }],
    sources: [{ name: "s1" }, { name: "s2" }],
    items: [],
    updatedAt: new Date().toISOString(),
  }, overrides);
}

// A minimal stand-in for a focused source chip: `closest` answers both the
// draw() capture guard ("#rail, #needs, #inbox-list, #sources") and
// focusDescriptor's own `.src-chip[data-source]` lookup by always naming
// itself — good enough here, since this test controls exactly what is asked.
function chipEl(source) {
  return {
    dataset: { source },
    closest(sel) { return sel.includes("src-chip") || sel.includes("#sources") ? this : null; },
  };
}

test("focus: a source chip focused before a redraw is refocused after it, by identity", async () => {
  const p = await page();
  await withState(p, baseState({ items: [] }));
  document.activeElement = chipEl("s1");
  // The chip findFocusTarget will look up post-redraw — cached by the harness
  // under this exact selector string, so it is the very element draw() will
  // hand .focus() to.
  const rerendered = p.el('.src-chip[data-source="s1"]');
  let focused = false;
  rerendered.focus = () => { focused = true; };
  document.querySelectorAll = sel => (sel === '.src-chip[data-source="s1"]' ? [rerendered] : []);
  await withState(p, baseState({ items: [] })); // a second draw
  assert.equal(focused, true);
});

test("focus: 'All' itself is focused, and survives a redraw the same way", async () => {
  const p = await page();
  await withState(p, baseState({ items: [] }));
  document.activeElement = chipEl("");
  const all = p.el('.src-chip[data-source=""]');
  let focused = false;
  all.focus = () => { focused = true; };
  await withState(p, baseState({ items: [] }));
  assert.equal(focused, true);
});

test("focus: a chip whose source vanished from state.sources lands on All instead", async () => {
  const p = await page();
  await withState(p, baseState({ items: [] }));
  document.activeElement = chipEl("s1");
  document.querySelectorAll = () => []; // s1's chip is gone — no match at all
  const all = p.el('.src-chip[data-source=""]');
  let focused = false;
  all.focus = () => { focused = true; };
  await withState(p, baseState({ sources: [{ name: "s2" }], items: [] })); // s1 no longer enrolled
  assert.equal(focused, true);
});
