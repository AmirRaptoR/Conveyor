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
SSE, run history, drag-to-reorder, drag out of the backlog to start an item now
(`POST /api/items/{id}/start`), each mark's reason on its card with a hand-back
button, `Unblock all`, and a strip saying how each agent is doing. It advances items on its own — `-watch`
is what makes it observe without touching anything. Discovery, scheduling and
the tick button are three goroutines, deliberately: a 90-minute stage must not
be able to stop every source being listed.

`internal/store` holds the persisted manual input order — v1's only human
control lever — and the data directory's owner lock.

Basic auth is in the app (`auth:` in the config, `conveyor passwd <name>` to
mint a line), not in a proxy in front of it: the nginx that used to do it was a
second server, a second config file and a second password store for one line of
behaviour. Only hashes go in the config — pbkdf2-sha256, parameters carried in
each line so the cost can be raised later without invalidating them — and a
plaintext password there is a load error, not a warning. **Serving a
non-loopback address with no `auth.users` is refused**, because the board starts
agent runs, reorders work and hands items back: reaching it is enough to drive
every repository the config enrols.

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
- **An item gets its own worktree, not the source's checkout.** `agents/_worktree`
  puts each item in `~/codes/.worktrees/<source>/<ref>`, branched from the
  remote's default branch (or from the pull request's branch on a rework), and
  the implement and review adapters `cd` into it. Three things follow. Two items
  in one repository can run at once, because they share only the object store,
  which git already makes safe. A tree left dirty by a dead run belongs to
  exactly the item that owns it, instead of to every item in the repository —
  which is what lets `implement` **resume** from it rather than refuse to start
  (`agents/_resume`): the leftovers are that item's own work on its own branch
  by construction, so the agent is handed the diff and told to read and verify
  it before building on it. A run dying part-way is normal, and the usual cause
  is a usage limit landing mid-stage. Two cases still stop it: resuming at the
  same commit more than `RESUME_MAX_ATTEMPTS` times, which is a loop rather than
  an interruption, and leftovers older than `RESUME_COLD_DAYS`, which are no
  longer a killed run. And the checkout a *person* works in is never touched —
  uncommitted work can sit in it for a week and the pipeline neither notices nor
  cares.
  `perSource` is therefore a throughput choice now, not a safety rule. It is
  still not unlimited: `refine` and `deploy` run in the source's own checkout.
  Worktrees are created by the adapter and removed by `approve` once a merge is
  confirmed — the engine knows nothing about any of it, because how the work
  happens is the script's business. `review` deliberately leaves the tree
  behind: `approving` still needs the branch.
  Agents must not change branch: several checkouts of one repository are live at
  once and `main` is checked out elsewhere. Both adapters say so in the prompt,
  and `/implement` step 2 defers to a branch it was handed.
- **The line is worked from its far end backwards.** `pipeline.Pick` ranks by
  how far along an item is before anything else: the item closest to done goes
  first, and the backlog is reached last. A pipeline exists to finish items, and
  a scheduler that always reaches for the head of the backlog widens the band of
  half-finished work — each one holding a worktree, a branch and a pull request
  going stale. Depth is the position of the stage an item is *heading into* in
  the config's stage list, which is already the line's order (`onSuccess` unset
  falls through to the next stage declared); following `onSuccess` from the
  front could not measure anything, because a rework edge makes the graph a
  cycle. A queue and the stage it feeds share one depth, so an interrupted run
  still outranks the queue behind it — recovery is the tie-break, one rung down.
  Manual order and priority decide *within* a stage, which is where a human
  lever belongs: a choice about what to start next, not a way to jump work
  already in flight.
- **Exit 10 means the next poll, and the engine enforces it.** A no-op in the
  stage the item was already in puts that item in `Server.resting`, and the
  scheduler skips it until a listing lands (every listing clears the set; so
  does the tick button, which means "look again now"). Without it a deferring
  stage re-ran on the wake its own transition caused: an `approving` stage
  waiting on a quiet pull request filed fourteen hundred runs an hour, each a
  real API call. The circuit breaker in `schedule` capped each burst and let
  the next one start, so it read as a warning rather than a fault.
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
- **A stop is either a question or a condition, and the script says which.** A
  condition (`limit`, `worktree`, a network that was down) may have passed, so
  the engine clears it in bulk: `Unblock all`, `retryStalled`, a quota
  returning. A question (`asked: true` in `$CONVEYOR_RESULT`, what `asks` in
  `agents/_blocked` writes) is never bulk-cleared and never counts towards a
  stall — nobody answers a question by waiting, and handing it back unanswered
  spends a run to be asked it again. Condition is the default because it is the
  safe one to get wrong. The engine reads the flag, never the word beside it.
- **A mark is one word and a paragraph.** The word (`decision`, `limit`,
  `worktree`, `error`) is all a card shows and all a provider labels; the
  paragraph is one click away in the panel. Scripts own the vocabulary — the
  engine never reads it, only checks it is short and unpunctuated, because it
  becomes a label on someone's tracker.
- **The provider says whether an item is marked; the engine says why.** The
  reason comes from the run that marked it and is recovered from run history
  after a restart — never from a label, and never by parsing a log. A red card
  that cannot say what it is waiting for sends the reader to the logs, which is
  the trip the mark exists to save.
- **Only a person clears a mark**, with two exceptions, and both are the outside
  world coming back rather than a decision being made. `retryStalled:` clears
  them all when *every* item is marked and the line cannot move at all — the
  guard is "everything" deliberately. And an agent's `status` going `limited` →
  `ok` clears the marks of kind `limit`, those and no others: the script itself
  said nothing was wrong with the item, and the quota returning is the whole
  answer. A `decision` mark is never touched — no amount of waiting produces an
  answer only a person has, and clearing it spends an agent run to be told the
  same thing.
- **A stop can be answered, and answering is unblocking.** `POST
  /api/items/{id}/unblock` takes `{"answer": "…"}`; the reply reaches the next
  run of that stage in its stdin as `answer`, beside the `session` the agent
  left in `$CONVEYOR_RESULT`. Both are opaque passthrough, like `Item.Raw` — an
  adapter that can resume says the answer into the conversation that asked, one
  that cannot leads its prompt with it. One endpoint, because an answer recorded
  against an item nobody handed back is a note in a drawer. Spent on use.
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
- **Every label this pipeline owns begins with `LABEL_PREFIX`** (`conveyor:`, a
  provider param, never a config key). One namespace, so a repository can see at
  a glance which labels a machine writes — and so `move` can find what it
  manages by *shape* rather than by a list that goes stale. Managed used to be
  the right-hand sides of `STAGE_LABELS` alone, which meant renaming a stage
  orphaned its old label forever: nothing took it off, and listing takes the
  first mapped label it finds, so an item landed in a stage nobody put it in.
- **`list` reads closed issues it labelled, and only those.** A finished item is
  a closed issue — the pull request says `Closes #N` — so listing open ones
  alone left the last stages empty: an item did not arrive in `done`, it
  disappeared on the way. The asymmetry is deliberate: an *open* issue that was
  handed over but carries no stage label yet is new work and lands in
  `DEFAULT_STAGE`; a *closed* one with no stage label is finished, and letting
  the onboarding tag alone put it back would re-list it as new work every poll.
- **Listing is opt-in, and there is no way to opt out.** Three labels put an
  issue on the board and nothing else does: a mapped stage label (the pipeline
  put it there), the `BLOCKED_LABEL` (it stopped there), or the bare onboarding
  tag — `LABEL_PREFIX` with its separator taken off, so `conveyor:` gives
  `conveyor`. Everything else is left entirely alone: not on the board, not
  counted, no stage ever run against it. There used to be an `IGNORE_LABELS`
  opt-out, which was the wrong way round — it made every issue in a repository
  conveyor's until someone said otherwise, so a repo could not be onboarded one
  issue at a time. Saying nothing is now the opt-out, and a person can work an
  issue by hand beside a running pipeline by simply not tagging it. The tag sits
  *outside* the namespace on purpose: `move` manages `conveyor:*` by shape and
  would strip a tag inside it at the first stage mapping to no label. Filtering
  happens in `list`, so the engine never learns the concept.
- **An un-onboarded repo is reported, never worked around.** Per-source problems
  disable that source and leave every other repo running. There is no shared
  fallback to inherit by accident.

## Merging is a gate, not a judgement

`review` ends when a pull request exists; it no longer merges. The `approving`
stage between `review` and `merged` runs `agents/claude/approve`, which waits
for the PR to settle and then merges it: no hold, not a draft, no conflict,
checks green, no unresolved review threads, and quiet for `QUIET_SECONDS`
(default 600). Any activity resets the clock, so the total wait is unbounded on
purpose — while review is still happening, the clock should keep giving it room.

**It must never sleep.** An item resting in a script stage has that script
re-run on every poll, and exit 10 is "leave it, try again next poll" — so
waiting is a script that returns immediately, not one that blocks. A `sleep`
there would hold a global slot for its duration and die with the engine on every
restart. A quiet PR costs a handful of `gh` reads and no model at all; the model
runs only to address an unresolved review thread.

The gate is fourteen rules in a fixed precedence, documented at the top of the
script and driven by `agents/claude/approve-selfcheck` against a stubbed `gh` —
most of them cannot be reproduced against a live repository on demand. Two
details worth keeping: the PR is found by *closing reference* and never by `gh
pr list --search`, which matches unrelated text and silently takes the first
hit; and a thread the script already answered but a reviewer left unresolved
becomes `asks decision`, never another attempt, which is what bounds the loop —
exit 10 clears the attempt count, so without it the model would re-run every
poll forever.

`providers/github/onboard.sh` creates every label a repository needs in one
command — the stage labels, the mark, the onboarding tag and one label per kind
of stop — idempotently, and never overwrites an existing label's colour or
description. Labels are still created lazily by `move.sh` when they are first
needed; this is the one-shot so that is not a discovery spread over a week.

## Running agents

`agents/<name>/<script>` holds reusable adapters, resolved by `agent:` exactly
as `provider:` resolves under `providers/`. The prompt, tools, model and turn
limit are that script's `params:`. The adapter prepends the item to the prompt
and appends the blocked convention, so prompts stay portable and every agent
signals "needs a human" identically: exit 20, with `{"blocked": true, "reason":
"…"}` in `$CONVEYOR_RESULT`. The engine hands that reason to `move`, and the
GitHub provider comments it on the issue beside the label.

## Agent status

`agents/<name>/status` is optional and reports how that agent is doing — run on
the discovery tick beside the list scripts, and rendered as a strip above the
sources. The engine reads one field of it, `state`, and knows three words: `ok`,
`limited`, `unknown`. Everything else is passed through and drawn as given.
What a usage window is, what counts against it, whether tokens or money is the
interesting number — that differs per agent and belongs to the script, which is
why there is no struct for it here. `agents/codex/status` reads the rate limits
Codex records in its own rollouts; `agents/claude/status` sums the last five
hours from `~/.claude/projects` and reports the refusal Claude Code files when
the account is over its limit. Neither is the account's ledger and both say so.

**`limited` stops the scheduler dispatching to that agent**, until the reset it
named or until it reports otherwise. A quota belongs to an agent, not to an item
or a source: every stage naming that agent meets the same wall in every
repository, so marking items one at a time was only a way of discovering the
same fact once per item. The first refused run pauses the agent too — a `limit`
mark is account-level news, and waiting for the next probe costs a whole poll of
runs to be told what that one just said. `retryStalled` stands down while
anything is paused, because its premise is that the cause may have passed and
this is a cause with a known end. Stages that run no agent keep moving, and the
tick button still overrides. Nothing wedges it shut: a probe that fails, or an
agent with no status script at all, lifts the pause rather than holding it.

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
