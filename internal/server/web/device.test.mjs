// Exercises device.js's openFromHash — what a tap on a push notification
// does. Same harness as the other UI test files. Run with:
//
//   node --test internal/server/web/device.test.mjs

import { test } from "node:test";
import assert from "node:assert/strict";
import { page, withState } from "./testutil.mjs";

test("openFromHash: a notification for an item still on the board opens its panel", async () => {
  const p = await page();
  await withState(p, {
    stages: [{ name: "working" }],
    items: [{ id: "s1:1", source: "s1", title: "Pick one", stage: "working" }],
  });
  globalThis.location.hash = "#item=s1%3A1";
  p.mod.openFromHash();
  assert.equal(p.el("#ptitle").textContent, "Pick one");
  assert.equal(p.el("#itemgone").hidden, true);
});

test("openFromHash: a notification for an item no longer on the board says so explicitly, rather than doing nothing", async () => {
  const p = await page();
  await withState(p, { stages: [{ name: "working" }], items: [] });
  globalThis.location.hash = "#item=s1%3A9";
  p.mod.openFromHash();
  assert.equal(p.el("#itemgone").hidden, false);
  assert.match(p.el("#itemgone-text").textContent, /s1:9/);
  assert.match(p.el("#itemgone-text").textContent, /no longer on the board/);
});

test("openFromHash: dismissing the unavailable notice hides it again", async () => {
  const p = await page();
  await withState(p, { stages: [{ name: "working" }], items: [] });
  globalThis.location.hash = "#item=gone%3A1";
  p.mod.openFromHash();
  assert.equal(p.el("#itemgone").hidden, false);
  p.el("#itemgone-dismiss").onclick();
  assert.equal(p.el("#itemgone").hidden, true);
});

test("openFromHash: a later notification for an item that does exist clears a previous unavailable notice", async () => {
  const p = await page();
  await withState(p, {
    stages: [{ name: "working" }],
    items: [{ id: "s1:2", source: "s1", title: "Still here", stage: "working" }],
  });
  globalThis.location.hash = "#item=gone%3A1";
  p.mod.openFromHash();
  assert.equal(p.el("#itemgone").hidden, false);
  globalThis.location.hash = "#item=s1%3A2";
  p.mod.openFromHash();
  assert.equal(p.el("#itemgone").hidden, true);
});
