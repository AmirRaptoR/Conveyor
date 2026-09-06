// Proves the UI test path: a browser-free interaction test, run under
// `node --test` with no npm install and no network. See
// README.md#running-the-checks for how to add another one.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadFunctions } from "./testutil.mjs";

const { formatDuration } = loadFunctions("formatDuration");

test("formatDuration renders the largest two non-zero units", () => {
  assert.equal(formatDuration(0), "<1s");
  assert.equal(formatDuration(999), "<1s");
  assert.equal(formatDuration(1_000), "1s");
  assert.equal(formatDuration(65_000), "1m");
  assert.equal(formatDuration(3 * 3_600_000 + 5 * 60_000), "3h 5m");
  assert.equal(formatDuration(2 * 86_400_000 + 4 * 3_600_000), "2d 4h");
});
