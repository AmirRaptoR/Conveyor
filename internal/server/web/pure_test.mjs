// Exercises the pure logic in index.html's "---- pure helpers ----" region —
// no DOM, no npm install, no network. Run with:
//
//   node --test internal/server/web/pure_test.mjs
//
// The region is pulled out of the live file by its marker comments and
// evaluated with vm, rather than duplicated here, so this test fails the
// moment the shipped functions change shape instead of silently testing a
// stale copy. See index.html's own comment on that region for why these four
// functions in particular are the ones written to take plain values instead
// of DOM nodes.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import vm from "node:vm";

const here = path.dirname(fileURLToPath(import.meta.url));
const html = readFileSync(path.join(here, "index.html"), "utf8");

const start = html.indexOf("// ---- pure helpers");
const end = html.indexOf("// ---- end pure helpers");
if (start < 0 || end < 0 || end <= start) {
  throw new Error("could not find the pure-helpers region in index.html");
}
const region = html.slice(start, end);

const sandbox = {};
vm.createContext(sandbox);
vm.runInContext(
  region + "\nglobalThis.__pure = { focusKeyOf, nextQueueIndex, shouldDeferDraw, startableRule, controlsForMode, isHttpUrl };",
  sandbox,
);
const { focusKeyOf, nextQueueIndex, shouldDeferDraw, startableRule, controlsForMode, isHttpUrl } = sandbox.__pure;

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
