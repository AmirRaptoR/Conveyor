import { test } from "node:test";
import assert from "node:assert/strict";
import { page, withState } from "./testutil.mjs";

const itemId = "s1:1";

function steering(overrides = {}) {
  return Object.assign({
    runId: "run-a", stage: "working", session: "session-a", accepts: [],
    queued: 0, consumed: 0, rejected: 0, carried: 0, dropped: 0,
    malformed: 0, maxSeq: 0, live: true, commands: [],
  }, overrides);
}

function run(overrides = {}) {
  return Object.assign({
    id: "run-a", itemId, kind: "stage", to: "working", outcome: "running",
  }, overrides);
}

function select(p, value = run(), mode = "auto") {
  p.mod.selectPanelRun(value.id, itemId, "Build it", mode);
  p.mod.applySteeringSnapshot(value, mode);
}

test("live selected run offers cancel and only the advertised steering kinds", async () => {
  const p = await page();
  select(p, run({ steering: steering({ accepts: ["instruction"] }) }));
  let html = p.el("#steering").innerHTML;
  assert.match(html, /data-steer="instruction"/);
  assert.match(html, /data-cancel/);
  assert.doesNotMatch(html, /data-steer="pause"/);

  select(p, run({ id: "run-b", steering: steering({ runId: "run-b", accepts: ["pause"] }) }));
  html = p.el("#steering").innerHTML;
  assert.match(html, /data-steer="pause"/);
  assert.match(html, /data-cancel/);
  assert.doesNotMatch(html, /data-steer="instruction"/);

  select(p, run({ id: "run-c" }));
  html = p.el("#steering").innerHTML;
  assert.match(html, /data-cancel/);
  assert.doesNotMatch(html, /data-steer=/);
});

test("controls are absent for historical, non-stage, unselected, and observe-mode runs", async () => {
  const p = await page();
  select(p, run({ outcome: "success", steering: steering({ live: false, accepts: ["instruction", "pause"],
    maxSeq: 1, commands: [{ id: "c1", seq: 1, kind: "instruction", text: "old command", state: "consumed" }] }) }));
  assert.match(p.el("#steering").innerHTML, /old command/);
  assert.doesNotMatch(p.el("#steering").innerHTML, /data-steer=|data-cancel/);

  select(p, run({ kind: "move", steering: steering({ accepts: ["instruction"] }) }));
  assert.doesNotMatch(p.el("#steering").innerHTML, /data-steer=|data-cancel/);

  select(p, run({ id: "run-live", steering: steering({ runId: "run-live", accepts: ["instruction"] }) }));
  p.mod.applySteeringEvent({ kind: "steering", runId: "another-run", itemId,
    steering: steering({ runId: "another-run", accepts: ["pause"], maxSeq: 2 }) });
  assert.match(p.el("#steering").innerHTML, /data-steer="instruction"/);
  assert.doesNotMatch(p.el("#steering").innerHTML, /data-steer="pause"/);

  select(p, run({ id: "run-watch", steering: steering({ runId: "run-watch", accepts: ["instruction", "pause"] }) }), "observe");
  assert.doesNotMatch(p.el("#steering").innerHTML, /data-steer=|data-cancel/);
});

test("instruction refuses empty text client-side and sends trimmed text with the selected run and session", async () => {
  const p = await page();
  select(p, run({ steering: steering({ accepts: ["instruction"] }) }));
  const form = p.el("#steering form.steer-instruction");
  const text = p.el("#steering .steer-text");
  text.value = "   ";
  await form.onsubmit({ preventDefault() {} });
  assert.equal(p.calls.fetch.length, 0);
  assert.match(p.el("#steering .steer-fault").textContent, /instruction is required/i);

  text.value = "  Keep the public API small.  ";
  await form.onsubmit({ preventDefault() {} });
  const [url, opts] = p.calls.fetch[0];
  assert.equal(url, `/api/items/${encodeURIComponent(itemId)}/steer`);
  assert.deepEqual(JSON.parse(opts.body), {
    kind: "instruction", text: "Keep the public API small.", runId: "run-a", session: "session-a",
  });
});

test("pause posts the exact selected run and session", async () => {
  const p = await page();
  select(p, run({ steering: steering({ accepts: ["pause"] }) }));
  await p.el('#steering [data-steer="pause"]').onclick();
  const [, opts] = p.calls.fetch[0];
  assert.deepEqual(JSON.parse(opts.body), { kind: "pause", text: "", runId: "run-a", session: "session-a" });
});

test("cancel dialog requires a reason and remains bound to the run for which it opened", async () => {
  const p = await page();
  select(p);
  p.el("#steering [data-cancel]").onclick();
  assert.equal(p.el("#cancel").open, true);
  const form = p.el("#cancel form");
  const reason = p.el("#cancel .cancel-reason");
  reason.value = "  ";
  await form.onsubmit({ preventDefault() {} });
  assert.equal(p.calls.fetch.length, 0);
  assert.match(p.el("#cancel .cancel-fault").textContent, /reason is required/i);

  select(p, run({ id: "run-b" }));
  reason.value = "Wrong approach";
  await form.onsubmit({ preventDefault() {} });
  const [, opts] = p.calls.fetch[0];
  assert.deepEqual(JSON.parse(opts.body), { reason: "Wrong approach", runId: "run-a" });
});

test("command audit rendering updates from SSE and escapes hello, command, operator, and reason strings", async () => {
  const p = await page();
  select(p, run({ steering: steering({ accepts: ["instruction<script>"] }) }));
  p.mod.applySteeringEvent({ kind: "steering", runId: "run-a", itemId,
    steering: steering({
      accepts: ["instruction<script>"], maxSeq: 1,
      commands: [{ id: "cmd<1>", seq: 1, kind: "instruction<img>", text: "Use <b>safe</b>",
        at: "2026-09-25T12:00:00Z", by: "A & B", state: "rejected", reason: "No <script>alert(1)</script>" }],
    }) });
  const html = p.el("#steering").innerHTML;
  assert.match(html, /instruction&lt;script&gt;/);
  assert.match(html, /Use &lt;b&gt;safe&lt;\/b&gt;/);
  assert.match(html, /A &amp; B/);
  assert.match(html, /No &lt;script&gt;alert\(1\)&lt;\/script&gt;/);
  assert.doesNotMatch(html, /<script>|<img>|<b>safe/);
});

test("lower maxSeq payloads from SSE and state cannot replace newer per-run steering", async () => {
  const p = await page();
  select(p, run({ steering: steering({ accepts: ["instruction"], maxSeq: 1 }) }));
  p.mod.applySteeringEvent({ kind: "steering", runId: "run-a", itemId,
    steering: steering({ accepts: ["instruction"], maxSeq: 5,
      commands: [{ id: "new", seq: 5, kind: "instruction", text: "newest", state: "queued" }] }) });

  assert.equal(p.mod.applySteeringEvent({ kind: "steering", runId: "run-a", itemId,
    steering: steering({ accepts: ["pause"], maxSeq: 4,
      commands: [{ id: "old", seq: 4, kind: "pause", text: "stale SSE", state: "queued" }] }) }), false);
  p.mod.applySteeringState({ mode: "auto", steering: { [itemId]: steering({ accepts: ["pause"], maxSeq: 3, live: false }) } });
  const html = p.el("#steering").innerHTML;
  assert.match(html, /newest/);
  assert.match(html, /data-steer="instruction"/);
  assert.doesNotMatch(html, /stale SSE|data-steer="pause"/);
});

test("an older acknowledgement version cannot replace a newer resolution at the same maxSeq", async () => {
  const p = await page();
  select(p, run({ steering: steering({ accepts: ["instruction"], maxSeq: 1, version: 2,
    commands: [{ id: "c1", seq: 1, kind: "instruction", text: "keep this", state: "queued" }] }) }));
  p.mod.applySteeringEvent({ kind: "steering", runId: "run-a", itemId,
    steering: steering({ accepts: ["instruction"], maxSeq: 1, version: 3,
      commands: [{ id: "c1", seq: 1, kind: "instruction", text: "keep this", state: "consumed" }] }) });

  assert.equal(p.mod.applySteeringEvent({ kind: "steering", runId: "run-a", itemId,
    steering: steering({ accepts: ["instruction"], maxSeq: 1, version: 2,
      commands: [{ id: "c1", seq: 1, kind: "instruction", text: "keep this", state: "queued" }] }) }), false);
  assert.match(p.el("#steering").innerHTML, /consumed/);
  assert.doesNotMatch(p.el("#steering").innerHTML, />queued</);
});

test("a final SSE payload with omitted live clears controls from the formerly live run", async () => {
  const p = await page();
  select(p, run({ steering: steering({ accepts: ["instruction"], maxSeq: 1, live: true }) }));
  assert.match(p.el("#steering").innerHTML, /data-cancel/);
  const ended = steering({ accepts: ["instruction"], maxSeq: 1 });
  delete ended.live; // Go's json omitempty representation of live:false.
  p.mod.applySteeringEvent({ kind: "steering", runId: "run-a", itemId, steering: ended });
  assert.doesNotMatch(p.el("#steering").innerHTML, /data-steer=|data-cancel/);
});

test("steering and cancel refusals surface the server reason as text", async () => {
  const p = await page({ fetch: async url => ({
    ok: false, status: 409, text: async () => url.endsWith("/cancel") ? "replacement run is live <now>" : "session changed <now>",
  }) });
  select(p, run({ steering: steering({ accepts: ["pause"] }) }));
  await p.el('#steering [data-steer="pause"]').onclick();
  assert.equal(p.el("#steering .steer-fault").textContent, "session changed <now>");

  p.el("#steering [data-cancel]").onclick();
  p.el("#cancel .cancel-reason").value = "Stop";
  await p.el("#cancel form").onsubmit({ preventDefault() {} });
  assert.equal(p.el("#cancel .cancel-fault").textContent, "replacement run is live <now>");
  assert.equal(p.el("#cancel").open, true);
});

test("Answer remains driven only by block state beside the run controls", async () => {
  const p = await page();
  await withState(p, {
    mode: "auto", stages: [{ name: "working", next: "done" }, { name: "done", terminal: true }],
    sources: [{ name: "s1" }], items: [{ id: itemId, source: "s1", stage: "working", title: "Build it", blocked: true }],
    blocks: { [itemId]: { kind: "decision", reason: "Choose", asked: true,
      questions: [{ header: "Choice", question: "Which?", options: [] }] } },
    active: [], updatedAt: "2026-09-25T12:00:00Z",
  });
  await p.mod.inspect(itemId, "Build it", "working");
  assert.match(p.el("#stop").innerHTML, /Answer the question/);
  select(p, run({ steering: steering({ accepts: ["instruction", "pause"] }) }));
  assert.match(p.el("#stop").innerHTML, /Answer the question/);
  assert.match(p.el("#steering").innerHTML, /Add instruction/);
});
