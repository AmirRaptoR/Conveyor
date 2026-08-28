# Design

## Thesis

Most tools in this space give an AI agent a goal and hope. **agent-team inverts
that: the pipeline is deterministic and human-authored; the AI is a worker inside
one stage.** A stage either exits 0 or it does not. The engine moves the card and
writes the provider state — never the model.

Non-goal, stated up front so scope does not creep: this is **not** a general
workflow engine. The unit of work is always an item from a source, and every
transition is a stage change. When someone asks for cron triggers or matrix
builds, that is CI, and CI already exists.

## The five requirements

| # | Requirement | How |
| --- | --- | --- |
| 1 | Top-down: board first, sources are scripts returning one format | `docs/CONTRACTS.md` §1 defines the item. GitHub and Azure both emit it. The engine never learns either. |
| 2 | Stages from config, minimum two | `config/*.yaml` `stages:` — order is the board, left to right. Validated at load. |
| 3 | Items loaded from sources | `list` scripts, one per source. Only configured sources are scanned; no directory auto-discovery. |
| 4 | Timer picks up an item | `poll:` re-lists; the scheduler picks the highest-priority item whose stage has an `onEnter` and whose source lock is free. |
| 5 | Moving a column runs a script, logs visible | Stage `onEnter`. stdout/stderr stream line-by-line to the UI over SSE; exit code decides the next stage. |

Mocked from day one: `sources/mock/` and `stages/mock/` exercise every path —
success, blocked, provider write-back — with no GitHub and no AI. The engine is
developed against them.

## Why logs and data are separate channels

An AI stage script writes megabytes of prose to stdout. Parsing structured data
out of that stream is the single most likely way this system breaks. So:
stdout/stderr are **logs, never parsed**; structured output goes to the file at
`$AGENT_TEAM_RESULT`. One rule, no exceptions, applies to every script kind.

## Why the engine owns provider writes

Stage scripts do work; they never set labels. The engine calls the source's
`move` script — **before** the stage script runs, not after. If the process dies
mid-stage the provider already says `in-progress`, so the item is not handed out
twice on the next poll. This is the same guarantee as claiming an issue before
implementing it, generalised.

## Concurrency

`perSource: 1` is a constraint, not a default: a source maps to a git worktree,
and two agents in one checkout corrupt each other. `global` starts at 1 and is
what you raise to run several sources at once. A stage script may spawn as many
subagents as it likes — invisible to the engine.

## Go package layout

Single static binary with the UI embedded — the distribution story is `curl | sh`,
not "install node and python".

```
cmd/agent-team/         main, flags, serve|run|validate
internal/config/        yaml load, defaults, validation (stage graph, cycles)
internal/model/         Item, Stage, Source, Run — the shapes in CONTRACTS.md
internal/source/        list + move invocation, item validation and dedupe
internal/pipeline/      state machine: which item moves where, and why
internal/runner/        os/exec + context timeout + line-streaming log capture
internal/store/         items, run history, manual order (SQLite or JSON files)
internal/server/        HTTP API + SSE hub; embed.FS serves web/
web/                    the existing dashboard, plus the board
sources/  stages/       shipped example + mock scripts
```

The existing `pick_issue.py` and `reports.py` are **not** rewritten. The runner
executes arbitrary scripts, so they keep working as steps. Port them later only
if there is a reason.

## Build order

1. `config` + `model` — load and validate the example yaml. No behaviour.
2. `runner` — exec one script, stream logs, honour timeout, return exit code.
3. `source` — list and move against the mocks.
4. `pipeline` — the transition rules in CONTRACTS.md §4, plus locks.
5. `server` — SSE for live logs; board reads real state.
6. Only then a real GitHub source.

Steps 1–4 are the system. If they are right, the rest is presentation.
