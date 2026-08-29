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
drains the mock pipeline in priority order and marks a blocked item in place.
`providers/github/` runs against real repositories.

`conveyor serve` runs the pipeline and renders it: the board, live logs over
SSE, run history, drag-to-reorder, and drag out of the backlog to start an
item now (`POST /api/items/{id}/start`). It advances items on its own — `-watch`
is what makes it observe without touching anything. Discovery, scheduling and
the tick button are three goroutines, deliberately: a 90-minute stage must not
be able to stop every source being listed.

`internal/store` holds the persisted manual input order — v1's only human
control lever — and the data directory's owner lock.

Not built yet: the log retention sweep (`logs.retention`, with the pinning rule
in CONTRACTS §6). See `docs/DESIGN.md`.

## Invariants — do not break these

- **stdout/stderr are logs and are never parsed.** Structured data comes back
  only via `$CONVEYOR_RESULT`. An AI stage script emits megabytes of prose.
- **The engine writes provider state before running a stage**, never after, and
  stage scripts never call `move` themselves. A crash mid-stage then leaves a
  truthful record and the item is not handed out twice.
- **A timeout kills the process group**, TERM then KILL. Killing only the parent
  leaves an agent's children holding the source's lock.
- **`perSource: 1` is a constraint, not a default.** A source maps to a git
  worktree; two agents in one checkout corrupt each other. Parallelism comes
  from `perStage` and `global`: different sources work different stages at once,
  which is a line filling up, not two agents in one repo.
- **The scheduler claims a slot before it launches.** Checking whether a slot is
  free and then starting a goroutine leaves a gap in which the next pass decides
  the same thing again. `Engine.Advance` does not lock; its caller must.
- **Blocked is a mark on the item, not a stage it moves to.** An item that
  stopped stays in the stage it stopped in and wears a mark the provider writes
  in its own vocabulary (a `blocked` label on GitHub). The scheduler never picks
  a marked item, and *that* is what stops the stage being re-run on every poll —
  it used to be a terminal `blocked` column, which cost the context of where the
  work stopped. `onSuccess:` is the only route; `onFailure:` and `onBlocked:` are
  rejected by name. `maxAttempts:` unset means one, so the first failure marks.
- **A drag decides when, never where.** The start endpoint runs only the
  transition `pipeline.Target` already chose, and the board offers the gesture
  only out of the first stage. Dropping a card onto a deploy stage would be a
  deploy nobody reviewed; the pipeline is authored ahead of time, not steered
  card by card. A busy slot is a refusal naming what holds it, not a queue.
- **The board never sorts.** `/api/state` hands items over in `pipeline.Order`,
  the same ladder `Pick` maximises over, and the page draws them in the order
  they arrive. Sorting in the page is a second copy of the scheduling rules,
  and the two drift: a card at the top of a column that the engine reaches
  fourth is a board that lies about what happens next.
- **A source names a `provider:`, never two script paths.** Naming `list` and
  `move` separately allowed pairing GitHub's list with Azure's move, which
  nothing downstream could detect.
- **A stage says only `script:`, a name.** Not a path, not a command, and
  nothing about how to run it. Each source declares what that name is, in its
  own `scripts:` block — never inside the repo being worked, where an agent told
  to "commit and push" would sweep it into a PR. Scripts still run in the
  source's workdir, so repo-local skills resolve. A named script in config is
  what the scheduler needs: a stage with none is a queue, a running stage found
  mid-flight is an interrupted job to re-run.
- **How the work happens is never in the config.** Which agent, which model,
  which tools, all of it lives in the source's script, because it differs per
  repository. Resist every request to add a key for it.
- **An un-onboarded repo is reported, never worked around.** Per-source problems
  disable that source and leave every other repo running. There is no shared
  fallback to inherit by accident.

## Running agents

`agents/<name>/<script>` holds reusable adapters, resolved by `agent:` exactly
as `provider:` resolves under `providers/`. The prompt, tools, model and turn
limit are that script's `params:`. The adapter prepends the item to the prompt
and appends the blocked convention, so prompts stay portable and every agent
signals "needs a human" identically: exit 20, with `{"blocked": true, "reason":
"…"}` in `$CONVEYOR_RESULT`. The engine hands that reason to `move`, and the
GitHub provider comments it on the issue beside the label.

## The extension seam

The extension is the scripts, never the engine. A stage names one; the source
provides it; how it works is that file's business. `run:` takes an inline body
for a stage that is identical everywhere, and must carry a shebang.

Configuration the engine passes but never reads, narrowing: `provider.params:`
(list and move only — labels are GitHub's vocabulary, not the pipeline's),
`env:` (every script — what the source is), and `scripts.*.params:` (one script,
winning on conflict). Script params are per-script because two stages both want
a PROMPT. Everything else in the schema is structure.

Deliberately not built: stage-level `env:` (settings that differ per source
belong to that source), and `args:` for scripts (env carries it and reads
better).

## Gotchas

- Go 1.27 lives at `~/.local/go/bin`, added to PATH in `.zshrc`.
- Paths in the config resolve against the **config file's directory**. A working
  config kept outside the checkout must therefore set `providers:` to point back
  at `providers/` here.
- The working config for this machine is `~/codes/conveyor.yaml`, outside the
  repo: machine state, not project source. It sets `providers:` and `agents:`
  back at the checkout. `conveyor.example.yaml` is the template that ships.
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
