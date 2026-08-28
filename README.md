# Conveyor

**Deterministic pipelines for AI-driven issue work. You author the stages; the
agent is a worker inside one.**

Most tools in this space hand an AI agent a goal and hope. Conveyor inverts that:
the pipeline is yours, written in YAML, and every transition is a script that
either exits 0 or does not. The engine moves the item and writes the provider
state — never the model.

Local-first and provider-agnostic. No control plane, no account, nothing about
your repositories leaves the machine. A work item is whatever a script emits, so
GitHub issues, Azure PBIs, Jira tickets or a text file are all just different
`list` scripts.

> **Status: early.** The engine runs end to end against mocks — config, runner,
> sources and the transition table are working and tested. There is no web UI and
> no GitHub source yet. See [docs/DESIGN.md](docs/DESIGN.md) for the build order.

## How it works

```
list script  ──▶  items  ──▶  pick  ──▶  move(provider)  ──▶  stage script
                                              ▲                     │
                                              └──── exit code ──────┘
                                                 0 / 10 / 20 / other
```

Stages are columns you define. A stage with a script does work; a stage without
one is a queue where items rest. Exit codes are the whole control flow:

| Code | Meaning | Engine does |
| --- | --- | --- |
| `0` | Success | Advance to `onSuccess` |
| `10` | No-op | Leave the item where it is |
| `20` | Blocked — needs a human | Route to `onBlocked` |
| anything else | Failure | Route to `onFailure`, count an attempt |

Because a stage is just an executable, a stage can be a headless AI agent, a test
suite, a deploy, or a shell one-liner. Claude Code, Codex and opencode are all
the same thing to Conveyor: a command.

## Try it

Nothing to install but Go — the mocks need no GitHub and no AI.

```bash
go build -o conveyor ./cmd/conveyor
cp conveyor.example.yaml conveyor.yaml

./conveyor validate         # check the config and print the stage graph
./conveyor list             # run every source's list script
./conveyor tick -n 8 -v     # drain the pipeline, streaming logs
```

You should see items advance in priority order, one blocked item routed out of
the pipeline, and then `nothing to do`.

## Configuration

```yaml
version: 1
concurrency:
  perSource: 1      # one item in flight per source — a source maps to a
  global: 1         # worktree, and two agents in one checkout corrupt it
stages:
  - name: backlog                        # no script: a queue
  - name: refining
    onEnter: ./stages/mock/refine.sh
    onSuccess: ready
    onFailure: backlog
  - name: done
    terminal: true
sources:
  - name: mock
    workdir: .
    list: ./sources/mock/list.sh
    move: ./sources/mock/move.sh
```

Only sources listed in the config are ever touched. There is no directory
auto-discovery: enrolling a repository is adding an entry.

## Writing a source or a stage

One contract for every script, in full in
[docs/CONTRACTS.md](docs/CONTRACTS.md):

- **stdin** — a JSON object with the item and the transition
- **stdout / stderr** — logs, streamed live, **never parsed**
- **`$CONVEYOR_RESULT`** — a file to write structured output to
- **exit code** — the transition

Logs and data are separate channels on purpose. An AI stage script writes
megabytes of prose to stdout; treating that as a data channel is how this kind of
system breaks.

## Design notes

- **The engine writes provider state before running a stage**, not after. If the
  process dies mid-stage the provider already reflects reality, so the item is
  not handed out twice on the next poll.
- **A timeout kills the process group**, escalating TERM to KILL. An AI stage
  script spawns children; killing only the parent leaves them holding the lock.
- **Every run is a self-contained directory** (`meta.json`, `stdin.json`,
  `log.txt`, `result.json`), so a failure can be handed to another person — or
  another agent — with everything needed to understand it.

## Non-goals

Conveyor is **not** a general workflow engine. The unit of work is always an item
from a source, and every transition is a stage change. If you want cron triggers
and matrix builds, you want CI, and CI already exists.

## Licence

MIT.
