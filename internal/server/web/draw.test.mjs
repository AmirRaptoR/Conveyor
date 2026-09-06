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

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import path from "node:path";
import vm from "node:vm";

const here = path.dirname(fileURLToPath(import.meta.url));
const html = readFileSync(path.join(here, "index.html"), "utf8");

const scriptStart = html.indexOf("<script>") + "<script>".length;
const scriptEnd = html.indexOf("const es = new EventSource(");
if (scriptStart < 0 || scriptEnd < 0 || scriptEnd <= scriptStart) {
  throw new Error("could not find the board script in index.html");
}
const source = html.slice(scriptStart, scriptEnd);

function makeElement() {
  const el = {
    id: "", className: "", textContent: "", innerHTML: "",
    hidden: false, disabled: false, value: "",
    dataset: {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    style: {},
    onclick: null,
    addEventListener() {}, removeEventListener() {},
    setAttribute() {}, getAttribute() { return null; },
    querySelectorAll() { return []; },
    closest() { return null; },
    contains() { return false; },
    focus() {}, click() {},
  };
  el.querySelector = () => makeElement();
  return el;
}

function elementFor(cache, sel) {
  if (!cache.has(sel)) cache.set(sel, makeElement());
  return cache.get(sel);
}

// A fresh vm context running the real script — draw() included, unstubbed —
// against a frozen `Date.now()` so duration text is deterministic across a
// test's assertions without sleeping.
function page(nowMsValue) {
  const cache = new Map();
  const el = sel => elementFor(cache, sel);
  let fetchStub = async () => ({ ok: true, status: 200, headers: { get: () => null }, json: async () => ({}) });
  const sandbox = {
    document: {
      querySelector: sel => el(sel),
      querySelectorAll: () => [],
      addEventListener() {},
      activeElement: null,
    },
    $: sel => el(sel),
    fetch: async (...args) => fetchStub(...args),
    console,
    setTimeout: () => 0,
    clearTimeout() {},
    queueMicrotask: fn => { try { fn(); } catch { /* openFromHash, unexercised here */ } },
    alert() {},
    confirm: () => true,
    navigator: {},
    location: { hash: "", pathname: "/" },
    history: { replaceState() {} },
    addEventListener() {},
    removeEventListener() {},
    CSS: { escape: s => String(s) },
    Date, JSON, Math, Set, Map, Array, encodeURIComponent, decodeURIComponent, isNaN, Number,
    ResizeObserver: class { observe() {} disconnect() {} unobserve() {} },
  };
  vm.createContext(sandbox);
  vm.runInContext(source, sandbox);
  if (nowMsValue !== undefined) {
    // nowMs() is Date.now() - skew; freezing Date.now() itself is simplest
    // and keeps every other Date-based helper (lastListedAt parsing, etc.)
    // consistent with the same instant.
    sandbox.Date.now = () => nowMsValue;
  }
  return { sandbox, el, setFetch: fn => { fetchStub = fn; } };
}

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

// `state` lives behind a `let` at the top of the script, invisible from
// outside a vm context the way a real module's private scope would be — so
// the one way to seed it is the same way the page itself does: a real
// load() against a stubbed /api/state response. load() calls draw() itself
// once its json lands, so this also performs the first draw.
async function withState(p, state) {
  p.setFetch(async () => ({
    ok: true, status: 200, headers: { get: () => null }, json: async () => state,
  }));
  await p.sandbox.load();
}

// ---- #warnings --------------------------------------------------------------

test("draw: every warning string is visible in #warnings, rendered whole", async () => {
  const p = page();
  await withState(p, baseState({ warnings: ["s1: source \"s1\": list exited 1 (boom)"] }));
  const rendered = p.el("#warnings").innerHTML;
  assert.match(rendered, /s1: source &quot;s1&quot;: list exited 1 \(boom\)/);
});

test("draw: #warnings is empty (innerHTML === \"\") when warnings is empty or absent", async () => {
  const p = page();
  await withState(p, baseState({ warnings: [] }));
  assert.equal(p.el("#warnings").innerHTML, "");

  const p2 = page();
  await withState(p2, baseState({ warnings: undefined }));
  assert.equal(p2.el("#warnings").innerHTML, "");
});

test("draw: #warnings is its own block, distinct from .faults", async () => {
  const p = page();
  await withState(p, baseState({ warnings: ["s1: boom"] }));
  assert.match(p.el("#warnings").innerHTML, /class="warnings"/);
  assert.doesNotMatch(p.el("#warnings").innerHTML, /class="faults"/);
});

test("draw: a warning with HTML metacharacters renders as text, no element created", async () => {
  const p = page();
  await withState(p, baseState({ warnings: ["s1: <img src=x onerror=alert(1)>"] }));
  const rendered = p.el("#warnings").innerHTML;
  assert.match(rendered, /&lt;img src=x onerror=alert\(1\)&gt;/);
  assert.doesNotMatch(rendered, /<img/);
});

// ---- masthead degraded note --------------------------------------------------

test("draw: no source degraded leaves the masthead line unchanged from today", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = page(now);
  await withState(p, baseState({
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: "2026-09-06T12:00:00Z" }],
  }));
  assert.doesNotMatch(p.el("#line1").innerHTML, /not listing/);
});

test("draw: while a source is degraded, the masthead names how many", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = page(now);
  await withState(p, baseState({
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", listError: "boom" }],
  }));
  assert.match(p.el("#line1").innerHTML, /1 source not listing/);
});

test("draw: before the first refresh completes, no source is degraded and the masthead carries no note", async () => {
  const p = page();
  await withState(p, baseState({ updatedAt: undefined, sources: [{ name: "s1", provider: "fake", workdir: "/repo" }] }));
  assert.doesNotMatch(p.el("#line1").innerHTML, /not listing/);
});

// ---- #sources chip ------------------------------------------------------------

test("draw: a degraded chip carries a marker and an unknown item count; a healthy empty source shows 0", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = page(now);
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
  const p = page();
  await withState(p, baseState({ sources: [{ name: "s1", provider: "fake", workdir: "/repo" }] }));
  assert.match(p.el("#sources").innerHTML, /not listed yet/);
});

test("draw: a source carrying configuration problems shows only its cannot-run badge", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = page(now);
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
  const stillFresh = page(now);
  await withState(stillFresh, baseState({
    pollNs: 100 * 1e6, // 100ms poll -> 60s floor
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: new Date(now - 59_000).toISOString() }],
  }));
  assert.doesNotMatch(stillFresh.el("#line1").innerHTML, /not listing/);

  const stale = page(now);
  await withState(stale, baseState({
    pollNs: 100 * 1e6,
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: new Date(now - 61_000).toISOString() }],
  }));
  assert.match(stale.el("#line1").innerHTML, /not listing/);
});

test("draw: with pollNs absent, the 10-minute fallback applies", async () => {
  const now = Date.parse("2026-09-06T12:00:00Z");
  const stillFresh = page(now);
  await withState(stillFresh, baseState({
    pollNs: 0,
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: new Date(now - 9 * 60 * 1000).toISOString() }],
  }));
  assert.doesNotMatch(stillFresh.el("#line1").innerHTML, /not listing/);

  const stale = page(now);
  await withState(stale, baseState({
    pollNs: 0,
    sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: new Date(now - 11 * 60 * 1000).toISOString() }],
  }));
  assert.match(stale.el("#line1").innerHTML, /not listing/);
});

// ---- working column empty text -----------------------------------------------

test("draw: with no source degraded, an empty column's text is unchanged from today", async () => {
  const p = page();
  await withState(p, baseState({ sources: [{ name: "s1", provider: "fake", workdir: "/repo", lastListedAt: "2026-09-06T12:00:00Z" }] }));
  assert.match(p.el("#rail").innerHTML, /Nothing here/);
  assert.doesNotMatch(p.el("#rail").innerHTML, /Picture incomplete/);
});

test("draw: while a source is degraded, a working column with no items says the picture is incomplete", async () => {
  const p = page();
  await withState(p, baseState({ sources: [{ name: "s1", provider: "fake", workdir: "/repo", listError: "boom" }] }));
  assert.match(p.el("#rail").innerHTML, /Picture incomplete/);
});

// ---- no accumulated variable --------------------------------------------------

test("draw: calling draw() twice with the same state and a frozen clock produces identical DOM", async () => {
  const now = Date.parse("2026-09-06T12:00:05Z");
  const p = page(now);
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
  p.sandbox.draw();
  const second = {
    warnings: p.el("#warnings").innerHTML,
    sources: p.el("#sources").innerHTML,
    line1: p.el("#line1").innerHTML,
    rail: p.el("#rail").innerHTML,
  };
  assert.deepEqual(second, first);
});

test("draw: a clean state after a degraded one leaves no warning, no masthead note and no stale marker behind", async () => {
  const p = page();
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
  const p = page();
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
