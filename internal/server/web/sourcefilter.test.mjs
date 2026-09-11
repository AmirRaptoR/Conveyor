// Exercises the Pipeline view's source filter (#93): the source chips as the
// filter, the rail narrowing to one source, rank/tally/paging/empty-text
// staying honest about the unfiltered picture, and the filter surviving a
// redraw. Same harness as draw.test.mjs — a fake DOM, the page's real
// modules, no browser. Run with:
//
//   node --test internal/server/web/sourcefilter.test.mjs

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
    sources: [
      { name: "s1", provider: "fake", workdir: "/repo1", lastListedAt: new Date().toISOString() },
      { name: "s2", provider: "fake", workdir: "/repo2", lastListedAt: new Date().toISOString() },
    ],
    items: [],
    warnings: [],
    updatedAt: new Date().toISOString(),
    pollNs: 0,
  }, overrides);
}

// ---- chip controls ----------------------------------------------------------

test("sources: All and every source render as buttons with aria-pressed, All pressed by default", async () => {
  const p = await page();
  await withState(p, baseState());
  const html = p.el("#sources").innerHTML;
  assert.match(html, /<button type="button" class="src-chip all selected" data-source=""\s*aria-pressed="true">All<\/button>/);
  assert.match(html, /<button type="button" class="src-chip"\s*\n?\s*data-source="s1" aria-pressed="false"/);
  assert.match(html, /<button type="button" class="src-chip"\s*\n?\s*data-source="s2" aria-pressed="false"/);
});

test("sources: pressing a chip selects it — exactly one aria-pressed=\"true\"", async () => {
  const p = await page();
  await withState(p, baseState());
  p.mod.setSourceFilter("s1");
  const html = p.el("#sources").innerHTML;
  assert.match(html, /data-source=""\s*aria-pressed="false"/);
  assert.match(html, /data-source="s1" aria-pressed="true"/);
  assert.match(html, /data-source="s2" aria-pressed="false"/);
});

test("sources: a broken chip still shows 'cannot run' and the broken class while selected", async () => {
  const p = await page();
  await withState(p, baseState({
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", problems: ["bad workdir"] }],
  }));
  p.mod.setSourceFilter("s1");
  const html = p.el("#sources").innerHTML;
  assert.match(html, /class="src-chip broken selected"/);
  assert.match(html, /cannot run/);
});

test("sources: a degraded chip still carries the degraded class and the ? count while selected", async () => {
  const p = await page();
  await withState(p, baseState({
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", listError: "boom" }],
  }));
  p.mod.setSourceFilter("s1");
  const html = p.el("#sources").innerHTML;
  assert.match(html, /class="src-chip degraded selected"/);
  assert.match(html, /<span class="n">\?<\/span>/);
});

// ---- filtering, no sorting, rank badge --------------------------------------

test("sources: selecting s1 shows only s1 cards in every #rail column, clearing shows both again", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s2:1", source: "s2", stage: "backlog", title: "s2 first" },
      { id: "s1:1", source: "s1", stage: "backlog", title: "s1 first" },
    ],
  }));
  p.mod.setSourceFilter("s1");
  let html = p.el("#rail").innerHTML;
  assert.match(html, /s1 first/);
  assert.doesNotMatch(html, /s2 first/);

  p.mod.setSourceFilter(""); // clear
  html = p.el("#rail").innerHTML;
  assert.match(html, /s1 first/);
  assert.match(html, /s2 first/);
});

test("sources: rank shows the card's position in the unfiltered column", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s2:1", source: "s2", stage: "backlog", title: "s2:1" },
      { id: "s1:1", source: "s1", stage: "backlog", title: "s1:1" },
      { id: "s2:2", source: "s2", stage: "backlog", title: "s2:2" },
      { id: "s1:2", source: "s1", stage: "backlog", title: "s1:2" },
    ],
  }));
  p.mod.setSourceFilter("s1");
  const html = p.el("#rail").innerHTML;
  // s1:1 is unfiltered index 1 -> rank 2; s1:2 is unfiltered index 3 -> rank 4.
  const around = (marker) => { const i = html.indexOf(marker); return html.slice(i, i + 400); };
  assert.match(around("s1:1"), /<span class="rank">2<\/span>/);
  assert.match(around("s1:2"), /<span class="rank">4<\/span>/);
});

// ---- tallies -----------------------------------------------------------------

test("sources: the tally counts only the items the column shows under the filter", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s1:1", source: "s1", stage: "backlog", title: "a" },
      { id: "s2:1", source: "s2", stage: "backlog", title: "b" },
      { id: "s2:2", source: "s2", stage: "backlog", title: "c" },
    ],
  }));
  p.mod.setSourceFilter("s1");
  const html = p.el("#rail").innerHTML;
  assert.match(html, /class="name">backlog<span class="count">1<\/span><\/div>/);
});

// ---- empty text --------------------------------------------------------------

test("sources: an empty working station under the filter names the source", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s2:1", source: "s2", stage: "backlog", title: "only s2" }],
  }));
  p.mod.setSourceFilter("s1");
  assert.match(p.el("#rail").innerHTML, /Nothing here from s1/);
});

test("sources: degraded still wins over the filter's empty text", async () => {
  const p = await page();
  await withState(p, baseState({
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", listError: "boom" }],
    items: [],
  }));
  p.mod.setSourceFilter("s1");
  assert.match(p.el("#rail").innerHTML, /Picture incomplete/);
  assert.doesNotMatch(p.el("#rail").innerHTML, /Nothing here from/);
});

test("sources: an empty terminal column renders no empty-state text, filtered or not", async () => {
  const p = await page();
  await withState(p, baseState({ items: [] }));
  p.mod.setSourceFilter("s1");
  const html = p.el("#rail").innerHTML;
  // The done column's own <div class="queue"> must be empty (no .slot text).
  const doneSection = html.slice(html.indexOf('class="name">done'));
  assert.doesNotMatch(doneSection, /class="slot"/);
});

// ---- paging -------------------------------------------------------------------

test("sources: the done ledger pages filtered items ten at a time, and switching sources resets to ten", async () => {
  const p = await page();
  const items = [];
  for (let i = 0; i < 25; i++) items.push({ id: `s1:${i}`, source: "s1", stage: "done", title: `s1 item ${i}`, finishedAt: new Date().toISOString() });
  for (let i = 0; i < 25; i++) items.push({ id: `s2:${i}`, source: "s2", stage: "done", title: `s2 item ${i}`, finishedAt: new Date().toISOString() });
  await withState(p, baseState({ items }));
  p.mod.setSourceFilter("s1");
  // Each card's id appears exactly once (in data-id) — a card's title is
  // written twice (data-title and the visible span), so it over-counts.
  const cardsOf = (html, src) => (html.match(new RegExp(`data-id="${src}:`, "g")) || []).length;
  let html = p.el("#rail").innerHTML;
  assert.match(html, /Show 10 more · 15 older/);
  assert.equal(cardsOf(html, "s1"), 10);

  // Expand to 20 — the same increment the ".more" button's click performs.
  p.mod.expandTerminus("done");
  html = p.el("#rail").innerHTML;
  assert.equal(cardsOf(html, "s1"), 20);

  // Switching to s2 shows s2's own first ten, not a continuation of s1's
  // expansion — changing the filter value resets every terminal column back
  // to ten.
  p.mod.setSourceFilter("s2");
  html = p.el("#rail").innerHTML;
  assert.equal(cardsOf(html, "s2"), 10);
  assert.equal(cardsOf(html, "s1"), 0);
});

test("sources: a redraw that does not change the filter value keeps a column's expansion", async () => {
  const p = await page();
  const items = [];
  for (let i = 0; i < 15; i++) items.push({ id: `s1:${i}`, source: "s1", stage: "done", title: `s1 item ${i}`, finishedAt: new Date().toISOString() });
  await withState(p, baseState({ items }));
  const cardsOf = html => (html.match(/data-id="s1:/g) || []).length;
  p.mod.expandTerminus("done");
  assert.equal(cardsOf(p.el("#rail").innerHTML), 15);
  await withState(p, baseState({ items })); // same filter value ("") across this redraw
  assert.equal(cardsOf(p.el("#rail").innerHTML), 15);
});

// ---- filter survives new state / vanished source ----------------------------

test("sources: the filter survives a later load()/draw() with changed state", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [{ id: "s1:1", source: "s1", stage: "backlog", title: "first" }],
  }));
  p.mod.setSourceFilter("s1");
  await withState(p, baseState({
    items: [
      { id: "s1:1", source: "s1", stage: "backlog", title: "first" },
      { id: "s2:1", source: "s2", stage: "backlog", title: "new s2 item" },
    ],
  }));
  const html = p.el("#rail").innerHTML;
  assert.match(html, /first/);
  assert.doesNotMatch(html, /new s2 item/);
});

test("sources: a vanished source resets both controls to All", async () => {
  const p = await page();
  await withState(p, baseState({
    items: [
      { id: "s1:1", source: "s1", stage: "backlog", title: "s1 item" },
      { id: "s2:1", source: "s2", stage: "backlog", title: "s2 item" },
    ],
  }));
  p.mod.setSourceFilter("s1");
  assert.match(p.el("#rail").innerHTML, /s1 item/);
  assert.doesNotMatch(p.el("#rail").innerHTML, /s2 item/);

  await withState(p, baseState({
    sources: [{ name: "s2", provider: "fake", workdir: "/repo2", lastListedAt: new Date().toISOString() }],
    items: [{ id: "s2:1", source: "s2", stage: "backlog", title: "s2 item" }],
  }));
  const html = p.el("#rail").innerHTML;
  assert.match(html, /s2 item/);
  assert.match(p.el("#sources").innerHTML, /class="src-chip all selected"/);
});

// ---- no network ---------------------------------------------------------------

test("sources: selecting or clearing the filter makes no fetch call", async () => {
  const p = await page();
  await withState(p, baseState({ items: [{ id: "s1:1", source: "s1", stage: "backlog", title: "x" }] }));
  const before = p.calls.fetch.length;
  p.mod.setSourceFilter("s1");
  p.mod.setSourceFilter("");
  assert.equal(p.calls.fetch.length, before);
});
