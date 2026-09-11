// Shared harness for the board's UI tests: loads the page's real ES modules
// and runs them against a minimal, hand-rolled DOM — no browser, no build
// step, no npm install, just node:module and node:test from a stock Node.
//
// The page used to be one inline <script> in index.html, and these tests used
// to slice that string out of the file and evaluate it under node:vm. Now that
// every function ships in a module of its own, they are imported instead,
// which is both simpler and stricter: a test exercises exactly the file the
// server serves, with its real imports resolved.
//
// Two consequences of the move are worth knowing before writing a test here.
//
// A fresh module graph per test. ES modules are cached per URL, so importing
// "./board.js" a second time hands back the same instance — and with it
// whatever `state`, `blocks` or open-panel id the previous test left behind.
// `page()` gives each test its own graph by importing the entry with a unique
// `?v=` query and registering a resolve hook that carries that query down
// every relative import, so `./drag.js` from `board.js?v=7` becomes
// `drag.js?v=7` and the whole board is instantiated fresh.
//
// No stubbing of one module's functions from another. A vm sandbox's globals
// were writable, so a test could swap `draw` or `fault` out and count calls.
// A module's bindings are not, so these tests observe what those functions
// actually did instead: `faults(p)` reads the masthead line fault() writes,
// and `p.el("#rail").writes` counts the rebuilds draw() performs. That is a
// stronger assertion, not a weaker one — it fails if the effect stops
// happening, not merely if a name stops being called.
import { registerHooks } from "node:module";

// Carries `?v=N` from a module's own URL down to every relative import it
// makes, which is what makes one `import("./board.js?v=N")` instantiate a
// whole private copy of the board's module graph.
registerHooks({
  resolve(specifier, context, next) {
    if (specifier.startsWith(".") && !specifier.includes("?")) {
      const m = /[?&]v=(\d+)/.exec(context.parentURL || "");
      if (m) return next(`${specifier}?v=${m[1]}`, context);
    }
    return next(specifier, context);
  },
});

// One fake element per selector, created lazily and cached for the lifetime of
// a single page() — so `$("#stop").querySelector(".hand")` and a test's own
// `p.el("#stop .hand")` see the same object, the way a real DOM's element
// identity works. Generic rather than a real parent/child tree: every function
// under test either takes its element by reference (handBack(id, btn)) or
// reaches it through one of a handful of fixed selectors, never through
// structural traversal a tree would be needed for.
//
// `innerHTML` and `textContent` are accessors rather than plain fields so the
// harness can count and record what was written to them (see `writes` and
// `textWrites`); everything else is a plain property a test can read back.
function makeElement(cache, sel) {
  let innerHTML = "";
  let textContent = "";
  const el = {
    id: "", className: "",
    hidden: false, disabled: false, value: "",
    dataset: {},
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    style: {},
    onclick: null,
    writes: 0,
    textWrites: [],
    addEventListener() {}, removeEventListener() {},
    setAttribute() {}, getAttribute() { return null; },
    querySelectorAll() { return []; },
    closest() { return null; },
    contains() { return false; },
    focus() {}, click() {},
    // Enough of <dialog> for report.js's openAsk/openReport to run to
    // completion rather than throw on the one call neither makes
    // conditionally: `open` tracks the two methods that flip it.
    open: false,
    showModal() { this.open = true; }, close() { this.open = false; },
  };
  Object.defineProperty(el, "innerHTML", {
    get: () => innerHTML,
    set: v => { innerHTML = v; el.writes++; },
    enumerable: true,
  });
  Object.defineProperty(el, "textContent", {
    get: () => textContent,
    set: v => { textContent = v; el.textWrites.push(v); },
    enumerable: true,
  });
  el.querySelector = sub => elementFor(cache, `${sel} ${sub}`);
  return el;
}

function elementFor(cache, sel) {
  if (!cache.has(sel)) cache.set(sel, makeElement(cache, sel));
  return cache.get(sel);
}

function define(name, value) {
  Object.defineProperty(globalThis, name, { value, configurable: true, writable: true });
}

const realNow = Date.now;
let generation = 0;

// A response good enough for the handful of calls a test does not exercise
// directly (loadHistory, drawNotify's push checks, ...): success, empty body,
// no interesting headers.
export function okResponse() {
  return { ok: true, status: 200, headers: { get: () => null }, text: async () => "", json: async () => ({}) };
}

// Installs a fresh fake DOM as the globals the page's modules read, then
// imports a private copy of the board's module graph against it. `nowMsValue`
// freezes Date.now() so duration text is deterministic across a test's
// assertions without sleeping — nowMs() is Date.now() - skew, and freezing
// Date.now() itself keeps every other Date-based helper consistent with the
// same instant.
export async function page({ fetch: fetchImpl, now } = {}) {
  const cache = new Map();
  const el = sel => elementFor(cache, sel);
  const calls = { fetch: [], alert: [], timers: [] };
  let fetchStub = fetchImpl || (async () => okResponse());

  define("document", {
    querySelector: sel => el(sel),
    querySelectorAll: () => [],
    addEventListener() {},
    activeElement: null,
    hidden: false,
    body: el("body"),
    createElement: () => makeElement(cache, "<created>"),
  });
  define("fetch", async (...args) => { calls.fetch.push(args); return fetchStub(...args); });
  define("setTimeout", (fn, ms) => { calls.timers.push({ fn, ms }); return calls.timers.length; });
  define("clearTimeout", () => {});
  define("queueMicrotask", fn => { try { fn(); } catch { /* openFromHash, unexercised here */ } });
  define("alert", msg => calls.alert.push(msg));
  define("confirm", () => true);
  define("navigator", {});
  define("location", { hash: "", pathname: "/" });
  define("history", { replaceState() {} });
  define("addEventListener", () => {});
  define("removeEventListener", () => {});
  define("CSS", { escape: s => String(s) });
  define("ResizeObserver", class { observe() {} disconnect() {} unobserve() {} });
  // report.js's openAsk feature-tests its opener with `instanceof HTMLElement`
  // before it can safely restore focus to it — nothing in this fake DOM is a
  // real one, so this exists only so that check does not throw.
  // document.activeElement stays null, so the check is always false here.
  define("HTMLElement", class {});
  Date.now = now === undefined ? realNow : () => now;

  const v = ++generation;
  const mods = await Promise.all(
    ["board.js", "shared.js", "panel.js", "drag.js", "report.js", "rail.js", "device.js", "inbox.js"]
      .map(name => import(`./${name}?v=${v}`)));
  const mod = Object.assign({}, ...mods.map(m => ({ ...m })));

  return { el, calls, mod, setFetch: fn => { fetchStub = fn; } };
}

// Every message fault() wrote, in order. fault() is the page's one failure
// channel — it puts the lamp down and writes the message into the masthead
// line — and #line1's textContent is written by nothing else except the tick
// button's own 409 notice, which no test that reads this also triggers.
export function faults(p) {
  return p.el("#line1").textWrites;
}

// `state` is a module-level `let` in shared.js, private to it the way it was
// private to the page's one scope — so the only way to seed it is the way the
// page itself does: a real load() against a stubbed /api/state response.
// load() calls draw() once its json lands, so this also performs a first draw.
export async function withState(p, state) {
  p.setFetch(async () => ({ ok: true, status: 200, headers: { get: () => null }, json: async () => state }));
  await p.mod.load();
}
