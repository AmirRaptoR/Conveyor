# Should Conveyor's stages use specialized agents?

This is a **desk study over retained run history and the shipped code, not an
experiment**. Every number below comes from `~/codes/data/runs` (30-day
retention, so it drifts as the pipeline keeps running) and from a handful of
bounded, single-shot probes against the installed `claude` CLI — never from a
controlled trial, which the issue that asked for this report rules out
explicitly. What it therefore could not test: whether a specialized agent
*improves outcomes*, causally, for any stage — the run store has correlation
at best, on small samples, confounded by which items happened to need
exploration in the first place; whether the account's usage window is metered
differently for a subagent's calls than the parent's (no quota endpoint
exists to ask); and anything about a model version, tool, or CLI behaviour
this report did not think to probe. Where a claim rests on the CLI's own
behaviour, the exact command is given so it can be re-run against a newer
build.

All figures are for `claude 2.1.263`, on this box, checked with `claude
--version` on 2026-09-10.

## What "agent" means here

Three things share the word in this stack, and this report is about only the
third:

1. **A Conveyor adapter** — `agents/claude/implement`, `agents/claude/refine`,
   and so on. A shell script the engine execs by name (`agent:` in config).
   Not an AI concept at all; it is this repository's plumbing.
2. **The model a stage runs** — `MODEL`, resolved to a `--model` flag on a
   single `claude -p` process. What issue #6 tuned per stage.
3. **A Claude Code specialized subagent** — a named worker with its own
   system prompt, tool allowlist, model and context window, spawned by the
   top-level `claude -p` process through the `Agent` tool while that process
   runs. This is what the rest of this report calls "a subagent" or "a
   specialized agent."

## Where a subagent may attach, and the boundaries around it

`docs/CONTRACTS.md:411-412`: **"A stage script may spawn as many subagents as
it likes internally — that is invisible to the engine."** A subagent may
attach inside a stage script's own model run, and nowhere else. It cannot be
a stage, cannot be scheduled, cannot hold a `resources:` slot of its own —
the engine has no concept of it, by design (CLAUDE.md's thesis, `CLAUDE.md:13`:
"the pipeline is deterministic and human-authored; the AI is a worker inside
one stage. Do not add features that let the model decide the flow"). A
subagent that somehow influenced which stage an item moved to next would be
exactly that: the model deciding the flow, wearing a different tool name.

Three rules follow, and they are **normative — rules the parent script and
its prompt must enforce, not guarantees the Claude Code runtime provides**:

- A subagent must not decide the item's next stage.
- A subagent must not write `$CONVEYOR_RESULT`.
- A subagent must not determine the run's exit code.

Nothing sandboxes a subagent into any of this. `docs/CONTRACTS.md`'s "What a
stage script's credentials actually reach" is explicit that **a stage script
runs as the conveyor user, with that user's whole ambient environment and
credentials** — and a subagent granted `Bash` or `Write` runs inside that
same process, with the same environment (confirmed by probe, below: a
subagent's `Bash` tool sees every variable the parent's shell exported). A
subagent with `Bash` could write `$CONVEYOR_RESULT` itself, or run `git push`
on a branch nobody asked it to touch. What stops it is the prompt the adapter
writes and the model that reads it — the same sentence `docs/CONTRACTS.md`
already uses about the `approve` gate — not a permission the engine
withholds. The parent script stays responsible for validating and writing
the result after the top-level run returns; it can never assume a subagent
respected these three rules just because it was told to.

## Measured baseline

**Window:** `startedAt >= 2026-08-28T00:00:00Z` and `< 2026-09-10T11:30:00Z`
— the full 30-day-retention window as it stood at the moment this was run
(the store's oldest record is `2026-08-28T19:51:11Z`; nothing older survives
retention). Runs still `"outcome": "running"` at the cutoff are excluded: **2**,
both `agents/claude/implement`, both live work in progress rather than a
resolved data point.

Selected by **full script path** (`meta.json.script`), not basename — two
`deploy` scripts across different sources share a name and are excluded by
construction, along with `providers/*/list.sh` and `move.sh`, which are not
this report's subject.

```bash
find ~/codes/data/runs -name meta.json -print0 | xargs -0 cat | jq -s '
  [.[] | select(.kind=="stage")
       | select(.startedAt >= "2026-08-28T00:00:00Z"
             and .startedAt <  "2026-09-10T11:30:00Z")]
  | group_by(.script)
  | map({script, total: length,
         wallclock_hours: ([.[]|select(.outcome!="running")|.durationNs]|add/1e9/3600),
         outcomes: (group_by(.outcome)|map({(.[0].outcome): length})|add)})'
```

`meta.json.durationNs` is the **whole stage process wall-clock** — preflight
`gh` calls, worktree setup, the test suite, deterministic polling exits (exit
10) — not model time. `approve` in particular runs 4,192 times in this
window and reaches a model in 34 of them; the other 4,085 are the
deterministic gate returning exit 10 with no model at all
(`agents/claude/approve`), so its 5.5 wall-clock hours describe almost
entirely `gh`/GraphQL polling, not agent work.

| Adapter | Total invocations | Reached a model | Wall-clock (h) | Outcome mix |
|---|---:|---:|---:|---|
| `agents/claude/refine` | 1,082 | 207 | 19.85 | success 135, blocked 191, failure 738, interrupted 18 |
| `agents/claude/implement` | 754 | 220 | 55.13 | success 93, blocked 618, failure 19, interrupted 22, running 2 (excluded) |
| `agents/claude/review` | 182 | 162 | 26.23 | success 90, blocked 43, failure 33, interrupted 10, timeout 6 |
| `agents/claude/approve` | 4,192 | 34 | 5.46 | success 59, blocked 39, noop 4,085, failure 8, timeout 1 |

**Outcome taxonomy**, used consistently throughout: `meta.json.outcome`
(`success`, `noop`, `blocked`, `failure`, `timeout`, `interrupted`, `running`)
is what the table above groups by; `result.json.kind` is the *reason* a
`blocked` outcome stopped (below); every rate quoted names its denominator
rather than leaving it implicit.

**"Reached a model"** is defined here as: the run's `log.txt` contains at
least one line matching `stdout +· [A-Za-z]{2,20}:` — `agents/claude/_stream`
`render_stream` prints a rendered tool call as `  · Name: arg`, prefixed by
the runner with a timestamp and `stdout`. This **undercounts** a run whose
model answered in prose without calling any tool (a `refine` that read
nothing new and just wrote a comment via `Bash` would still be counted — a
zero-tool-call clean text answer is the only case this proxy misses, and
nothing in this box's logs distinguishes that from a run that never started
the model at all). No better signal exists without parsing `--output-format
stream-json`, which is deleted at the end of every run
(`agents/claude/_stream:24`, `trap 'rm -f "$CLAUDE_SAID" "$CLAUDE_RAW"' EXIT`)
and therefore isn't retained to check against.

```bash
# reached-model count, per adapter, over the window above (paths pre-filtered
# to the four adapters via the meta.json query above, piped to a file list)
xargs grep -lE "stdout +· [A-Za-z]{2,20}:" < loglist_<adapter>.txt | wc -l
```

**Blocked-kind distribution**, same window, same four adapters,
`result.json.kind` where `.blocked == true`:

| Adapter | present | absent/empty | kind counts |
|---|---:|---:|---|
| `refine` | 193 | 889 | limit 152, decision 8, *(no `kind` field)* 33 |
| `implement` | 613 | 141 | limit 259, worktree 225, no-output 21, dependency 17, decision 9, unfinished 7, turns 1, already-delivered 1, *(no `kind` field)* 72 |
| `review` | 50 | 132 | limit 18, turns 10, worktree 4, unfinished 4, decision 2, *(no `kind` field)* 12 |
| `approve` | 52 | 4,140 | conflict 21, error 5, worktree 4, turns 4, decision 2, checks 2, no-output 1 |

`present` counts `result.json` files that exist, are non-empty and parse as
JSON — every one that existed in this window did (0 malformed across all
four adapters). `absent/empty` is every run whose result file does not exist
or is zero bytes, which is the ordinary case for a clean `success`, a
crash-before-writing `failure`, or (for `approve`) the 4,085 deterministic
noops that never touch `$CONVEYOR_RESULT` at all. `(no kind field)` is
`result.json` with `"blocked": true` but no `"kind"` — 33/72/12 cases across
refine/implement/review where the model wrote its own stop (via
`decision_brief`'s convention) without the `kind` the convention asks for;
these are real stops, just not attributable to one of the named kinds. As a
cross-check, `blocked`-outcome counts from the meta table (191/618/43/39)
land within a few runs of `present` in every adapter except `approve`, where
`present` (52) minus the 13 files with `"blocked": false` equals 39 —
matching the meta table exactly.

```bash
find ~/codes/data/runs -name result.json -print0 | xargs -0 cat 2>/dev/null | \
  jq -s '[.[] | select(type=="object" and .blocked==true)] | group_by(.kind)
  | map({kind: (.[0].kind // "(no kind field)"), n: length})'
```

(Run per-adapter, restricted to the file list already produced by the window
query — a bare `jq -s` over the whole store also picks up `deploy`, `status`
and non-Claude adapters, which is why the counts above were computed with the
adapter's `result.json` list piped through `xargs jq`, not the one-liner
above run unfiltered.)

## Subagent usage today

Two distinct measures, each with its own denominator:

| Adapter | Runs with ≥1 `Agent` call | of total invocations | of reached-model | `Agent` calls (ceiling) |
|---|---:|---:|---:|---:|
| `refine` | 16 | 1,082 | 207 | 18 |
| `implement` | 26 | 754 | 220 | 37 |
| `review` | 0 | 182 | 162 | 0 |
| `approve` | 1 | 4,192 | 34 | 1 |
| **total** | **43** | 6,210 | 623 | **56** |

The call count is a **ceiling, not an exact count**: the renderer can emit
the same tool-call event more than once (retries in the stream, or a tool use
that gets echoed at more than one point in `--output-format stream-json`'s
event sequence) — 56 calls across 43 runs, never fewer runs than calls, which
is consistent with that but does not prove it either way; nothing in the
retained logs disambiguates a genuinely repeated call from a re-rendered one.

**Counting method matters.** The tool name is matched at its rendered field
position — `stdout +· Agent:` — not as a free substring:

```bash
grep -rhoE "stdout +· [A-Za-z]{2,20}:" --include=log.txt ~/codes/data/runs | sort | uniq -c | sort -rn
# tool mix across the four adapters in the window (23,158 Bash calls down to 1 WebFetch)
```

A substring match (`grep -l "· Agent:"`) over the same file list returns
**46**, not 43 — three false positives, one of them this very run's own
log: a `Bash` command in this report's own research phase *searched for*
the string `"· Agent:"` (to build the counting-method example above), and the
naive grep matched its own invocation. This is exactly the failure the issue
that asked for this report warned about, reproduced while writing the report
that describes it.

## Does `ALLOWED_TOOLS` gate the `Agent` tool?

**No — an allow-list does not restrict it, but a deny-list does.** Settled
by a bounded local probe rather than guessed, because 16 `refine` runs
spawned a subagent in this window despite `refine`'s default
`ALLOWED_TOOLS` (`agents/claude/refine:71`, `Bash,Read,Glob,Grep`, no
`Agent`) and `~/codes/conveyor.yaml`'s `refine` params matching it:

```bash
$ claude -p 'Call the Agent tool (spawn a general-purpose subagent) with the
trivial task "reply with the single word PONG". Reply with exactly one line:
either "AGENT_TOOL_RAN: <what it returned>" or "AGENT_TOOL_DENIED: <reason>".' \
  --allowedTools "Read" --max-turns 4 --permission-mode acceptEdits
AGENT_TOOL_RAN: PONG

$ claude -p '(same prompt)' --disallowedTools "Agent,Task" --max-turns 4 \
  --permission-mode acceptEdits
No "Agent" tool exists in my available or deferred tool set — I have no tool
by that name to call.
AGENT_TOOL_DENIED: No such tool "Agent" is available to invoke.
```

`--allowedTools "Read"` (no `Agent`, no `Task`) let the subagent run anyway;
`--disallowedTools "Agent,Task"` removed it from the tool set entirely. A
third probe with `--output-format stream-json --verbose` confirms the
rendered tool name is literally `Agent` (matching what `_stream`'s counting
greps for), so this isn't a naming mismatch — `ALLOWED_TOOLS` as this
repository uses it (an allow-list passed to `--allowedTools`) simply does not
gate this tool on the installed CLI version. That is the whole explanation
for `refine`'s 16 subagent runs: nothing in `agents/claude/refine` or
`~/codes/conveyor.yaml` permits `Agent`, and nothing needs to, because the
CLI does not check.

The same probe file list also shows other tool names this report did not go
looking for — `TaskOutput`, `ToolSearch`, `Monitor`, `ScheduleWakeup`,
`ListAgents`, `SendMessage`, `TaskStop` — appearing in adapter logs whose
`ALLOWED_TOOLS` lists none of them either (95, 141, 40, 9, 4, 12, 182 calls
respectively, in the tool-mix table above). Consistent with the same gap,
not separately verified here: this report's scope is the `Agent` tool.

## Per-stage candidates

For every candidate, "the tools it would get" is argued as least privilege —
**a subagent is not sandboxed**, so a tool granted to it is a tool granted to
whatever the parent's prompt tells it to do, same as the parent itself.

### Where a definition would live

Three mechanisms exist on the installed CLI, verified rather than assumed:

- **Inline `--agents <json>`** — `claude --help`: `--agents <json>` , "JSON
  object defining custom agents." No file at all; the definition is a
  string the adapter script builds and passes on the command line, the same
  way it already builds `--allowedTools` and `--model`.
- **Project- or user-level `.claude/agents/*.md`** — not present anywhere on
  this box today (`~/.claude/agents` does not exist; no worked repository
  has a `.claude/agents` directory either), but confirmed as a real,
  version-specific mechanism by the installed CLI's own changelog rather
  than assumed from general Claude Code knowledge:
  `~/.claude/cache/changelog.md:312`, "Added `experimental.cacheTtl`... to
  agent frontmatter"; `:3924`, "Added `effort`, `maxTurns`, and
  `disallowedTools` frontmatter support for plugin-shipped agents";
  `:560`, "Fixed agents, skills, and commands whose `.md` file starts with a
  UTF-8 BOM being silently ignored" — three separate changelog entries that
  only make sense if agent definitions are `.md` files with frontmatter,
  confirming the mechanism without needing to construct one to prove it
  exists.
- **Plugin-provided agents** — same changelog line (`:3924`) as above
  explicitly distinguishes "plugin-shipped agents" from the other two,
  confirming this is a third, distinct mechanism.

Comparing the three on portability, versioning and accidental-commit risk:

`.claude/agents/*.md` at **user level** is machine state outside this
repository, the same category `~/codes/conveyor.yaml` already is
(`CLAUDE.md`, "the working config for this machine... outside the repo:
machine state, not project source") — it would not ship with the repo, and
every box running Conveyor would need the file recreated by hand, silently,
with nothing in code review ever seeing it. At **project level** it is worse
than undocumented: `agents/claude/implement` and `agents/claude/review` both
`cd "$wt"` into the *item's own worktree* — a checkout of the repository
*being worked on* — before calling `run_claude`
(`agents/claude/implement:107-108`, `wt=$(item_worktree ...); cd "$wt"`),
and Claude Code resolves project-level `.claude/agents` off the current
working directory. A definition placed there would have to be committed
into every repository Conveyor works, where an agent told to commit and
push would sweep it into the item's own pull request — the exact failure
mode `CLAUDE.md` already names for skills ("never inside the repo being
worked, where an agent told to 'commit and push' would sweep it into a
PR"). Project-level `.claude/agents` is disqualified by this architecture,
not merely discouraged.

A **plugin** keeps the definition out of the worked repository entirely and
is versioned and portable in principle, but adds an installation step of its
own (a plugin directory to ship and install on every box, alongside
`providers/` and `agents/` — machinery this report's scope does not
otherwise need) for what, per the per-stage candidates below, is one small
JSON object per adapter.

**Inline `--agents` JSON, built into the adapter script itself, is what
every candidate below proposes.** It ships in the same file, reviewed in the
same pull request and versioned by the same commit as the rest of the
adapter; it never depends on the CLI's cwd-based resolution, so it is immune
to the worktree-sweep risk by construction; and it costs nothing to install
beyond what already has to be installed for the adapter to run at all.

### `refine`

**Candidate: codebase-location questions during requirement-gathering** —
"where is session state written," "what test helpers exist" — the shape the
`/refine` skill already tells a top-level agent to answer itself, and the
shape 16 of `refine`'s 1,082 runs already delegate today, informally, with no
definition backing it. `refine`'s own `MAX_TURNS` is 40
(`agents/claude/refine:70`), the tightest budget of the four adapters — a
question answered by a narrow subagent instead of three or four `Grep`/`Read`
rounds in the parent's own turn count is turns the parent does not spend, in
the stage with the least room to spend them (though `turns` never once
appears as `refine`'s blocking kind in this window — the budget already
holds, so this is about the parent's context staying uncluttered for the
result the model actually has to write, not about avoiding a stop that
isn't happening).

- **Tools:** `Read, Glob, Grep`. No `Bash` — a location question does not
  need a shell, and `refine` never grants `Write`/`Edit` to begin with
  (`agents/claude/refine:71`); a subagent with more reach than the stage that
  spawns it would be a strange first agent to define. No `Agent` for the
  subagent itself — one level of fan-out is the job, not a tree of them.
- **Model:** `claude-haiku-4-5-20251001` — verified to resolve
  (`claude -p 'reply with OK' --model claude-haiku-4-5-20251001
  --output-format stream-json --verbose` returns `"model":
  "claude-haiku-4-5-20251001"` in its `init` event). A location lookup is
  exactly issue #6's "smaller job than producing one" case, one tier down
  from even the cheaper end of that table.
- **Displaces:** the parent's own `Grep`/`Read` rounds spent locating context
  before it can draft — no `/refine` skill instruction moves, since the skill
  already tells the top-level agent to delegate this; formalizing gives it a
  named, least-privilege definition instead of an ad-hoc, unscoped `Agent`
  call with whatever tools and model the top-level model happens to pick.
- **Where it lives:** inline, as a `--agents` JSON block built into
  `agents/claude/refine` itself — see "Where a definition would live" above.

**Measurement, correlational only, not causal:** within `refine`'s
reached-model population (n=207), the 16 subagent-using runs succeeded 11/16
(68.8%); the other 191 succeeded 124/191 (64.9%). A 4-point gap on n=16 is
well inside noise — not evidence the subagent *caused* anything, only the
number a follow-up would keep watching.

### `implement`

**Candidate:** the same exploration job, already explicit in the `/implement`
skill ("Fan out — codebase questions before you start... Use the Explore
agent, or sonnet, one per question, in parallel") and already used more than
`refine` (26/754 runs, 220 reached-model). The dominant blocked kinds here,
though, are `limit` (259) and `worktree` (225) — 78% of `implement`'s named
blocks — and neither is something a subagent touches: quota is shared with
the parent, and a worktree left dirty by a dead run is a filesystem fact no
amount of specialization changes. The turn-budget argument that helps
`refine` barely applies either — `turns` blocks `implement` exactly once in
the whole window, against a 200-turn budget (`agents/claude/implement:184`).

- **Tools:** `Read, Glob, Grep` — same reasoning as `refine`; `implement`'s
  own `Bash, Read, Edit, Write, Glob, Grep, Agent, Skill`
  (`agents/claude/implement:185`) is the *orchestrator's* allowance for the
  TDD loop itself, which `/implement` already reserves for the parent
  ("Keep yourself — the whole red-green-commit cycle... parallel agents
  produce conflicting edits").
- **Model:** `claude-haiku-4-5-20251001`, same reasoning.
- **Displaces:** nothing new in the prompt — the skill already asks for this.
  What changes is giving the existing, unscoped `Agent` call a named,
  least-privilege definition instead of leaving its tools and model to
  whatever the top-level model decides at the call site.
- **Where it lives:** inline `--agents` JSON in `agents/claude/implement`.

**Measurement:** within `implement`'s reached-model population (n=220), the
26 subagent-using runs succeeded 15/26 (57.7%); the other 194 succeeded
79/194 (40.7%) — a 17-point gap, on a sample too small (n=26) to call causal
and confounded by which items happened to need exploration, but larger than
`refine`'s and worth tracking as the same before/after number a follow-up
would use.

### `review`

**Candidate: independent triage of Codex findings**, before any fix is
written. `agents/claude/review`'s own header comment says the skill it
invokes "own[s] the loop — ask Codex, judge findings, fix, re-review, up to
three rounds, then merge when everything is green" — a comment that predates
`approve` taking over the merge (`CLAUDE.md`, "Merging is a gate, not a
judgement": "`review` ends when a pull request exists; it no longer merges");
the ask/judge/fix/re-review loop itself is current, only the merge at the
end has moved. Judging
each finding (is it real, does it matter) is read-only and independent
finding-to-finding — no ordering between findings, no shared state a parallel
judgement would corrupt. **Zero of `review`'s 182 runs in this window used a
subagent**, despite `Agent` already
being in its default `ALLOWED_TOOLS`
(`agents/claude/review:147`, `Bash,Read,Edit,Write,Glob,Grep,Agent,Skill`) —
the mechanism is switched on and unused, which is itself worth noting rather
than assuming it means nothing to delegate: `turns` is `review`'s
second-largest named block (10/50, 20%) after `limit` (18/50, 36%), and
serial finding-by-finding judging across up to three rounds is exactly the
kind of turn-heavy work a parallel triage pass could shrink.

Unlike `refine`/`implement`'s exploration case, this is genuinely new
behaviour, not a formalization of something already happening, and it sits
closer to the part of the job the `/implement` skill explicitly warns off
parallelizing ("the whole red-green-commit cycle... one agent, one branch")
— judging is read-only and independent, but the *fix* that follows a
judgement is not, and has to stay serial in the parent to avoid the same
conflicting-edit risk. Given that, and given no organic usage exists to
measure against yet, this is a **needs a trial**, not an adopt.

- **Tools (for the triage subagent only):** `Read, Glob, Grep` — judging
  whether a finding is real needs to read the diff and the surrounding code,
  never to change it.
- **Model:** `claude-sonnet-5` — verified to resolve
  (`claude -p 'reply with OK' --model claude-sonnet-5 --output-format
  stream-json --verbose` returns `"model":"claude-sonnet-5"` in its `init`
  event). Judging a finding is issue #6's own "smaller job than producing
  one" reasoning for why `review`'s orchestrator itself runs sonnet rather
  than opus; the triage subagent inherits the same argument one level down.
  (`claude-haiku-4-5-20251001` is plausible too; not proposed here because
  which tier a judgement call needs is exactly what the trial should settle,
  not a fact this report can probe.)
- **Displaces:** the serial "ask Codex → judge finding 1 → judge finding 2 →
  ..." portion of the loop the skill currently runs one finding at a time in
  the parent's own turn budget.
- **Where it lives:** inline `--agents` JSON in `agents/claude/review`.

### `approve`

**Candidate: none.** 4,192 invocations, 34 (0.8%) reach a model at all —
`agents/claude/approve`'s gate is fourteen deterministic rules returning
exit 10 on every quiet poll (`agents/claude/approve`, header comment); a
model runs only to resolve an unresolved review thread or a merge conflict.
Both of those need the *whole* context — the full thread, the full diff —
in one place to produce one coherent resolution; neither decomposes into
independent sub-questions a subagent could take pieces of. And `approve` is
the stage closest to an actual merge:
`docs/CONTRACTS.md`'s "What a stage script's credentials actually reach"
says plainly that **"an agent running with the same credentials `approve`
itself uses could merge directly, or push to any other branch it can reach,
without going through the gate at all. What stops that from happening is the
prompt an adapter writes and the model that reads it, not a permission the
engine withholds."** A subagent here is one more actor that prompt has to
keep honest, for a job too small (under 3 model-reaching runs a day across
the whole window) to be worth the design and monitoring cost. One run in
this window already spawned a subagent inside `approve`'s conflict-resolution
path — an existing data point worth reading, not a pattern worth expanding.

## Costs and risks

Each resolved to **real**, **not applicable**, or **undetermined** — never
left as a maybe.

**Quota multiplication — real.** `limit` is the largest named block in every
adapter that reaches a model at meaningful volume (`refine` 152/193 = 79%,
`implement` 259/613 = 42%, `review` 18/50 = 36%) and 429 of 891 blocked
outcomes across the four adapters in this window are `limit`. A subagent's
`Agent` tool call is a real, metered model call under the same authenticated
session as the parent — it spends the same account's usage window, not a
separate one. Specialization only nets a saving if the subagent's job is
genuinely narrower than what the parent would otherwise spend re-deriving —
the exploration candidates above argue exactly that; a subagent that instead
duplicates context the parent already holds is a net quota loss, not a
saving.

**Cold-start re-derivation — real.** A subagent starts with none of the
parent's accumulated context — not the item, not the worktree state, not
what the parent has already ruled out — and has to re-derive it from
whatever the task string hands it (`docs/CONTRACTS.md:411-412`'s "own
context window" is the mechanism; this repo's own `/implement` skill states
the cost plainly: "A subagent starts cold and re-derives everything, so it
only pays when the work is bigger than the briefing"). Every candidate above
is sized to be bigger than its briefing — a location question, or one
finding — deliberately.

**`MAX_TURNS` reaching a subagent — real, and it does not.**
`agents/claude/_stream:251`'s `run_claude` passes `--max-turns
"${MAX_TURNS:-40}"` to the **top-level** `claude -p` process only. Per the
installed CLI's own changelog (`~/.claude/cache/changelog.md:3924`,
"Added `effort`, `maxTurns`, and `disallowedTools` frontmatter support for
plugin-shipped agents"), a subagent's turn budget is a property of its own
agent definition, set independently of the parent's `--max-turns` flag — the
adapter's `MAX_TURNS` env var has no path to a subagent at all. A subagent
defined with no `maxTurns` of its own is unbounded by anything this
repository sets.

**`CONVEYOR_DEADLINE` reaching a subagent — split.** The raw environment
variable does reach a subagent's tool calls, confirmed by probe: a custom
env var set in the parent shell was visible inside a subagent's own `Bash`
call (`echo $CONVEYOR_DEADLINE_PROBE` printed the parent's value, relayed
back through the subagent's result). But the *instruction* — `deadline_brief`
in `agents/_deadline`, what actually tells an agent what the deadline means
and what to do as it nears — is appended once to the top-level prompt
(`agents/claude/implement:177-178`, `full="${full}\n$(deadline_brief)"`) and
never passed into a subagent's task string by anything in this repository
today. A subagent has the raw timestamp available to it if its own prompt
happens to read the environment, but none of the guidance — "commit and push
before the wall," "stopping unfinished is supported" — unless the parent's
prompt to the `Agent` tool composes it in explicitly, which no candidate
above currently proposes doing (none of them run long enough, or write
anything of their own, to need it — a future subagent that does would have
to carry `deadline_brief`'s text itself).

**Log volume — real, measured.** The 43 runs that used a subagent in this
window average 34.8 KB of `log.txt`; a 200-run sample of the ones that did
not average 2.2 KB — roughly 16×.

```bash
awk '{ "stat -c%s "$0 | getline s; sum+=s; n++ } END{print sum/n}' agent_logs.txt        # 34812
awk '{ "stat -c%s "$0 | getline s; sum+=s; n++ } END{print sum/n}' sample_non_agent.txt  # 2198.72
```

**The `agents/_blocked` stop convention is only reachable from the top-level
run — real.** `blocked()` and `asks()` (`agents/_blocked`) are bash functions
sourced into the adapter script that wraps `run_claude`; `hit_limit` and
`hit_max_turns` (`agents/claude/_stream`) grep `$CLAUDE_SAID`, which is the
**top-level** process's own rendered output, not a subagent's. A subagent
that itself hits a wall — its own `maxTurns`, a tool denial, anything —
has no way to signal that upward except through whatever text it returns as
its result, which the parent then has to notice and decide what to do about;
none of it reaches `blocked`/`asks` automatically the way the top-level
run's own limit and turn-budget stops do.

## Verdict

| Stage | Verdict | Next step |
|---|---|---|
| `refine` | Adopt | [#85](https://github.com/AmirRaptoR/Conveyor/issues/85) — formalize the existing informal exploration fan-out as a named, least-privilege `--agents` definition; track blocked/success rate for reached-model `refine` runs (baseline: 207 reached-model, 152 `limit`, 68.8% vs 64.9% success agent-using vs not) as the confirm/refute measure. |
| `implement` | Adopt | [#85](https://github.com/AmirRaptoR/Conveyor/issues/85) — same formalization; track the same numbers (baseline: 220 reached-model, 259 `limit` + 225 `worktree` = 78% of named blocks, 57.7% vs 40.7% success agent-using vs not). Expect no material change — the dominant blocks are untouched by this — the value is a named definition instead of an unscoped `Agent` call. |
| `review` | Needs a trial | [#86](https://github.com/AmirRaptoR/Conveyor/issues/86) — design and land a bounded triage-subagent for judging Codex findings (not a duplicate-run controlled trial, which is out of this report's scope); track `turns` share of `review`'s named blocks (baseline: 10/50, 20%) and wall-clock per review run (baseline: 26.23h / 182 runs) before and after. |
| `approve` | Reject | Do nothing. The two jobs that reach a model each need whole-context judgement, volume is under 3 model-reaching runs a day, and it is the stage closest to an unsupervised merge. |

Attribution caveat for every "adopt"/"needs a trial" measure above: none of
these numbers isolate one stage run's contribution to a merged item cleanly
— retries, resumes, manual merges, and 30-day retention all blur "before" and
"after" into overlapping windows rather than clean cohorts. What each measure
can see is a rate over a stated window, re-computable with the commands in
this report; what it cannot see is causation, or an item whose history fell
out of retention before the comparison was made.

Both follow-ups were filed as real issues (`gh issue create`, referencing
#69), not inline proposals — issue creation was available at the time of
writing. Each carries the full scope, tool/model choice and measurement plan
already argued above, rather than repeating it here. Neither adds an
adapter, prompt, config or agent-definition change in *this* pull request —
`docs/specialized-agents.md` (plus the one-line link from `docs/DESIGN.md`)
is the entire deliverable of the issue this document closes; the follow-ups
are where any of it gets built.
