# Contracts

Everything the engine knows is in this file. It has no concept of GitHub, git,
Claude, or issues — only **sources**, **items**, **stages** and **scripts**.
Adding Azure DevOps, Jira or a text file means writing scripts, never touching
the engine.

## 1. The item

The unit of work. A source script emits these; the engine stores and displays
them. Provider-neutral by construction: a GitHub issue and an Azure PBI both
arrive in this shape.

```jsonc
{
  "id": "midgame:47",           // REQUIRED. Globally unique, stable across polls.
                                // Convention: "<source>:<ref>". This is the join
                                // key for everything — never let it change for
                                // the same underlying work item.
  "ref": "47",                  // REQUIRED. Provider-native id, passed back to
                                // scripts verbatim. The engine never parses it.
  "source": "midgame",          // REQUIRED. Which source produced this.
  "stage": "ready",             // REQUIRED. Must be one of the configured stage
                                // names. The SOURCE decides this — it is the one
                                // that knows how provider state maps to a stage.
  "title": "Verify the web build on mobile browsers",   // REQUIRED

  "description": "Full body text, markdown ok",
  "url": "https://github.com/RaptoR-Soft/midgame/issues/47",
  "labels": ["bug"],            // display only; the engine attaches no meaning
  "priority": 2,                // 0 = most urgent. null = unranked.
  "assignee": null,
  "createdAt": "2026-08-20T10:00:00Z",
  "updatedAt": "2026-08-27T11:00:00Z",
  "raw": {}                     // provider passthrough. Opaque to the engine,
                                // handed back to scripts untouched.
}
```

Rules the engine enforces:

- Unknown `stage` → the item is rejected and logged, not silently dropped.
- Duplicate `id` within one poll → first wins, the collision is logged.
- An item that disappears from a source is marked gone, not deleted, so its run
  history survives.

## 2. The script contract

One contract for every script the engine runs. No exceptions — an extension
author learns it once.

**Input** arrives two ways:

| | |
| --- | --- |
| `stdin` | A JSON object: `{"item": {...}, "stage": "in-progress", "from": "ready", "config": {...}}`. For `list` scripts there is no item: `{"source": "midgame", "stages": ["backlog","ready",…], "config": {...}}` |
| env | `CONVEYOR_RESULT` (path to write structured output), `CONVEYOR_WORKDIR`, `CONVEYOR_SOURCE`, `CONVEYOR_STAGE`, `CONVEYOR_ITEM_ID`, `CONVEYOR_ITEM_REF`, plus everything in the source's `env:` block |

**Output** is split deliberately:

| Channel | Carries |
| --- | --- |
| `stdout` + `stderr` | **Logs only.** Streamed live to the UI, line by line, interleaved in order. Never parsed. |
| `$CONVEYOR_RESULT` | **Structured data only.** A JSON file the script writes. Absent means "no data". |

Logs and data are separated because an AI agent writes megabytes of prose to
stdout. Parsing data out of that is how this kind of system breaks. If a script
writes nothing to the result file, it simply produced no data.

**Exit codes** are the whole control flow:

| Code | Meaning | Engine does |
| --- | --- | --- |
| `0` | Success | Advance to `onSuccess` (default: next stage) |
| `20` | Blocked — needs a human | Move to `onBlocked` (default: `blocked` stage), no retry |
| `10` | No-op — nothing to do | Leave the item where it is, not an error |
| any other | Failure | Move to `onFailure` (default: stay), count an attempt |
| timeout | Killed after `timeout:` | Treated as failure, logged as `timeout` |

## 3. The three script kinds

**`list`** — read items from a provider. Writes a JSON array of items to
`$CONVEYOR_RESULT`. Exit 0 with `[]` means an empty backlog, which is normal.

**`move`** — write a stage change back to the provider (set a label, move a card,
update a field). Called by the **engine**, never by a stage script: the engine
owns provider state so a crashed stage script cannot leave it inconsistent.
Receives `{"item":…, "stage": "<target>", "from": "<current>"}`.

**stage script** (`onEnter`) — do the actual work of a stage. Run an AI agent, run
tests, deploy, cut a release. This is the extension point; everything else is
plumbing.

## 4. Transition order

Moving an item from stage A to stage B is always, in this order:

```
1. acquire the source's lock        (one item in flight per source)
2. move(item, B)                    provider now says B — before any work starts,
                                    so a crash leaves a truthful record
3. run B.onEnter                    logs stream to the UI
4. on exit 0   → move(item, next)   and release
   on exit 20  → move(item, blocked)
   on failure  → move(item, onFailure) or leave, count attempt
5. release the lock                 always, including on crash and timeout
```

Step 2 before step 3 is deliberate: it is the same guarantee as marking an issue
`in-progress` before implementing it. If the process dies mid-stage, the provider
already reflects reality and the item is not handed out twice.

## 5. Concurrency

```yaml
concurrency:
  perSource: 1      # one item in flight per source. Not configurable above 1 in
                    # v1: a source maps to a git worktree, and two agents in one
                    # checkout corrupt each other.
  global: 1         # v1 ships 1. Raising it runs N sources in parallel.
```

`perSource: 1` is a real constraint, not a default. A stage script may spawn as
many subagents as it likes internally — that is invisible to the engine.

## 6. Runs and logs

Every script invocation is a **run**, and every run is a self-contained directory.
Nothing about a run lives only in memory or only in a database row.

```
data/runs/<yyyy-mm-dd>/<run-id>/
  meta.json      item snapshot, source, from -> to, script path, exit code,
                 started/finished, duration, the env the script was given
  stdin.json     exactly what was piped in
  log.txt        stdout and stderr interleaved in real order, each line stamped
  result.json    whatever the script wrote to $CONVEYOR_RESULT (may be absent)
```

Self-contained is the point: a failed run can be `tar`'d and handed to someone
else — or to another agent — with everything needed to understand it and nothing
else needed from the machine it ran on.

Logs go to disk, not into the database. An AI stage script produces megabytes;
that is a file, not a row. The store indexes run metadata and points at the
directory.

### Retention

```yaml
logs:
  retention: 30d       # runs older than this are deleted
  sweepAt: 04:00       # daily; also runs once on startup
```

One exception, and it matters: **a run is pinned if it is the most recent failed
or blocked run of an item that is still in a failed or blocked state.** Retention
must never delete the evidence for the thing currently asking for attention.
Pinned runs are reported in the sweep log so they cannot pile up unnoticed.

### Failure is a first-class state

An item whose last run exited non-zero is **needs-attention**, shown red, and
surfaced above everything else in the UI. That state carries the run id, so one
click reaches the log that produced it. In v1 the answer to "what needs me?" is
the primary question the UI answers — more important than showing progress.

`20` (blocked) and other non-zero exits are both red, but they mean different
things and the UI says which: blocked is *"a human must decide"*, failure is
*"this broke, and it may just need retrying"*.
