// Proves the UI test path: a browser-free interaction test, run under
// `node --test` with no npm install and no network. See
// README.md#running-the-checks for how to add another one.
//
// formatDuration mirrors report.go's formatDuration exactly, so this pins a
// cross-language contract: at most two units, the smaller dropped when it is
// zero, and never a negative number. It lives in board.js, which draws the
// board and therefore reaches for a DOM as it is evaluated — page() supplies
// the fake one and hands back the module's real export.
import { test } from "node:test";
import assert from "node:assert/strict";
import { page } from "./testutil.mjs";

const { formatDuration } = (await page()).mod;

test("formatDuration renders the largest two non-zero units", () => {
  assert.equal(formatDuration(0), "<1s");
  assert.equal(formatDuration(999), "<1s");
  assert.equal(formatDuration(1_000), "1s");
  assert.equal(formatDuration(65_000), "1m");
  assert.equal(formatDuration(3 * 3_600_000 + 5 * 60_000), "3h 5m");
  assert.equal(formatDuration(2 * 86_400_000 + 4 * 3_600_000), "2d 4h");
});

// The contract report.go's own formatDuration keeps, pinned here because
// nothing else compares the two: a clock-skewed device or a timestamp from
// the future must read as "<1s", never as a negative duration.
test("formatDuration: a negative or future-dated input reads as <1s", () => {
  assert.equal(formatDuration(-1), "<1s");
  assert.equal(formatDuration(-86_400_000), "<1s");
  assert.equal(formatDuration(NaN), "<1s");
});

// At most two units, and the smaller one dropped when it is zero.
test("formatDuration: never three units, and a zero smaller unit is dropped", () => {
  assert.equal(formatDuration(2 * 86_400_000), "2d");
  assert.equal(formatDuration(2 * 86_400_000 + 4 * 3_600_000 + 30 * 60_000), "2d 4h");
  assert.equal(formatDuration(3 * 3_600_000), "3h");
  assert.equal(formatDuration(3 * 3_600_000 + 5 * 60_000 + 30_000), "3h 5m");
  assert.equal(formatDuration(5 * 60_000), "5m");
  assert.equal(formatDuration(5 * 60_000 + 30_000), "5m");
});
