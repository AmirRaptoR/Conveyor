// Exercises the pure logic in pure.js — no DOM, no npm install, no network.
// Run with:
//
//   node --test internal/server/web/pure.test.mjs
//
// The module is imported, not sliced out of a larger file, so this test
// exercises exactly what the server serves. See pure.js's own comment on that
// region for why these functions in particular are the ones written to take
// plain values instead of DOM nodes.

import { test } from "node:test";
import assert from "node:assert/strict";
import {
  focusKeyOf, nextQueueIndex, shouldDeferDraw, startableRule, staleThresholdMs, sourceDegraded,
  controlsForMode, isHttpUrl,
} from "./pure.js";

test("focusKeyOf: a card and its .open link are different keys", () => {
  assert.equal(focusKeyOf({ kind: "card", id: "issue-44", control: "card" }), "card:issue-44:card");
  assert.equal(focusKeyOf({ kind: "card", id: "issue-44", control: "open" }), "card:issue-44:open");
  assert.notEqual(
    focusKeyOf({ kind: "card", id: "issue-44", control: "card" }),
    focusKeyOf({ kind: "card", id: "issue-44", control: "open" }),
  );
});

test("focusKeyOf: different kinds of control key off their own attribute", () => {
  assert.equal(focusKeyOf({ kind: "more", stage: "done" }), "more:done:");
  assert.equal(focusKeyOf({ kind: "need", id: "issue-9" }), "need:issue-9:");
});

test("focusKeyOf: no element focused is its own key, distinct from any real one", () => {
  assert.equal(focusKeyOf(null), "");
  assert.notEqual(focusKeyOf(null), focusKeyOf({ kind: "card", id: "", control: "" }));
});

test("nextQueueIndex: moves by one in either direction", () => {
  assert.equal(nextQueueIndex(2, 5, -1), 1);
  assert.equal(nextQueueIndex(2, 5, 1), 3);
});

test("nextQueueIndex: refused at the top, not wrapped", () => {
  assert.equal(nextQueueIndex(0, 5, -1), null);
});

test("nextQueueIndex: refused at the bottom, not wrapped", () => {
  assert.equal(nextQueueIndex(4, 5, 1), null);
});

test("nextQueueIndex: the only card in a queue of one can move neither way", () => {
  assert.equal(nextQueueIndex(0, 1, -1), null);
  assert.equal(nextQueueIndex(0, 1, 1), null);
});

test("shouldDeferDraw: defers exactly when a card is being dragged", () => {
  assert.equal(shouldDeferDraw(null), false);
  assert.equal(shouldDeferDraw(undefined), false);
  assert.equal(shouldDeferDraw({}), true);
});

const stages = [
  { name: "backlog", next: "refining" },
  { name: "refining", next: "review" },
  { name: "review", next: undefined },
];

test("startableRule: the first stage, into its own declared next, is startable", () => {
  assert.equal(startableRule("backlog", "refining", stages), true);
});

test("startableRule: any stage but the first is never startable", () => {
  assert.equal(startableRule("refining", "review", stages), false);
});

test("startableRule: the first stage into anything but its declared next is refused", () => {
  assert.equal(startableRule("backlog", "review", stages), false);
});

test("startableRule: an empty stage list has no first stage to start out of", () => {
  assert.equal(startableRule("backlog", "refining", []), false);
});

// Compared field by field, not with assert.deepEqual on the whole object:
// controlsForMode runs inside the vm sandbox, so an object it returns and an
// object literal written in this file are cross-realm and never
// reference-equal even with identical own properties.
const controlFields = ["tick", "unblockAll", "diagnose", "handBack", "dragStart"];

test("controlsForMode: observe offers none of the five mutating controls", () => {
  const got = controlsForMode("observe");
  for (const f of controlFields) assert.equal(got[f], false, `${f} in observe`);
});

test("controlsForMode: auto and manual both offer every control — only observe differs", () => {
  for (const mode of ["auto", "manual"]) {
    const got = controlsForMode(mode);
    for (const f of controlFields) assert.equal(got[f], true, `${f} in ${mode}`);
  }
});

test("isHttpUrl: only http and https pass, case-insensitively", () => {
  assert.equal(isHttpUrl("https://github.com/x/y/issues/1"), true);
  assert.equal(isHttpUrl("HTTP://example.com"), true);
  assert.equal(isHttpUrl("javascript:alert(1)"), false);
  assert.equal(isHttpUrl("data:text/html,<script>1</script>"), false);
  assert.equal(isHttpUrl(""), false);
  assert.equal(isHttpUrl(undefined), false);
});

// ---- staleThresholdMs -------------------------------------------------------

test("staleThresholdMs: absent pollNs falls back to 10 minutes", () => {
  assert.equal(staleThresholdMs(undefined), 10 * 60 * 1000);
  assert.equal(staleThresholdMs(0), 10 * 60 * 1000);
});

test("staleThresholdMs: is 2x the configured poll interval", () => {
  assert.equal(staleThresholdMs(5 * 60 * 1e9), 10 * 60 * 1000); // 5m poll -> 10m
});

test("staleThresholdMs: floored at 60s, so a fast test poll does not call every source stale between polls", () => {
  assert.equal(staleThresholdMs(100 * 1e6), 60000); // 100ms poll, 2x would be 200ms
});

// ---- sourceDegraded ----------------------------------------------------------

test("sourceDegraded: before the first refresh completes, nothing is degraded", () => {
  assert.equal(sourceDegraded({ listError: "boom" }, 0, 0, 1_000_000), false);
});

test("sourceDegraded: a source with configuration problems is never degraded", () => {
  assert.equal(sourceDegraded({ problems: ["bad workdir"], listError: "boom" }, 1000, 0, 1_000_000), false);
});

test("sourceDegraded: a non-empty listError is degraded", () => {
  assert.equal(sourceDegraded({ listError: "exit 1" }, 1000, 0, 1_000_000), true);
});

test("sourceDegraded: never listed at all (no lastListedAt) is degraded", () => {
  assert.equal(sourceDegraded({}, 1000, 0, 1_000_000), true);
});

test("sourceDegraded: listed well within the stale threshold is not degraded", () => {
  const now = 1_000_000;
  const s = { lastListedAt: new Date(now - 1000).toISOString() };
  assert.equal(sourceDegraded(s, 1000, 0, now), false); // 1s old, 10m fallback threshold
});

test("sourceDegraded: exactly at the stale threshold's edge, older is degraded", () => {
  const now = 1_000_000_000;
  const pollNs = 100 * 1e6; // 100ms poll -> 60s floor
  const justUnder = { lastListedAt: new Date(now - 59_000).toISOString() };
  const justOver = { lastListedAt: new Date(now - 61_000).toISOString() };
  assert.equal(sourceDegraded(justUnder, 1000, pollNs, now), false);
  assert.equal(sourceDegraded(justOver, 1000, pollNs, now), true);
});

test("sourceDegraded: with pollNs absent, the 10-minute fallback applies", () => {
  const now = 1_000_000_000;
  const justUnder = { lastListedAt: new Date(now - 9 * 60 * 1000).toISOString() };
  const justOver = { lastListedAt: new Date(now - 11 * 60 * 1000).toISOString() };
  assert.equal(sourceDegraded(justUnder, 1000, 0, now), false);
  assert.equal(sourceDegraded(justOver, 1000, 0, now), true);
});
