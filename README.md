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

> **Status: working.** The engine runs end to end against mocks and against
> real repositories — config, runner, sources and the transition table are
> tested, `conveyor serve` renders the board, and `providers/github/` runs
> against real issues. See [docs/DESIGN.md](docs/DESIGN.md) for what remains.

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
| `10` | No-op | Leave the item where it is; not retried until the next listing |
| `20` | Blocked — needs a human | Mark the item blocked in place, with a reason |
| anything else | Failure | Mark the item blocked in place, counting an attempt against `maxAttempts` |

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
./conveyor tick     -c conveyor.example.yaml -n 10 -v
```

`conveyor.yaml` is the working config and is deliberately untracked;
`conveyor.example.yaml` is the template that ships.

A real working config is usually kept outside the checkout entirely — it is
machine state, not project source. It then has to say where the adapters are:

```yaml
providers: ~/codes/Conveyor/providers
```

Without the key, `providers/` is looked for beside the config file.

You should see the furthest-along item go first each pass — priority only
decides between items at the same depth — one item end up marked blocked in
place in `refining`, and then `nothing to do`.

### Serving the board

`conveyor serve` runs the pipeline and renders it. On your own machine that is
all there is to it:

```bash
./conveyor serve -c conveyor.yaml -addr 127.0.0.1:8090
```

Anywhere else, put a password on it. The board is a control plane — it starts
agent runs, reorders work and hands marked items back — so **serving a
non-loopback address with no `auth.users` is refused**, not warned about:

```bash
./conveyor passwd amir      # prompts twice, prints the block to paste
```

```yaml
auth:
  realm: conveyor
  users:
    amir: "pbkdf2-sha256$600000$…$…"
```

Only hashes go in the config; a plaintext password there is a load error. Each
line carries its own parameters, so the cost can be raised later without
invalidating the lines already written. Auth is in the app rather than in a
proxy in front of it because a proxy was a second server, a second config file
and a second password store for one line of behaviour.

## Configuration

```yaml
version: 1
concurrency:
  perSource: 1      # items in flight per source — safe above 1 too, since an
  global: 1         # item works in its own worktree; see docs/CONTRACTS.md §5
stages:
  - name: backlog                        # no script: a queue
  - name: refining
    script: refine    # every source must provide a script by this name
    onSuccess: done   # explicit; would default to the next stage anyway.
                       # There is no onFailure and no onBlocked: a refine that
                       # stops wears a blocked mark where it stopped, and
                       # resumes there once a person clears it.
  - name: done
    terminal: true
sources:
  - name: mock
    provider: mock    # a folder under providers/
    workdir: .
    scripts:
      refine:
        agent: mock   # resolves agents/mock/refine
```

Only sources listed in the config are ever touched. There is no directory
auto-discovery: enrolling a repository is adding an entry.

`provider:` is the one thing a source says about *how* to talk to its backend.
It cannot name `list` and `move` separately, deliberately — that would let one
source pair GitHub's list with Azure's move, a mismatch nothing downstream could
detect, because by then they are just two executable paths.

## Onboarding a source

`stages:` is the state machine: which columns exist and where an item goes next.
The only thing a stage says about execution is `script:` — a **name**. A stage
with no `script:` runs nothing and is a queue.

A source declares what those names are:

```yaml
sources:
  - name: midgame
    workdir: ~/codes/midgame

    # How it reaches its backend. Params here reach ONLY list and move.
    provider:
      name: github
      params:
        STAGE_LABELS: |
          refining=conveyor:refining
          ready=conveyor:ready

    # What this source IS — reaches every script it runs.
    env:
      REPO: RaptoR-Soft/midgame

    # What this source PROVIDES, for the names the stages ask for.
    scripts:
      refine:
        agent: claude          # resolves agents/claude/refine
        params:
          ALLOWED_TOOLS: "Bash,Read,Glob,Grep"
          MAX_TURNS: "40"
          PROMPT: |
            Refine this issue until it is ready to implement.
            ...
      implement:
        agent: claude
        params:
          MAX_TURNS: "200"
          PROMPT: |
            Implement this issue.
            ...
```

`agent: claude` resolves `agents/<name>/<script>` exactly as `provider: github`
resolves `providers/<name>/<verb>`. Use `script: <path>` instead for anything
that is not a shipped agent — absolute, `~/`, or relative to the config.

Swapping an agent is one line, and it changes only that source. Nothing is
written into the repository being worked, so an agent told to "commit and push"
cannot sweep the pipeline into its own pull request. Scripts still *run* in the
source's workdir, so `claude` there resolves that repo's `.claude/skills/`.

**`env:` and `params:` are different scopes**, and the difference matters:

| | reaches | for |
| --- | --- | --- |
| `provider.params:` | `list` and `move` only | the backend's own vocabulary — `STAGE_LABELS` |
| `env:` | every script | what the source **is** — `REPO` |
| `scripts.*.params:` | one script | what it **needs** — its prompt, its tools |

A provider needing no configuration stays the one-line `provider: github`.

Params are per-script because two stages both want to be handed a `PROMPT`. At
source level the second would overwrite the first. Params win on conflict.

**A source that does not declare a script its stages ask for is reported:**

```
    source "caravan-v2" cannot run:
      - stage "in-progress" needs a script named "implement",
        which this source does not declare

3 of 4 source(s) unusable; the other 1 will still be worked
```

It is *that source's* error: conveyor reports it, skips it, and keeps working
every healthy source. There is no shared fallback to inherit by accident.

### What a script gets

| | |
| --- | --- |
| `$CONVEYOR_ITEM_ID` `$CONVEYOR_ITEM_REF` | the item, and the id its provider knows it by |
| `$CONVEYOR_SOURCE` `$CONVEYOR_STAGE` | which source, which stage is being entered |
| `$CONVEYOR_WORKDIR` `$CONVEYOR_RESULT` | where it runs, and where to write structured output |
| the source's `env:` + the script's `params:` | configuration the engine carries and never reads |
| stdin | the whole item as JSON, plus `stage` and `from` |

The shipped agents prepend the item to `PROMPT`, so a prompt is instructions
only — no ids to interpolate, no substitution language — and append the blocked
convention, so every agent reports "needs a human" the same way.

### An inline script

For a stage where every source does the same thing, `run:` takes the body
instead. It must start with a shebang — the engine writes it out and execs it,
and never picks an interpreter:

```yaml
  - name: announcing
    run: |
      #!/usr/bin/env bash
      echo "$CONVEYOR_SOURCE #$CONVEYOR_ITEM_REF is done"
```

It cannot vary per source — that is what `scripts:` is for — so keep it small.
Anything with real logic wants to be a file, where a shell can check it and a
person can run it by hand. The body is written into the run directory, so the
exact text that ran is archived with its own logs.

### Onboarding a GitHub repository

Every label the pipeline needs, in one command — the stage labels, the mark, the
onboarding tag and one per kind of stop:

```bash
REPO=owner/name STAGE_LABELS="$(…the source's provider params…)" \
  providers/github/onboard.sh
```

Running it twice changes nothing, and it never overwrites an existing label's
colour or description — someone may have recoloured one on purpose. Labels are
also created lazily as they are first needed, so this is a convenience, not a
prerequisite.

Then tag an issue with the bare namespace word (`conveyor`) to hand it over.
Listing is opt-in and there is no way to opt out: an issue nobody tagged was
never the pipeline's.

### Merging is a gate, not a judgement

`review` ends when a pull request exists. The `approving` stage after it waits
for that PR to settle and merges it only when every gate passes — no hold, not a
draft, no conflict, checks green, no unresolved review threads, and quiet for
`QUIET_SECONDS`. Any activity resets the clock, so there is always a window in
which a comment can change the outcome.

It never sleeps: an item resting in a script stage has that script re-run every
poll, so waiting is exit 10, not a blocked process holding a slot.

## Writing a provider

A provider is a folder under `providers/` holding one script per verb:

```
providers/github/
  list.sh      open issues, and closed ones it labelled -> items
  move.sh      item stage -> a conveyor:* label
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
      # label. Listing is opt-in: a stage with no entry here is a stage whose
      # items are never labelled, so they drop off the board.
      STAGE_LABELS: |
        refining=conveyor:refining
        ready=conveyor:ready
        in-progress=conveyor:in-progress
        blocked=conveyor:blocked
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

## Running the checks

```bash
./check
```

is the one command: `go build`, `go vet`, `go test`, `go test -race`, every
self-check suite in the tree, `bash -n` over every shell script under
`agents/` and `providers/`, and `conveyor validate` against the example
config — in that order, stopping at the first failure and naming it.
`.github/workflows/check.yml` runs this same script on push and pull request
against `main`, so CI and local are never two things to keep in sync.

Self-check suites are found by name, not listed: anything called `selfcheck`
or `selfcheck.sh`, or ending `-selfcheck`/`-selfcheck.sh`, anywhere in the
tree. Add one and `./check` picks it up on its own.

Adding a browser-free UI interaction test — no browser, no npm install, no
network — means adding a file named `*.test.mjs` anywhere in the tree, written
against Node's own `node:test` and `node:assert` (nothing else is
installed). `internal/server/web/testutil.mjs` has a small helper,
`loadFunctions`, that pulls a named function straight out of `index.html`'s
inline script and evaluates it in a sandbox with `node:vm` — no DOM, no
build step — so a pure function in the board's UI can be tested exactly as
written; `internal/server/web/format_duration.test.mjs` is a working example.
`./check` runs every `*.test.mjs` file it finds under `node --test`.

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
