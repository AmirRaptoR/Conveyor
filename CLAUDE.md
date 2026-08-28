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

Working: `internal/{model,config,runner,source,pipeline}` and the CLI
(`validate | list | run | tick`). `./conveyor tick -n 8 -c conveyor.example.yaml`
drains the mock pipeline in priority order and routes a blocked item out.
`providers/github/` runs against real repositories.

Not built yet, in build order (see `docs/DESIGN.md`):

1. `internal/server` — HTTP + SSE, live logs, the pipeline view
2. `web/` — served from the Go binary via `embed.FS`
3. Log retention sweep (`logs.retention`, with the pinning rule in CONTRACTS §6)
4. Persisted manual input order — v1's only human control lever, so it matters

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
  rejects a `work: true` stage with no route out — a blocked item that stays
  put gets re-run on every poll forever.
- **A source names a `provider:`, never two script paths.** Naming `list` and
  `move` separately allowed pairing GitHub's list with Azure's move, which
  nothing downstream could detect.
- **Stages declare `work:`; the script is per-source**, at
  `conveyor.d/<source>/<stage>` beside the config — never inside the repo being
  worked, where an agent told to "commit and push" would sweep it into a PR.
  Scripts still run in the source's workdir, so repo-local skills still resolve.
  `work:` stays in config because the scheduler needs it: a stage with no work
  is a queue, a work stage found mid-flight is an interrupted job to re-run.
- **An un-onboarded repo is reported, never worked around.** Per-source problems
  disable that source and leave every other repo running. There is no shared
  fallback to inherit by accident.

## The extension seam

`env:` is the only key whose values the engine never reads; every other key is
structure it needs to run the state machine. Adding a capability should almost
always be a string in `env:` and a line in a script, never a new key.

Three scopes, narrowest wins: stage env (this stage, every source), source env
(this source, every stage), and the script itself at
`conveyor.d/<source>/<stage>` — already one file per pair, so per-repo-per-stage
behaviour needs no configuration.

Deliberately not built: `args:` for scripts (env carries it, and `${FOO}` reads
better than `$3`), and per-source stage overrides (a three-way merge in Go to
express what the file layout expresses for free).

## Gotchas

- Go 1.27 lives at `~/.local/go/bin`, added to PATH in `.zshrc`.
- Paths in the config resolve against the **config file's directory**. A working
  config kept outside the checkout must therefore set `providers:` to point back
  at `providers/` here.
- The working config for this machine is `~/codes/conveyor.yaml` with its
  scripts in `~/codes/conveyor.d/<source>/`, outside the repo: machine state,
  not project source. `conveyor.example.yaml` is the template that ships.
- `-c` is the only flag normally needed; everything else is found relative to
  the config. `-providers` exists for a binary run away from its providers/.
- Provider and stage scripts resolve by name with or without an extension —
  `list.sh`, `list.py` and a compiled `list` are equivalent, because the runner
  execs the file and never consults an interpreter. Two matches is an error.
- The module path is `github.com/AmirRaptoR/Conveyor` — capital C, matching the
  remote, because the module path must match for `go get` to resolve.
- Mock state lives in `/tmp/conveyor-mock.json`; delete it to reset a demo.

## Related work living outside this repo

A bash prototype of the same idea — a global cross-repo queue in `sweep.sh`,
plus the `refine-backlog` and `deliver-issue` skills — is in the handoff bundle
at `conveyor-handoff/` (gitignored: it is migration material for one machine,
and it is not installed under `~/.claude/`). `providers/github/` was built from
it. What it still holds that conveyor does not: cross-repo ranking scores, and
`reports.py`, which reconciles against GitHub rather than trusting the model.

## Prior art to stay honest about

`issueflow.cloud` is a commercial product with a near-identical thesis. The
defensible difference is **local-first and provider-agnostic**: no control
plane, and an item is whatever a script emits. Do not write "nobody does this"
in the README.
