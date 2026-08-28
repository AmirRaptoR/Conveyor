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

# -c keeps your own conveyor.yaml, if you have one, out of the way
./conveyor validate -c conveyor.example.yaml   # check config, print the graph
./conveyor list     -c conveyor.example.yaml   # run every source's list script
./conveyor tick     -c conveyor.example.yaml -n 8 -v
```

`conveyor.yaml` is the working config and is deliberately untracked;
`conveyor.example.yaml` is the template that ships.

A real working config is usually kept outside the checkout entirely — it is
machine state, not project source. It then has to say where the adapters are:

```yaml
providers: ~/codes/Conveyor/providers
```

Without the key, `providers/` is looked for beside the config file.

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
    script: refine    # every source must provide a script by this name
    onSuccess: ready
    onFailure: backlog
  - name: done
    terminal: true
sources:
  - name: mock
    provider: mock    # a folder under providers/
    workdir: .
```

Only sources listed in the config are ever touched. There is no directory
auto-discovery: enrolling a repository is adding an entry.

`provider:` is the one thing a source says about *how* to talk to its backend.
It cannot name `list` and `move` separately, deliberately — that would let one
source pair GitHub's list with Azure's move, a mismatch nothing downstream could
detect, because by then they are just two executable paths.

## Onboarding a source

`stages:` is the state machine: which columns exist and where an item goes next.
The only thing a stage says about execution is `script:` — a **name**, not a
path and not a command. A stage with no `script:` runs nothing and is a queue.

Every source must provide that name in `conveyor.d/<source>/`:

```
~/codes/
  conveyor.yaml
  conveyor.d/
    midgame/
      refine        claude -p, this repo's own skills and tools
      implement
    caravan-v2/
      refine        codex exec with a long prompt
```

Same pipeline, different work. How a stage is run — which agent, which model,
which tools — is entirely in the script, because it differs per repository and
the engine must never know. The stage and the script need not share a name, and
two stages may use the same one.

Scripts live beside the config rather than inside the repo being worked, so
nothing is written into somebody's project and an agent told to "commit and
push" cannot sweep the pipeline into its own pull request. They still *run* in
the source's workdir, so `claude` there resolves that repo's `.claude/skills/`.

**A source missing a named script is reported, not tolerated:**

```
    source "caravan-v2" cannot run:
      - stage "in-progress": no implement script in ~/codes/conveyor.d/caravan-v2

3 of 4 source(s) unusable; the other 1 will still be worked
```

It is *that source's* error: conveyor reports it, skips it, and keeps working
every healthy source. There is no shared fallback to inherit by accident.

### What a script gets

| | |
| --- | --- |
| `$CONVEYOR_ITEM_ID` `$CONVEYOR_ITEM_REF` | the item, and the bare id the provider knows it by |
| `$CONVEYOR_SOURCE` `$CONVEYOR_STAGE` | which source, which stage is being entered |
| `$CONVEYOR_WORKDIR` `$CONVEYOR_RESULT` | where it runs, and where to write structured output |
| the source's `env:` | provider config — `REPO`, label mappings, anything |
| stdin | the whole item as JSON, plus `stage` and `from` |

### An inline script

For a stage where every source does the same thing, `run:` takes the body
instead. It must start with a shebang — the engine writes it out and execs it,
and never picks an interpreter:

```yaml
  - name: announced
    run: |
      #!/usr/bin/env bash
      echo "item $CONVEYOR_ITEM_REF reached $CONVEYOR_STAGE"
```

It cannot vary per source — that is what `script:` is for — so keep it small.
Anything with real logic wants to be a file, where a shell can check it and a
person can run it by hand. The body is written into the run directory, so the
exact text that ran is archived with its own logs.

The starting template is in [`examples/`](examples/):

```bash
cp -r examples/conveyor.d/_source ~/codes/conveyor.d/midgame
```

## Writing a provider

A provider is a folder under `providers/` holding one script per verb:

```
providers/github/
  list.sh      open issues -> items
  move.sh      item stage -> a status:* label
```

The engine finds them by name, with or without an extension — `list.sh`,
`list.py` and a compiled `list` are equivalent, because the runner execs the
file directly and never consults an interpreter. Exactly one match per verb is
required; two would make the choice depend on glob order.

Writing a provider is creating a folder. Nothing registers it.

Per-repository detail lives in the source's `env:`, which is how one adapter
serves many repositories:

```yaml
sources:
  - name: midgame
    provider: github
    workdir: ~/codes/midgame
    env:
      REPO: RaptoR-Soft/midgame
      # The source owns the provider<->stage mapping; the engine never sees a
      # label. Stages with no entry simply have nothing written.
      STAGE_LABELS: |
        refining=status:refining
        ready=status:ready
        in-progress=status:in-progress
        blocked=status:blocked
```

Credentials are not part of this: the script inherits the ambient environment,
so `gh`'s existing auth works and no token belongs in a committed config.

`providers/github/selfcheck.sh` exercises both scripts against a stubbed `gh`
and in dry-run, touching no network and no repository.

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
