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
(`validate | list | run | tick`). `./conveyor tick -n 10 -c conveyor.example.yaml`
drains the mock pipeline in priority order and marks a blocked item in place.
`providers/github/` runs against real repositories.

`conveyor serve` runs the pipeline and renders it: the board, live logs over
SSE, run history, drag-to-reorder, drag out of the backlog to start an item now
(`POST /api/items/{id}/start`), each mark's reason on its card with a hand-back
button, `Unblock all`, a `Diagnose` sweep, and a strip saying how each agent is
doing. An Inbox tab beside it (#40) is the same items again, reordered around
attention rather than stage: a question, a failure, a dependency wait, a quota
limit and a resting "pending" item are five distinct labels, never one grey
"blocked", and a source that has not been listed recently marks its own items
`stale` so old data cannot read as current. Its filters (source, search) are a
client-side array transform with no fetch and no ordering rule of its own —
never mistaken for the scheduler's own order or a provider write — and every
action opens the very same `#panel` `inspect()` does, so nothing there is a
second implementation of hand-back, Answer, or the report/history/log it
already carries. It advances items on its own — `-watch`
is what makes it observe without touching anything. Discovery, scheduling and
the tick button are three goroutines, deliberately: a 90-minute stage must not
be able to stop every source being listed. `Diagnose` (`POST /api/doctor`)
sweeps every marked item through its source's `doctor:` script, reading each
one's own reason before deciding whether to retry it, resume it or leave it
for a person — a third, more careful exception to "only a person clears a
mark" (CONTRACTS §6). Dry run by default; a person presses `Apply` once the
sweep has shown what it would do. `agents/claude/doctor` and `agents/mock/doctor`
are the shipped policies; a source declaring no `doctor:` just has its marked
items skipped by a sweep.

The board is an installable app (`web/manifest.webmanifest`, `web/sw.js`) and
sends Web Push when an item asks a question or reaches a terminal stage —
the two moments worth hearing about without watching. `internal/push` does
RFC 8291/8292 on the standard library (`crypto/ecdh`, `crypto/hkdf`), proven
against the RFC's own test vector; the VAPID key pair lives in
`<data>/vapid.json` and subscriptions in `<data>/push.json`, both created on
first use and never committed. No account anywhere: a device presses
"Notify me" once (or installs the app, which asks on its own), and
`POST /api/push/test` proves the path. **Push needs a secure origin**, so
the live board is fronted by Caddy for TLS on the host — the app itself
still binds loopback and keeps its own basic auth.

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
every repository the config enrols. `serve` also always listens on a Unix
socket at `<data>/api.sock`, mode `0600` and no password at all — a caller on
the same OS user already has full authority over the data directory, so a
loopback TCP peer check would be trusting exactly what a same-host proxy
(Caddy, `cloudflared`) can forward by accident; TCP keeps auth regardless of
peer, and the socket is the only door with no password on it (#94).

## Invariants — do not break these

- **stdout/stderr are logs and are never parsed.** Structured data comes back
  only via `$CONVEYOR_RESULT`. An AI stage script emits megabytes of prose.
- **The engine writes provider state before running a stage**, never after, and
  stage scripts never call `move` themselves. A crash mid-stage then leaves a
  truthful record and the item is not handed out twice.
- **A stage's timeout reaches its agent, as an instant.** `CONVEYOR_DEADLINE`
  is when the process group dies, in UTC, read off the context that enforces it;
  unset means the stage has no timeout and must never be read as "expired".
  `agents/_deadline` turns it into a brief giving the agent an earlier deadline
  (`WRAP_UP_GRACE`, 300s) so it can commit and push before the wall instead of
  being cut mid-verification. The brief never says to hurry — a review that goes
  green because time ran out is worse than the timeout, which at least marks the
  item and fetches a person — and the agent's deadline is derived from the
  engine's, so `conveyor.yaml` stays the only place the number lives.
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
  Reclaim is a positive allowlist, never an inference from a clean status: a
  path is touched — created into, emptied, or removed — only when it resolves
  to exactly `<managed root>/<source>/<ref>` and carries the marker
  `item_worktree` wrote into it when it created it — or, missing that marker,
  proves itself anyway: a **linked** worktree (its git dir sits under the
  repository's common dir's `worktrees/`, not the common dir itself) of this
  same repository whose `HEAD` is exactly `issue-<ref>` is a worktree made
  before the marker existed, since #33 shipped the marker with no migration
  path and stranded everything created before it. The marker is written the
  first time that proof succeeds, so adoption costs nothing again, and a
  marker that fails to write (a read-only git dir) does not undo the proof —
  the next call just proves the same worktree again. The proof stands on its
  own merits and never expires: there is no reliable "predates the marker"
  signal to retire it by. The primary checkout, an
  external `git worktree`, a directory a person made, a symlink escaping the
  managed root, a linked worktree of a *different* repository or on a
  *different* branch — none of that is ever Conveyor's, however clean or old
  it looks, and adoption leaves it untouched. A branch already checked out
  somewhere else is refused, not
  reclaimed: reclaiming a *clean* holder used to be the rule, and Git's own
  refusal to remove the primary checkout — always clean, always listed in
  `git worktree list` — is exactly what turned that refusal into the trigger
  for `rm -rf` on it. The sweep that reclaims spent worktrees reads an
  explicit lease instead — the owning process's PID, or the run's deadline,
  written every time a stage picks the worktree up — because directory age
  says nothing about whether an agent is still using it.
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
- **A limit is about a resource, never about a transition.** `resources:` names
  the scarce things — one account's quota, one deploy host, one staging
  database — and a stage lists which it spends; a transition takes one of each
  before it starts, all or none, and gives them back when it ends.
  `global`/`perStage`/`perSource` count transitions, which meant the number had
  to be chosen for the agent runs and a stage that only reads the GitHub API
  queued behind them for nothing. A stage naming no resource is bounded only by
  `global`. A resource named with no limit declared is a load error, and an
  undeclared one is refused at the lock rather than treated as unlimited: a
  silently unlimited resource is the failure this exists to prevent. The same
  stage is Claude in one repo and Codex in the next, so a source's
  `scripts.<name>.resources` replaces the stage's list; `[]` there means "spends
  nothing", which is not the same as saying nothing.
- **A script says what it is waiting for; a person says "not any more".** Exit
  10 with `{"waiting": {"until": …, "why": …}}` draws a live countdown on the
  card — a resting item and a stuck one look identical otherwise. A stage
  declares `actions:`, the board draws them as buttons, and pressing one arms
  one word for the next run of that stage (`$CONVEYOR_MANUAL`, and `manual` on
  stdin) and clears the deferral so it happens now. That word is the whole of
  the manual override: it cannot move an item, choose a stage or skip a script.
  `approve` reads `merge-now` as "stop giving review more room" and *only*
  that — every other gate still has to pass, so nobody presses a red build
  into main.
- **The scheduler claims a slot before it launches.** Checking whether a slot is
  free and then starting a goroutine leaves a gap in which the next pass decides
  the same thing again. `Engine.Advance` does not lock; its caller must.
- **The item's status is the truth about the item.** A closed-as-completed
  GitHub issue is finished, whatever stage label it is still wearing, and
  `list.sh` reports it in the terminal stage and reconciles the labels through
  `move.sh` rather than keeping a second copy of that script's rules. It used
  to be marked in place instead — "closed while still in `approving`" — which
  produced a red card nobody could ever clear, because the thing that would
  clear it (the PR becoming open again) had already happened and cannot
  un-happen. A pull request merged by hand is the ordinary way to get there.
  The mirror image is the same rule: an open issue wearing the terminal stage
  label is *not* finished and is marked so a person reconciles it, and an issue
  closed as not planned is not work any more and leaves the board entirely.
- **A mark is one label and a section in the issue body.** `conveyor:blocked`,
  and nothing beside it — the `conveyor:blocked: <kind>` label is gone, because
  it put the same fact in a third place and made "everything blocked" two
  queries. The kind and the reason go into a `<!-- conveyor:block … -->`
  section at the top of the issue: rewritten in place on every mark, removed
  when the mark goes, and read straight back by `list.sh` into `blockReason` /
  `blockKind`. That is what the board shows. Run history is the *fallback*, not
  the source, and only a run of the stage the item is actually in — the walk is
  newest-first across the whole store, so without that restriction a three-day
  old `in-progress` failure was displayed as the reason a card was stuck in
  `approving`. The old append-only comment is gone with it.
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
  For a model-written stop the split lives in `agents/_blocked`'s
  `CONDITION_KINDS`/`is_condition_kind`: `agent_asked` (`agents/claude/_stream`)
  derives `asked` from the kind the model actually declared, never forces it
  true, so a model that correctly recognises a condition is not promoted to a
  question it never asked (#68).
- **A question carries its options.** `decision_brief` in `agents/_blocked` is
  the paragraph every adapter appends: stop by writing `kind: decision`, a
  one-sentence `reason`, and `questions` in the AskUserQuestion shape (the tool
  itself is not available under `claude -p`; verified). The engine passes
  `questions` through on the block unread; the board draws a modal that walks
  them one at a time and posts the choices as the `answer`. The board shows a
  stop in one of three tones — a question (violet, "needs you"), a condition
  that passes on its own (grey: `limit`, `turns`, `unfinished`, `dependency`,
  `worktree`), or a fault (red) — and lists every question in a strip under
  the masthead. The tone is presentation: the engine still reads only `asked`.
- **An item's pull request is found by closing reference, never by search.**
  `agents/_pr` is the one lookup — "Closes #N" in the body, or the
  `issue-N` branch `_worktree` names — used by implement, review and approve.
  `gh pr list --search "$ref"` matched the number anywhere in any PR and handed
  issue 188 the PR whose body said "depends on #188". `agents/_pr` also asks
  GitHub whether an item has *landed* — `item_landed`, straight off
  `Issue.closedByPullRequestsReferences` rather than a page of `pr list` —
  because no *open* PR is not the same as nothing landed: it may already be
  merged, and a guard that cannot tell those apart marks a finished item
  `no-output` forever, since a merged PR never becomes open again for
  `retryStalled` to find. This is not a second way to pick a pull request to
  work on; it only ever answers whether this item is already done, and stays
  the one place both implement and review ask that question.
- **An item sequenced behind an open issue is not started.** `agents/_deps`
  reads "Depends on #N" / "Blocked by #N" lines from the body — inline, or a
  bare "Depends on:" line followed by one markdown list item per predecessor,
  which is the shape a refined issue actually writes and which used to
  declare nothing (the keyword line had no number on it, and a line break is
  a sentence break); implement stops before the worktree with a `dependency`
  mark, a condition the doctor clears once every named issue is closed. The
  agent is also told not to stop for an open dependency itself — nine of one
  week's seventeen "decisions" were that. The GitHub provider's `list.sh`
  restates the same parse for `dependsOn`, and `agents/_deps-fixtures.jsonl`
  is the one corpus both selfchecks read so the two cannot drift.
- **A follower never passes what it depends on.** `pipeline.Deps` is built from
  the full listing each pass and gates rung 1 of `pipeline.Target`: an item may
  enter a stage its dependency has already entered, may share it, may never
  pass it, and may not walk into a stage its dependency is marked in. Stated
  once, in `Target`, so `Pick`, `Order` and the drag endpoint's 409 cannot
  disagree about it. A stage raises that bar for itself with `dependenciesAt:
  <stage>` — the implement stage says `merged`, because an item built while
  its dependency is still on a branch produces a pull request that cannot
  merge on its own, and every issue must be deliverable by itself even as a
  slice of a bigger feature. The hold carries `until` so the board can say
  "must reach merged" instead of "cannot enter in-progress". Every unusable edge — an unknown id, a self-edge, a stage
  the config does not declare, a cycle — is dropped rather than held, because
  the alternative is a pipeline a person wedges shut by mistyping one line of
  an issue body; only the edges inside a cycle go, so an unrelated dependency
  off the same item still holds. This is the line's own sequencing and is not
  `agents/_deps`, which still asks GitHub about issues the board cannot see —
  narrower (an un-onboarded sibling gates nothing here) and later (it catches
  what the engine could not see, immediately before the worktree). `Unblock
  all` asks the same rule directly — would this item be held if it were not
  marked — rather than through `Target`, which would refuse it on its own mark
  first; a marked item still held this way gets no provider write and is
  reported separately (`heldByDependencies`), never through the mark's own
  kind, which stays the scripts' vocabulary. A hold is drawn on the board grey,
  in `.item.held` — reusing the tone of a condition that passes on its own, but
  never the `dependency` mark kind, because nobody clears a hold and a mark is
  a decision waiting on a person; the two must never be drawn the same way.
- **A merge conflict is work before it is a decision.** `approve` tries one
  mechanical rebase in the worktree review left behind, pushed with a lease.
  If that does not apply cleanly, and `CONFLICT_RESOLVE` has not disabled it,
  and the PR is this pipeline's own (not a fork, headed by its `issue-<ref>`
  branch), one attempt merges the base into that same worktree — never a
  rebase there, since the final merge squashes and a multi-commit `--continue`
  loop is more chances to lose one. A model runs only if that merge actually
  conflicts; a base that now merges cleanly is committed, tested and pushed
  with no model run at all. A `<!-- conveyor:approve:conflict <headOid>
  <baseOid> -->` marker comment on the PR records a resolved-and-pushed
  attempt, so a conflict still reported at that exact head and base is a
  person's, once, rather than a model run repeated every poll; a genuinely new
  conflict (either OID moved) gets a fresh attempt. Marks `conflict` whenever
  the rebase fails and the attempt after it does too — verification, the test
  suite, the push, or recording the marker itself, same as the model actually
  disagreeing — or when the attempt is not this pipeline's to make.
  **A merge already in progress is resumed, and one with no conflicts left is
  simply finished** — committed, tested, pushed, marker recorded, no model run.
  That state is what a run killed mid-verification leaves, and the `turns` stop
  is the usual way to get there, so it is the common case and not a fault;
  marking it made it permanent, because the model was already out of the
  picture and every later poll found the same tree and marked it again. The one
  thing still handed over is a resolution that changes nothing at all against
  HEAD, which would push a branch whose own work had been discarded. The rebase
  at the front of the gate therefore refuses on a merge or rebase in progress
  and not merely on a dirty tree: a resolution staged to exactly what HEAD
  holds leaves `git status --porcelain` empty, and the `reset --hard` that
  rebase opens with would have thrown it away with nothing saying so.
- **A mark is one word and a paragraph.** The word (`decision`, `limit`,
  `turns`, `worktree`, `error`) is all a card shows and all a provider labels; the
  paragraph is one click away in the panel. Scripts own the vocabulary — the
  engine never reads it, only checks it is short and unpunctuated, because it
  becomes a label on someone's tracker.
- **The provider says whether an item is marked; the engine says why.** The
  reason comes from the run that marked it and is recovered from run history
  after a restart — never from a label, and never by parsing a log. A red card
  that cannot say what it is waiting for sends the reader to the logs, which is
  the trip the mark exists to save. The one exception is a mark no run in this
  pipeline produced: a listing may supply its own reason (`blockReason` on the
  item) for a mark it decided on itself — a closed issue found sitting in a
  non-terminal stage, say. A recovered run-history reason always wins when
  there is one; the listing's is only the fallback for the gap where no run
  explains the mark at all.
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
- **`list` asks for open and closed issues separately**, so a repository that
  has finished more issues than `LIMIT` still lists its open work, and each
  closed issue carries the `closedAt` it was fetched with as `finishedAt`.
  `pipeline.Order` is what actually reads the done column newest-first: a
  terminal item skips the manual-order and `priority` rungs entirely and sorts
  by `finishedAt` descending instead, which is also what merges every source's
  closed work into one ledger rather than one newest-first block per
  repository. The board draws ten and offers the rest ten at a time.
- **Open-issue discovery does not truncate before it filters.** `list.sh` asks
  `gh` once per enrolling label — every mapped stage label, the mark, the
  onboarding tag — and unions the results, rather than one `--state open`
  call capped at `LIMIT` before enrolment is even checked. Older enrolled work
  can no longer be pushed off the page by newer unrelated issues; a label-scoped
  call is server-side filtered; there is nothing to push it behind. Closed
  history stays a single listing, but is sorted newest-`closedAt`-first
  *before* `CLOSED_LIMIT` cuts it down, so that is the N most recently closed
  rather than an arbitrary N `gh` happened to return first.
- **`list` reads closed issues it labelled, and only those.** A finished item is
  a closed issue — the pull request says `Closes #N` — so listing open ones
  alone left the last stages empty: an item did not arrive in `done`, it
  disappeared on the way. The asymmetry is deliberate: an *open* issue that was
  handed over but carries no stage label yet is new work and lands in
  `DEFAULT_STAGE`; a *closed* one with no stage label is finished, and letting
  the onboarding tag alone put it back would re-list it as new work every poll.
  A closed issue that *does* carry a mapped stage label is only "finished" if
  that stage is terminal — `ListInput` carries `terminalStages` alongside
  `stages` for exactly this, so a list script never keeps its own copy of the
  stage graph. One whose mapped stage is not terminal stopped mid-flight
  (someone closed it by hand, say) rather than finished: it keeps that stage
  and is marked, with a `blockReason` a person can read on the card, instead of
  being silently reported as work the pipeline never actually completed.
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
(list, move and preflight only — labels are GitHub's vocabulary, not the
pipeline's), `env:` (every script — what the source is), and
`scripts.*.params:` (one script, winning on conflict). Script params are
per-script because two stages both want a PROMPT. Everything else in the
schema is structure.

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
