# Conveyor — working notes for Claude

Read `docs/CONTRACTS.md` first: it is the whole abstraction, and most questions
about "how should X work" are already answered there.

## What this is

A pipeline engine. Stages come from `conveyor.yaml`; every transition runs a
script; the script's exit code is the entire control flow. The engine knows
nothing about GitHub, git or AI — those are all scripts behind one contract.

**The thesis, which shapes every decision:** the pipeline is deterministic and
human-authored; the AI is a worker inside one stage. Do not add features that
let the model decide the flow.

## Status

Working and tested against mocks: `internal/{model,config,runner,source,pipeline}`
and the CLI (`validate | list | run | tick`). `./conveyor tick -n 8` drains the
mock pipeline in priority order and routes a blocked item out.

Not built yet, in build order (see `docs/DESIGN.md`):

1. `internal/server` — HTTP + SSE, live logs, the pipeline view
2. `web/` — served from the Go binary via `embed.FS`
3. `sources/github/` — a real source; the mocks only prove the contract
4. Log retention sweep (`logs.retention`, with the pinning rule in CONTRACTS §6)
5. Persisted manual input order — v1's only human control lever, so it matters

## Invariants — do not break these

- **stdout/stderr are logs and are never parsed.** Structured data comes back
  only via `$CONVEYOR_RESULT`. An AI stage script emits megabytes of prose.
- **The engine writes provider state before running a stage**, never after, and
  stage scripts never call `move` themselves. A crash mid-stage then leaves a
  truthful record and the item is not handed out twice.
- **A timeout kills the process group**, TERM then KILL. Killing only the parent
  leaves an agent's children holding the source's lock.
- **`perSource: 1` is a constraint, not a default.** A source maps to a git
  worktree; two agents in one checkout corrupt each other.
- **Every item must be able to leave a scripted stage.** Config validation
  rejects a stage with `onEnter` but no route out — a blocked item that stays
  put gets re-run on every poll forever.

## Gotchas

- Go lives at `/usr/local/go/bin` and is not on the default PATH.
- Script paths in the config resolve against the **config file's directory**,
  which is why `conveyor.yaml` sits at the repo root.
- `conveyor.yaml` is gitignored; `conveyor.example.yaml` is the template.
- The module path is `github.com/AmirRaptoR/Conveyor` — capital C, matching the
  remote, because the module path must match for `go get` to resolve.
- Mock state lives in `/tmp/conveyor-mock.json`; delete it to reset a demo.

## Related work living outside this repo

A bash prototype of the same idea is at `~/.claude/issue-loop/` (global
cross-repo queue, `sweep.sh`) plus the `refine-backlog` and `deliver-issue`
skills, and a local patch to `ship-issues/scripts/pick_issue.py` that adds
`status:*` labels to its blocking patterns. That prototype is what
`sources/github/` should be built from — it already solved the ranking, the
worktree guards and the label lifecycle.

## Prior art to stay honest about

`issueflow.cloud` is a commercial product with a near-identical thesis. The
defensible difference is **local-first and provider-agnostic**: no control
plane, and an item is whatever a script emits. Do not write "nobody does this"
in the README.
