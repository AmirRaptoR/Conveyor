// Exercises draw() itself against real /api/state-shaped state objects — no
// stub swapped in for draw the way writes.test.mjs does, since this file is
// what asserts what draw() actually renders. No DOM library: elements are
// generic stand-ins whose innerHTML is a plain string draw() writes into and
// this file reads back with assert.match/assert.equal, never a real parse —
// draw() itself never reads its own innerHTML back, only assigns it, so a
// string is enough to observe everything it does. No npm install, no
// network, run with:
//
//   node --test internal/server/web/draw.test.mjs
//
// board.js is imported, not sliced out of index.html: await page() installs the
// fake DOM as globals and hands back a private copy of the board's module
// graph, so one test's element state can never bleed into the next. See
// testutil.mjs.

import { test } from "node:test";
import assert from "node:assert/strict";
import { page, withState } from "./testutil.mjs";

function baseState(overrides) {
  return Object.assign({
    stages: [
      { name: "backlog", next: "working" },
      { name: "working", next: "done" },
      { name: "done", terminal: true },
    ],
    sources: [{ name: "s1", provider: "fake", workdir: "/repo" }],
    items: [],
    warnings: [],
    order: [],
    updatedAt: "2026-09-06T12:00:00Z",
    pollNs: 0,
  }, overrides);
}

// ---- #warnings --------------------------------------------------------------

test("draw: every warning string is visible in #warnings, rendered whole", async () => {
  const p = await page();
  await withState(p, baseState({ warnings: ["s1: source \"s1\": list exited 1 (boom)"] }));
  const rendered = p.el("#warnings").innerHTML;
  assert.match(rendered, /s1: source &quot;s1&quot;: list exited 1 \(boom\)/);
});

test("draw: #warnings is empty (innerHTML === \"\") when warnings is empty or absent", async () => {
  const p = await page();
  await withState(p, baseState({ warnings: [] }));
  assert.equal(p.el("#warnings").innerHTML, "");

  const p2 = await page();
  await withState(p2, baseState({ warnings: undefined }));
  assert.equal(p2.el("#warnings").innerHTML, "");
});

test("draw: #warnings is its own block, distinct from .faults", async () => {
  const p = await page();
  await withState(p, baseState({ warnings: ["s1: boom"] }));
  assert.match(p.el("#warnings").innerHTML, /class="warnings"/);
  assert.doesNotMatch(p.el("#warnings").innerHTML, /class="faults"/);
});

test("draw: a warning with HTML metacharacters renders as text, no element created", async () => {
  const p = await page();
  await withState(p, baseState({ warnings: ["s1: <img src=x onerror=alert(1)>"] }));
  const rendered = p.el("#warnings").innerHTML;
  assert.match(rendered, /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.doesNotMatch(rendered, /<img/);
});

// ---- masthead degraded note --------------------------------------------------

test("draw: no source degraded leaves the masthead line unchanged from today", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = await page({ now });
  await withState(p, baseState({
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: "2026-09-06T12:00:00Z" }],
  }));
  assert.doesNotMatch(p.el("#line1").innerHTML, /not listing/);
});

test("draw: while a source is degraded, the masthead names how many", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = await page({ now });
  await withState(p, baseState({
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", listError: "boom" }],
  }));
  assert.match(p.el("#line1").innerHTML, /1 source not listing/);
});

test("draw: before the first refresh completes, no source is degraded, the masthead carries no note, and every column's empty text is today's", async () => {
  const p = await page();
  await withState(p, baseState({ updatedAt: undefined, sources: [{ name: "s1", provider: "fake", workdir: "/repo" }] }));
  assert.doesNotMatch(p.el("#line1").innerHTML, /not listing/);
  assert.doesNotMatch(p.el("#sources").innerHTML, /degraded/);
  assert.match(p.el("#rail").innerHTML, /Nothing here/);
  assert.doesNotMatch(p.el("#rail").innerHTML, /Picture incomplete/);
});

// ---- #sources chip ------------------------------------------------------------

test("draw: a degraded chip carries a marker and an unknown item count; a healthy empty source shows 0", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = await page({ now });
  await withState(p, baseState({
    sources: [
      { name: "s1", provider: "fake", workdir: "/repo1", listError: "boom" },
      { name: "s2", provider: "fake", workdir: "/repo2", lastListedAt: "2026-09-06T12:00:00Z" },
    ],
  }));
  const rendered = p.el("#sources").innerHTML;
  const chipAround = marker => {
    const i = rendered.indexOf(marker);
    return rendered.slice(Math.max(0, i - 400), i + 200);
  };
  const s1chip = chipAround(">s1<");
  const s2chip = chipAround(">s2<");
  assert.match(s1chip, /degraded/);
  assert.match(s1chip, /<span class="n">\?<\/span>/);
  assert.doesNotMatch(s2chip, /class="src-chip degraded"/);
  assert.match(s2chip, /<span class="n">0<\/span>/);
});

test("draw: a source with no lastListedAt says it has not listed yet", async () => {
  const p = await page();
  await withState(p, baseState({ sources: [{ name: "s1", provider: "fake", workdir: "/repo" }] }));
  assert.match(p.el("#sources").innerHTML, /not listed yet/);
});

test("draw: a source carrying configuration problems shows only its cannot-run badge", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = await page({ now });
  await withState(p, baseState({
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", problems: ["bad workdir"] }],
  }));
  const rendered = p.el("#sources").innerHTML;
  assert.match(rendered, /cannot run/);
  assert.doesNotMatch(rendered, /class="src-chip broken degraded"/);
  assert.doesNotMatch(rendered, /not listed yet/);
});

// ---- stale threshold, end to end through draw() ------------------------------

test("draw: a listing older than the stale threshold is degraded (60s floor, small pollNs)", async () => {
  const now = Date.parse("2026-09-06T12:00:00Z");
  const stillFresh = await page({ now });
  await withState(stillFresh, baseState({
    pollNs: 100 * 1e6, // 100ms poll -> 60s floor
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: new Date(now - 59_000).toISOString() }],
  }));
  assert.doesNotMatch(stillFresh.el("#line1").innerHTML, /not listing/);

  const stale = await page({ now });
  await withState(stale, baseState({
    pollNs: 100 * 1e6,
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: new Date(now - 61_000).toISOString() }],
  }));
  assert.match(stale.el("#line1").innerHTML, /not listing/);
});

test("draw: with pollNs absent, the 10-minute fallback applies", async () => {
  const now = Date.parse("2026-09-06T12:00:00Z");
  const stillFresh = await page({ now });
  await withState(stillFresh, baseState({
    pollNs: 0,
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: new Date(now - 9 * 60 * 1000).toISOString() }],
  }));
  assert.doesNotMatch(stillFresh.el("#line1").innerHTML, /not listing/);

  const stale = await page({ now });
  await withState(stale, baseState({
    pollNs: 0,
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: new Date(now - 11 * 60 * 1000).toISOString() }],
  }));
  assert.match(stale.el("#line1").innerHTML, /not listing/);
});

// ---- working column empty text -----------------------------------------------

test("draw: with no source degraded, an empty column's text is unchanged from today", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = await page({ now });
  await withState(p, baseState({ sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: "2026-09-06T12:00:00Z" }] }));
  assert.match(p.el("#rail").innerHTML, /Nothing here/);
  assert.doesNotMatch(p.el("#rail").innerHTML, /Picture incomplete/);
});

test("draw: while a source is degraded, a working column with no items says the picture is incomplete", async () => {
  const p = await page();
  await withState(p, baseState({ sources: [{ name: "s1", provider: "fake", workdir: "/repo", listError: "boom" }] }));
  assert.match(p.el("#rail").innerHTML, /Picture incomplete/);
});

// ---- no accumulated variable --------------------------------------------------

test("draw: calling draw() twice with the same state and a frozen clock produces identical DOM", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = await page({ now });
  await withState(p, baseState({
    warnings: ["s1: boom"],
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", listError: "boom" }],
  }));
  const first = {
    warnings: p.el("#warnings").innerHTML,
    sources: p.el("#sources").innerHTML,
    line1: p.el("#line1").innerHTML,
    rail: p.el("#rail").innerHTML,
  };
  p.mod.draw();
  const second = {
    warnings: p.el("#warnings").innerHTML,
    sources: p.el("#sources").innerHTML,
    line1: p.el("#line1").innerHTML,
    rail: p.el("#rail").innerHTML,
  };
  assert.deepEqual(second, first);
});

test("draw: a clean state after a degraded one leaves no warning, no masthead note and no stale marker behind", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = await page({ now });
  await withState(p, baseState({
    warnings: ["s1: boom"],
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", listError: "boom" }],
  }));
  assert.notEqual(p.el("#warnings").innerHTML, "");
  assert.match(p.el("#line1").innerHTML, /not listing/);

  await withState(p, baseState({
    warnings: [],
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: "2026-09-06T12:00:00Z" }],
  }));
  assert.equal(p.el("#warnings").innerHTML, "");
  assert.doesNotMatch(p.el("#line1").innerHTML, /not listing/);
  assert.doesNotMatch(p.el("#sources").innerHTML, /degraded/);
});

// ---- items dropped by a failed listing are not redrawn from an old snapshot --

test("draw: items that disappear when their source's listing fails are not left looking completed", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = await page({ now });
  await withState(p, baseState({
    items: [{ id: "s1:1", source: "s1", stage: "backlog", title: "still here a moment ago" }],
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: "2026-09-06T12:00:00Z" }],
  }));
  assert.match(p.el("#rail").innerHTML, /still here a moment ago/);

  await withState(p, baseState({
    items: [],
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", listError: "boom" }],
  }));
  assert.doesNotMatch(p.el("#rail").innerHTML, /still here a moment ago/);
  assert.match(p.el("#line1").innerHTML, /not listing/);
});

// ---- the waiting countdown --------------------------------------------------
//
// A resting item and a stuck one look identical on a board — both sit still —
// so the script says what it is waiting for and the card draws the clock.

test("draw: an item with a waiting entry gets a countdown chip", async () => {
  const p = await page();
  const until = new Date(Date.now() + 5 * 60 * 1000).toISOString();
  await withState(p, baseState({
    items: [{ id: "s1:1", source: "s1", stage: "backlog", title: "settling" }],
    waiting: { "s1:1": { until, why: "the PR must stay quiet" } },
  }));
  const rendered = p.el("#rail").innerHTML;
  assert.match(rendered, /class="wait"/);
  assert.match(rendered, /left</);
  // The instant travels on the element, so tickDurations can recount it in
  // place without draw() rebuilding the board once a second.
  assert.match(rendered, /data-until="\d+"/);
  assert.match(rendered, /the PR must stay quiet/);
});

test("draw: a wait with no deadline says so and draws no clock", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:1", source: "s1", stage: "backlog", title: "settling" }],
    waiting: { "s1:1": { why: "a human is reviewing it" } },
  }));
  const rendered = p.el("#rail").innerHTML;
  assert.match(rendered, /class="wait"/);
  assert.doesNotMatch(rendered, /data-until/);
});

test("draw: an item nobody is waiting on gets no chip", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:1", source: "s1", stage: "backlog", title: "moving" }],
  }));
  assert.doesNotMatch(p.el("#rail").innerHTML, /class="wait"/);
});

// A deadline that has passed reads as "any moment", never a negative number
// or a zero: the thing it was counting down to happens on the next poll.
test("draw: an elapsed countdown says any moment", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:1", source: "s1", stage: "backlog", title: "due" }],
    waiting: { "s1:1": { until: "2020-01-01T00:00:00Z", why: "overdue" } },
  }));
  assert.match(p.el("#rail").innerHTML, /any moment/);
});
