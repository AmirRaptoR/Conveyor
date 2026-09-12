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
  "blocked": false,             // a MARK, not a stage. true means a human must
                                // decide, and the item stays exactly where it
                                // is. The scheduler never picks a marked item.
  "blockReason": null,          // optional, and preferred over run history:
                                // the LISTING reads it back off the item
                                // itself, so it is this poll's truth and it
                                // survives a restart and a retention sweep.
                                // Run history answers only when the listing
                                // says nothing, and only a run of the stage
                                // the item is actually in (§6).
  "blockKind": null,            // optional. The same stop in one word.
  "priority": 2,                // 0 = most urgent. null = unranked.
  "dependsOn": ["midgame:33"],  // items this one is sequenced behind, by id.
                                // The provider translates its own vocabulary
                                // ("Depends on #33" in a GitHub issue body, or
                                // a bare "Depends on:" line followed by one
                                // list item per predecessor); the engine only
                                // ever compares ids, the same way it never
                                // sees a label name. Omitted or empty means
                                // nothing gates this item. See §5 for the
                                // rule this enforces.
  "assignee": null,
  "createdAt": "2026-08-20T10:00:00Z",
  "updatedAt": "2026-08-27T11:00:00Z",
  "finishedAt": "2026-08-30T09:00:00Z", // when the source considers this item
                                // finished. Provider-supplied and optional;
                                // empty when the source does not say. Only
                                // meaningful in a terminal stage — see §4a.
  "raw": {}                     // provider passthrough. Opaque to the engine,
                                // handed back to scripts untouched.
}
```

Rules the engine enforces:

- Unknown `stage` → the item is rejected and logged, not silently dropped.
- Missing `ref` → the item is rejected and logged, the same as a missing `id`.
- Duplicate `id` within one poll → first wins, the collision is logged. This is
  enforced across every source's listing, not merely within one source's own —
  the engine keys `working`, marks, timers and the manual order by the bare id,
  and two sources cannot be trusted not to collide.
- An item that disappears from a source is marked gone, not deleted, so its run
  history survives.
- A marked item is never picked. That is the whole mechanism by which a stage
  that asked for a human stops being re-run: nothing carries the item out of the
  line, so nothing has to decide where to put it back.

**The item's own status is the truth about the item, and a listing must not
argue with it.** A source that has a notion of finished — GitHub closes an
issue — reports what that status says, not what its own bookkeeping says. A
pull request merged by hand closes the issue with no stage of this pipeline
having moved anything; the item is finished, and reporting it in the stage its
labels still name (marked, because the two disagree) produces a card nobody can
ever clear. It is worse than useless: the mark cannot pass, because the thing
that would clear it has already happened. Report the terminal stage, and
reconcile whatever the provider records so the next listing derives the same
answer without the special case. The mirror image is the same rule: an item
whose status says it is *not* finished does not sit in a terminal stage on the
strength of a label, and a source that has a notion of *abandoned* — closed as
not planned — reports nothing at all for it, because it is not work any more.

## 2. The script contract

One contract for every script the engine runs. No exceptions — an extension
author learns it once.

**Input** arrives two ways:

| | |
| --- | --- |
| `stdin` | A JSON object: `{"item": {...}, "stage": "in-progress", "from": "ready", "blocked": false, "config": {...}}`, plus `"answer"` and `"session"` when this run follows a stop a person answered (§5a). For `list` scripts there is no item: `{"source": "midgame", "stages": ["backlog","ready",…], "terminalStages": ["done"], "config": {...}}` — `terminalStages` is which of `stages` are terminal, so a list script can tell a finished item from one that merely stopped without keeping its own copy of the stage graph in source `env:` |
| env | `CONVEYOR_RESULT` (path to write structured output), `CONVEYOR_WORKDIR`, `CONVEYOR_SOURCE`, `CONVEYOR_STAGE`, `CONVEYOR_ITEM_ID`, `CONVEYOR_ITEM_REF`, `CONVEYOR_DEADLINE` (§4b), `CONVEYOR_MANUAL` when a person armed one of this stage's `actions:` for this run (§5b), plus everything in the source's `env:` block |

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
| `20` | Blocked — needs a human | **Mark the item where it stands.** No retry, no move |
| `10` | No-op — nothing to do | Leave the item where it is, not an error |
| any other | Failure | Count an attempt; re-run until `maxAttempts`, then mark |
| timeout | Killed after `timeout:` | Treated as failure, logged as `timeout` |

`onSuccess:` is the only route in the schema. There is no `onFailure:` and no
`onBlocked:`, because there is nowhere else to send an item: work that stopped
stopped *somewhere*, and that is the one fact worth keeping. `maxAttempts:`
defaults to 1, so a failure that is not retried marks immediately — a failure
that neither routed nor marked would be re-run on every poll forever.

Exit 10 means the next **poll**, and the engine holds the item to it. A
finished transition wakes the scheduler, and the item that just exited 10 has
not moved — it is still the best candidate in the stage it never left, so
without this it is picked again immediately and the "try again later" it asked
for arrives two seconds later, forever. A stage that deferred is skipped until
its source's next listing lands — specifically, a listing that *began* after
the deferral was set; one already in flight when the deferral was set has not
had the chance to answer it yet and leaves the item resting. A person pressing
the tick button clears every deferral regardless of timing: that gesture means
"look again now", and honouring one against it would answer a person with
nothing.

A blocked script may say why: `{"blocked": true, "reason": "…"}` in
`$CONVEYOR_RESULT`. The engine passes the reason to `move`, and a provider that
has somewhere to put it — a comment, a field — should.

Any stage script may also write `{"summary": "…"}` — one line of narrative,
unrelated to whether it blocked. The engine does not read it; the final report
(the panel's per-item write-up, built from run history) quotes it under the
stage that wrote it. No script is required to write one, and none does today.

## 3. The script kinds

**`list`** — read items from a provider. Writes a JSON array of items to
`$CONVEYOR_RESULT`. Exit 0 with `[]` means an empty backlog, which is normal.

What it does not emit does not exist: an item the lister filters out is not on
the board, not in a count, and nothing will ever be run against it. That is the
whole of deciding what the pipeline works on, and it belongs to the script: the
GitHub provider lists only the issues wearing one of its labels — a stage, the
mark, or the bare onboarding tag — and the engine never learns that the others
were there to skip.

**`move`** — write a stage change *and the blocked mark* back to the provider
(set a label, move a card, update a field). Called by the **engine**, never by a
stage script: the engine owns provider state so a crashed stage script cannot
leave it inconsistent. Receives
`{"item":…, "stage": "<target>", "from": "<current>", "blocked": <bool>,
"blockedReason": "…", "blockedKind": "…"}`.

How a provider records the reason and the kind is its own business, but it must
be somewhere the *item itself* carries, not somewhere only this process knows:
the GitHub adapter writes them into a marked section at the top of the issue
body, rewritten in place on every mark and removed when the mark goes. That is
what `list` reads back into `blockReason`/`blockKind`, and what lets a board
say why an item stopped after a restart, after a retention sweep, and for a
mark nothing in this pipeline made. One mark label and no more: the kind was a
second label once, which put the same fact in a third place and made
"everything blocked" two queries.

Both facts go every time, and `blocked` is always present. There is no separate
verb for the mark: a mark is provider state, and this is how provider state gets
written. Setting and clearing it are the same call with a different value.

**stage script** (whatever the source declares for that name) — do the actual work
of a stage. Run an AI agent, run
tests, deploy, cut a release. This is the extension point; everything else is
plumbing.

**`status`** (optional, `agents/<name>/status`) — report how an agent is doing.
Run on the discovery tick, beside the list scripts, and given no stdin. Writes:

```json
{"state": "ok" | "limited",
 "summary": "one line, shown on the board",
 "resetsAt": "<RFC3339>",
 "detail": [{"label": "5-hour window", "value": "34% used"}]}
```

`state` is the only field the engine reads, and it reads three words: anything
but `ok` or `limited` becomes `unknown`. Everything else is passed through and
rendered as given. What a usage window is, what counts against it, whether
tokens or money is the interesting number — all of that differs per agent and
belongs to the script, which is why the engine holds no struct for it. An agent
with no `status` script simply says nothing, which is not an error.

**`preflight`** (optional, `providers/<name>/preflight`) — a readiness check
for one source, run only by `conveyor preflight`, and by `conveyor enroll`
and `conveyor run`'s post-run checklist when they need the same answer. Never
run by the scheduler, and never as part of an ordinary poll: this is the
outside world checked deliberately, on request. Receives the same stdin
`list` receives — `model.ListInput` with `source`, `stages` and
`terminalStages`; `config` left unset, exactly as `Client.List` leaves it —
and `ProviderEnv()` (the source's `env:` plus its `provider.params:`), since
what this script checks (a label existing, a repository's permission level)
is the provider's own vocabulary and belongs to the same env `list` and
`move` already see. Writes:

```json
{"checks": [
  {"name": "gh on PATH", "status": "pass"},
  {"name": "label conveyor:blocked", "status": "fail",
   "detail": "missing", "fix": "providers/github/onboard.sh ..."}
]}
```

`status` is one of `pass`, `fail`, `warn`, `skip`. Any other word, or an
absent one, is read as `unknown` and counts as a failure, the same as an
unrecognised agent `state` does — a check whose outcome cannot be read has
not passed. Exit 0 means only "the checks ran, and here is what they found";
the verdicts carry the outcome. A non-zero exit, a timeout, or a failure to
start becomes one synthetic `fail` naming the exit code (or timeout) and the
run directory, since none of those left any verdict to trust. A malformed
envelope — not JSON, not an object, no `checks` key, `checks` not an array,
or an empty result on a 0 exit — becomes the same kind of synthetic `fail`.
A malformed *entry* inside an otherwise well-formed array does not take the
rest down with it: it becomes one `unknown` check named by its index, and
every valid entry beside it is still reported.

Resolved by name with or without an extension, exactly like `list` and
`move`. A provider shipping none is not a problem — `conveyor preflight`
reports a single `skip` for that source, and `Source.OK()` is unaffected;
providers that predate this issue keep working with no changes. Two matching
files is a single `fail` naming both, for the same reason two matching
`list` files would be an error: the choice must not depend on glob order.
Every invocation is recorded with `Kind: "preflight"`, bounded by
`discovery:` rather than `timeout:` — these are API reads, not agent work.

What a preflight script writes into `detail` or `fix` — or logs on its own
account — is that script's own promise, not something the engine polices:
the same standing `move`'s own comment on an issue already carries. A script
that echoes one of its own params into a `detail` string has broken that
promise, not the engine's redaction, which only ever covers what the *engine
itself* prints (env values in `conveyor run -explain`, `conveyor enroll`'s
stdout) and never rewrites a script's own prose.

**`doctor`** (optional, `scripts.doctor:` — a reserved source-script key no
stage names) — triage one marked item, on demand, as part of a *sweep* the
board starts across every marked item at once (`POST /api/doctor`). Not a
stage in an item's lifecycle: it runs across items, and a source that declares
none simply has its marked items skipped by a sweep. Receives

```json
{"item": …, "stage": "review", "blocked": true,
 "block": {"kind": "turns", "reason": "…", "stage": "review",
           "runId": "…", "at": "…", "asked": false},
 "runs": [ {"id": "…", "kind": "stage", "stage": "review", "outcome": "blocked",
            "exitCode": 20, "timedOut": false,
            "startedAt": "…", "finishedAt": "…", "dir": "…"} ]}
```

`runs` is that item's own history, newest first and capped at 20, including
earlier doctor runs — trimmed, deliberately not the shape a run is stored in:
it never carries `env` or `item`, which would hand a model every source
parameter, tokens included. `block` never carries the block's `session`. A
`decision` mark is never something a doctor may clear — see below.

| Exit | Means | The sweep does |
| --- | --- | --- |
| `0` | hand it back | clears the mark; if `$CONVEYOR_RESULT` has a non-empty string `answer`, records it first (with the block's own session) so the next run resumes into it |
| `10` | leave it | nothing |
| `20` | the doctor itself needs a person | nothing to the item |
| other, or a timeout | the doctor failed on this item | nothing to the item; the sweep continues |

Every invocation is recorded with `kind: "doctor"`, never `"stage"` —
§6's run history reader for a mark's reason reads only `kind: "stage"`, so a
doctor run that exits 20 is never mistaken for the run that marked the item.

A block with `asked: true` is skipped before the script runs — the same
`asked` guard every bulk clear already applies. Immediately before a `0` is
applied, the item is re-checked against the live board, not the snapshot the
sweep began with: unblocked, or re-marked under a new run id, since it was
diagnosed, and the row is skipped rather than acted on twice.

`CONVEYOR_DRY_RUN=1` reaches the script when the sweep is a dry run — the
default invocation, given the blast radius of a script that can act across
every onboarded repository at once. A dry run clears no mark and records no
answer, whatever the script exits; what a doctor writes to the *provider*
under it (a comment, say) is the script's own promise, exactly as it already
is for `move`.

**`source.template.yaml`** (optional, `providers/<name>/source.template.yaml`)
— not a script; never resolved by `findScript`, so it can never be mistaken
for `list`, `move`, `status`, `doctor` or `preflight`, the way `onboard.sh`
and `selfcheck.sh` already avoid a name `provider:` could resolve. It is the
one thing `conveyor enroll` reads to ask a provider's own questions without
understanding what a param means:

```yaml
prompts:
  - name: STAGE_LABELS_HINT   # [A-Z][A-Z0-9_]* — becomes {{NAME}} and an
                              # -answer flag; SOURCE, WORKDIR and SCRIPTS are
                              # reserved for the engine's own questions
    prompt: "a one-line label prefix"
    example: "conveyor"      # optional
    default: "conveyor"      # optional
template: |
  - name: {{SOURCE}}
    provider: mock
    workdir: {{WORKDIR}}
    scripts:
      {{SCRIPTS}}
```

A provider that resolves `list` and `move` but ships no template is still a
usable provider — `enroll` just cannot offer it, and says so with the reason.
`enroll` substitutes single-pass and never recursively, so a value containing
literal `{{X}}` text is emitted as-is; every substituted value — the
provider's own answers, and the engine's `{{SOURCE}}`/`{{WORKDIR}}` — is
encoded as a YAML-safe scalar first. `{{SCRIPTS}}` is the one exception: it
is `enroll`'s own rendering of the `agent:`/`script:` choice for every
distinct script name a stage asks for, spliced in verbatim and indented to
whatever column its placeholder sits at. A malformed template — an
unresolved placeholder, a prompt never named in `template:`, a duplicate or
reserved prompt name, a name shaped like a secret (`token`, `secret`,
`password`, `key`, `credential`, case-insensitively) — is a refusal naming
the file and the offending entry, never a partial draft: `enroll` prints
everything it collects, on stdout, in the clear, so a credential belongs in
the drafted source's `env:` by hand instead.

## 4. Transition order

Moving an item from stage A to stage B is always, in this order:

```
1. acquire the source's lock        (one item in flight per source)
2. move(item, B, blocked=false)     provider now says B and says the item is not
                                    blocked — before any work starts, so a crash
                                    leaves a truthful record
3. run B's named script             logs stream to the UI
4. on exit 0   → move(item, next, blocked=false) and release
   on exit 20  → move(item, B, blocked=true)     marked where it stands
   on failure  → count an attempt; leave it in B to be re-run, or once
                 maxAttempts is spent, move(item, B, blocked=true)
5. release the lock                 always, including on crash and timeout
```

Nothing in step 4 moves an item backwards. An item that stopped is left in the
stage it stopped in, and the mark is what keeps the scheduler off it — so when a
person clears the mark, the next listing shows an item sitting inside a stage
that runs a script, which is exactly what an interrupted job looks like, and it
is re-run from there. Unblocking costs one label and loses no context.

Step 2 before step 3 is deliberate: it is the same guarantee as marking an issue
`in-progress` before implementing it. If the process dies mid-stage, the provider
already reflects reality and the item is not handed out twice.

## 4a. Which item is next

The scheduler works the line **from its far end backwards**. Rungs, most
decisive first:

1. has anywhere to go at all — a marked item, a terminal stage, a queue with
   no exit, or an item sequenced behind something that has not reached the
   stage it wants to enter, is not in the running
2. **how far along it is**: the position, in the config's stage list, of the
   stage it is heading into. Further along wins
3. recovery — a stage that runs a script, found holding an item, is an
   unfinished job and is re-run
4. within one stage: the manual input order, then `priority`, then the
   source's own listing order — except a **terminal** stage, which is not
   scheduled at all and orders instead by `finishedAt` descending (falling
   back to listing order when it is empty or unparseable), so the done column
   reads newest-finished first and merges every source into one ledger

An item declaring `dependsOn` may enter a stage its dependency has already
entered, and no further. It may share that stage — a line where every follower
waited for its predecessor to *finish* would run one item deep and give away
the throughput that makes it a pipeline — but it may never pass it, and it may
not enter a stage its dependency is marked in: a mark means that station did
not finish, and putting a second item into it is the case this rule exists to
prevent. The chain needs no transitive closure: every link is enforced, so a
family sequences itself one edge at a time, and child 3 declares only its
immediate predecessor. An id absent from the listing, a self-edge, a cycle, or
a dependency sitting in a stage this config does not declare all fail open —
the edge is dropped, never treated as holding forever, because a typo in an
issue body must not be able to wedge the line.

A stage may raise that bar for itself with `dependenciesAt: <stage>`: an item
enters it only once every dependency has reached the named stage or gone past
it (and is not marked there). Sharing is the right default for stages that
only read their own item; it is the wrong one for the stage that builds on
the dependency's code. An item implemented while its dependency is still in
review is built on a branch that is not on `main`, and the pull request it
produces cannot merge on its own — so `dependenciesAt: merged` on the
implement stage is what makes "every issue is deliverable by itself" a rule
the line enforces rather than a hope the spec expresses. The hold carries
`until`, the stage the dependency has to reach, so the board and `run
-explain` can say "must reach merged" rather than "cannot enter in-progress".
The name must be a declared stage, checked at load.

Rung 2 is the point of the whole thing: finish an item before starting another.
Every half-finished item holds a worktree, a branch and an open pull request
that goes stale while the line starts something new, so a job one stage from
done is worth more than a job one stage from started — however urgent the
backlog looks.

Depth is read off the config because the config is already the line's order: a
stage with no `onSuccess` falls through to the next one declared. Walking
`onSuccess` instead could not measure anything, since a review that sends work
back to be implemented makes the graph a cycle.

A queue and the stage it feeds are one position, not two, because depth is the
stage being *entered*. That is what keeps rung 3 meaningful: an interrupted run
only ever ties with the queue directly behind it, and it wins that tie, so
nothing is left stranded wearing a status it is not in.

Rung 4 therefore decides *within* one stage. That is where a human lever
belongs — it is a choice about what to start next, and dragging a backlog card
cannot jump the queue past work already in flight. A terminal stage never
reaches rung 4 through `Pick` — rung 1 already excludes it from scheduling —
but `Order` still has to place it somewhere, and a finish time is the only
honest answer: the manual order and `priority` are levers over what runs
next, and a finished item does not run again.

## 4a2. A source may cost more than the stage assumes

A stage's `timeout:` is a statement about the work. The same work costs
different amounts in different repositories, so a source's script entry may
override it:

```yaml
scripts:
  review:
    agent: claude
    timeout: 75m        # this repo's suite alone runs for a quarter of an hour
```

Unset means the stage's, which is the normal case. Raising the *stage* to fit
the slowest repository is the wrong lever: it removes the guard everywhere it
was doing its job.

An inline `run:` stage cannot be overridden this way, by construction — it
exists for the case where every source does the same thing, so there is no
per-source entry to carry the number.

## 4b. The deadline

A stage with a `timeout:` tells its script when it will be killed:
`CONVEYOR_DEADLINE`, an RFC3339 instant in UTC, taken from the context that
enforces it rather than calculated a second time. **Unset when the stage has no
timeout** — a real configuration, and never to be read as a deadline that has
already passed.

An instant, not a duration, because a duration is only useful to something that
knows when it started. An agent sixty turns in cannot feel elapsed time and has
no reason to have kept count; a timestamp costs it one `date` to know exactly
where it stands.

What an adapter does with it is the adapter's business, and `agents/_deadline`
is what the ones here do: hand the agent an earlier deadline than the engine's —
`WRAP_UP_GRACE` seconds earlier, 300 by default — so the last minutes go on
landing work rather than being cut mid-verification.

Two properties of that brief are deliberate. It never tells an agent to hurry:
a review that returns green because the clock was running is worse than the
timeout it avoided, since the timeout at least marks the item and fetches a
person. And the agent's deadline is **derived**, never configured — one number
in `conveyor.yaml` moves both, where a budget written into a `PROMPT` would go
stale the first time that number changed, with nothing able to catch it.

Stopping unfinished is a supported outcome, not a failure: the run's session and
the item's worktree both carry to the next run, so it continues rather than
starting over.

## 5. Concurrency

```yaml
concurrency:
  perSource: 2      # items in flight per source
  perStage:  2      # items in flight in one stage
  global:    4      # items in flight anywhere

resources:          # what the work consumes, and how much of it there is
  claude:   1
  codex:    2
  ssh-prod: 5

stages:
  - name: in-progress
    script: implement
    resources: [claude]
```

**The scarce thing is never "a transition".** It is one account's quota, one
deploy host, one staging database — and those are shared across every stage and
every repository that reaches for them, which is exactly what `perSource`,
`perStage` and `global` cannot express. Counting transitions meant the number
had to be chosen for the agent runs, so a stage that only reads the GitHub API
queued behind them for no reason at all.

A stage lists what its work consumes; a transition takes one of each before it
starts and gives them back when it ends, and it takes **all or none** — holding
one while waiting for another is how two schedulers deadlock each other. A stage
naming nothing is bounded only by `global`, which is the right answer for the
cheap ones. A name with no limit under `resources:` is a load error, because a
silently unlimited resource is the failure the mechanism exists to prevent.

The same stage is Claude in one repository and Codex in the next, so a source
may replace the list for its own version of a script (`scripts.<name>.resources`);
`[]` there says it spends nothing, which is not the same as saying nothing.

`perSource` above 1 is safe only because an item works in **its own git
worktree**, not in the source's checkout — see `agents/_worktree`. Two agents in
one directory overwrite each other; two agents in two worktrees of one
repository share only the object store, which git already makes safe. Where the
worktrees live, when they are made and when they are removed is entirely the
adapter's business: the engine has no concept of a worktree and must not gain
one.

It is still not unbounded. Scripts that run in the source's own checkout — a
`refine` that only reads, a `deploy` that ships from it — are bounded by
`perSource` like everything else, and a deploy is bounded to one at a time by
`perStage` anyway.

A stage script may spawn as many subagents as it likes internally — that is
invisible to the engine.

### What a stage script's credentials actually reach

Three facts worth being explicit about, because a worktree is easy to
over-read as a security boundary when it is only a filesystem one:

- **A stage script runs as the conveyor user, with that user's whole ambient
  environment and credentials** — the same `gh` auth, the same SSH keys, the
  same cloud credentials any other process that user runs would have. The
  engine adds nothing to that and takes nothing away from it.
- **A worktree isolates a checkout, not credentials, network or host access.**
  It is what lets two items in one repository run at once without one agent's
  uncommitted work colliding with another's, and nothing more. It does not stop
  a script from reading another repository, calling an external API, or acting
  outside the worktree entirely.
- **The `approve` gate (§"Merging is a gate, not a judgement" in README.md) is
  a workflow control, not an enforced boundary.** It waits for checks, review
  threads and a quiet PR before merging — but an agent running with the same
  credentials `approve` itself uses could merge directly, or push to any other
  branch it can reach, without going through the gate at all. What stops that
  from happening is the prompt an adapter writes and the model that reads it,
  not a permission the engine withholds.

None of this is a defect to fix here: it is what "the extension is the
scripts" (see the top of this document) means in practice, and a reader
relying on worktrees or the approve gate as a sandbox should not.

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

Comparing days, not timestamps: a run directory whose day is strictly before
the cutoff's UTC date is deleted, one on or after it is kept, and a whole
expired day can be skipped without reading a single `meta.json` — conservative
by up to 24 hours in the safe direction. The sweep runs once at startup and
then daily at `sweepAt`, in the process's own local time zone, and never under
`-watch`: a server that only observes must not delete anything either.

Three exceptions, and they matter:

- **A run whose `meta.json` outcome is still `running` is never deleted**,
  regardless of age. Nothing this process started is alive to have finished
  writing that record honestly, and a killed run is exactly the one someone
  needs to read afterwards.
- **The most recent failed, blocked or timeout run of an item that is still
  marked is never deleted.** Retention must never delete the evidence for the
  thing currently asking for attention. A run only ever explains a mark in the
  stage it ran in: the history walk is newest-first across the whole store, so
  without that restriction the newest failure anywhere answered for a mark it
  had nothing to do with, and the card sent its reader to an unrelated log.
- **The most recent successful move that landed a currently-listed item in the
  stage it currently occupies is never deleted.** Sweeping it would blank the
  stage-age chip for an item still sitting right there.

Both item-based pins are evaluated against the board's *current* items only —
an expired run belonging to nothing on the board today is swept normally. A
day directory left empty by the sweep is removed; one still holding a pinned
run is not. Pinned runs are reported in the sweep log, named individually, so
they cannot pile up unnoticed.

### Failure is a first-class state

An item whose last run exited non-zero is **needs-attention**, shown red, and
surfaced above everything else in the UI. That state carries the run id, so one
click reaches the log that produced it. In v1 the answer to "what needs me?" is
the primary question the UI answers — more important than showing progress.

`20` (blocked) and other non-zero exits are both red, but they mean different
things and the UI says which: blocked is *"a human must decide"*, failure is
*"this broke, and it may just need retrying"*.

There is no blocked column. A marked item is drawn in the stage it stopped in,
and that stage's header counts how many it is holding — because blocked is
something an item *is*, not somewhere it went.

**The mark carries a kind and a reason.** The kind is one word — `decision`,
`limit`, `worktree`, `error` — and it is all the board puts on a card, because a
column of paragraphs is a column nobody can read across. The reason is the whole
of it and lives one click away. A script names its own kind in
`$CONVEYOR_RESULT` (`{"blocked": true, "kind": "...", "reason": "..."}`); when
it names none, the run record supplies one. The engine never interprets the
word, only its shape: too long or punctuated and it is dropped, because it is
about to become a label on someone's issue tracker.

**The mark carries its reason, and the item carries the mark.** The provider is
the authority on *whether* an item is marked, and it should be the authority on
*why* too: a provider that can record the reason against the item writes it
there when the mark goes on and reads it back on the next listing
(`blockReason`/`blockKind`, §1). That is what the board shows, because it is
the only account that is still true after a restart, after retention has swept
the run, and for a mark this pipeline never made. A red card that cannot say
what it is waiting for sends the reader to the logs, which is the trip the mark
exists to save.

Run history is the fallback, not the source. It answers when the listing says
nothing — a provider with nowhere to record a reason, a mark set by hand — and
only a run of the stage the item is *in*: the walk is newest-first across the
whole store, and without that restriction the newest failed run anywhere
explained a mark it had nothing to do with. When both exist and disagree, the
listing wins; the run is still worth reading for what only it knows — whether
the stop was a question, the session that asked it, the options it offered —
but only while the two are describing the same stop.

**A script may say what it is waiting for.** Exiting 10 with
`{"waiting": {"until": "<RFC 3339>", "why": "…"}}` in `$CONVEYOR_RESULT` says
"nothing to do yet, and here is what would change that". The engine stores it
against the item and hands it to the board, which draws a live countdown; it
reads neither field and acts on neither. `until` is optional — plenty of waits
have no deadline — and a wait without one says what it is for and draws no
clock. It exists because a resting item and a stuck one look identical
otherwise: both sit still, and only the script that stopped knows which.

**A stop is either a question or a condition**, and the script says which. A
condition — out of quota, a dirty checkout, a network that was down — may have
passed by the time anyone looks, so retrying it is reasonable. A question has
not passed: nobody answered it by waiting. A script that stopped on a question
writes `"asked": true` beside its reason; everything else is a condition, which
is the safe default, because a condition wrongly retried costs one run and a
question wrongly cleared costs the run *and* asks you the same thing again.
A question may also carry `"questions": [...]` — the AskUserQuestion shape
(`header`, `question`, `options[{label, description}]`, `multiSelect`). The
engine passes it through on the block untouched, like `session`; the board
turns it into a modal and sends the choices back as the `answer`.

The engine reads the flag and never the word beside it. Conditions are cleared
in bulk — `Unblock all`, `retryStalled`, an agent's quota returning. **A question
is never cleared in bulk**, and is not counted towards a stall either: a board
holding nothing but questions is stopped on purpose. Only answering it on its
own card takes it off.

**`Unblock all` also leaves standing whatever the sequencing rule (§4a rung 1)
would hold anyway.** A marked item whose dependency has not yet reached the
stage it is marked in gets no provider write from this button — clearing the
mark would only have the scheduler refuse it again on the next pass, spending a
write to learn what `pipeline.Deps` already knows. The question this asks is
"would this item be held if it were not marked", never `pipeline.Target`, which
refuses a marked item on its own mark before the sequencing gate is ever
reached — and it is decided from `dependsOn` and the current listing alone,
never from the mark's own kind, because a mark's vocabulary belongs to the
script that wrote it. The response reports this count separately
(`heldByDependencies`), distinct from the questions the button already leaves
standing (`waitingOnYou`); an item that is both is counted once, under
`waitingOnYou`. The one gap this accepts: a dependency the listing cannot see
at all (an un-onboarded issue, another repository) is not something the engine
can compute a hold for, so that item is still cleared here — `agents/_deps`
remains the backstop that re-marks it at `implement` time, before the worktree
and before any model run.

**Only a person clears a mark**, with three exceptions. The first two are the
outside world coming back rather than a decision being made; the third is the
only one that reads the reason first.

When *every* item is marked and the line cannot move at all, `retryStalled:`
clears them on an interval and lets it try again. The guard is "everything" on
purpose: while one item can still move, a mark is a decision waiting on a human,
and clearing it spends an agent run to be told the same thing. A total stall is
a different animal — it is almost always the outside world.

And when an agent's `status` script goes from `limited` back to `ok`, the
conditions of kind `limit` are cleared — those and no others. A `limit` says, in the
script's own words, that nothing was wrong with the item and the agent had no
quota left; the quota returning is the whole of the answer, and it arrives on a
schedule the agent itself reports. Leaving those for someone to clear by hand is
a night of the line standing still for a reason that expired at 3am. A
`decision` mark is untouched, because no amount of waiting produces an answer
only a person has.

The third is a **doctor** script (§3) clearing a mark it examined itself, one
item at a time, as part of a sweep a person started (`POST /api/doctor`). It
differs from the first two in kind, not just in trigger: `retryStalled` and a
quota's return clear a mark blind, on the strength of "the outside world may
have changed"; a doctor reads the mark's kind and reason, and that item's own
run history, before deciding. The one invariant that does not move for it
either: a `decision` mark is never something a doctor may clear — the adapter
holds that rule, checked by a selfcheck rather than by trust, because the
engine itself still never reads the word beside the flag.

## 5a. Answering a stop

A marked item is handed back with an optional reply: `POST
/api/items/{id}/unblock` takes `{"answer": "…"}`, and answering and unblocking
are deliberately one gesture. An answer recorded against an item nobody handed
back is a note in a drawer — the stage that would read it never runs.

The reply reaches the next run of that stage in its stdin, as `answer`, together
with `session` — whatever handle the agent left in `$CONVEYOR_RESULT` when it
stopped (`{"blocked": true, …, "session": "…"}`). Both are opaque to the engine,
which stores them, hands them back once, and never reads either: what resuming
*means* differs per agent, and some cannot resume at all. An adapter with both
says the answer into the conversation that asked it; an adapter with only the
answer leads its original prompt with it and starts fresh, which costs the
rediscovery but never costs the answer.

An answer is spent when it is handed over. A second run of the same stage is not
a second reply to one question.

## 5b. Actions: the manual override

A stage may declare buttons:

```yaml
- name: approving
  script: approve
  actions:
    - name: merge-now
      label: Merge now
      confirm: Merge without waiting out the quiet period?
```

Pressing one arms a single word for **the next run of that stage for that
item** — delivered in `$CONVEYOR_MANUAL` and as `"manual"` on stdin, and spent
by that run — and clears the item's deferral so the run happens now rather than
at the next listing.

That is the whole of it, and the smallness is the point. An action cannot move
an item, choose a stage, or skip a script: it hands one word to the script,
which decides what — if anything — it means. A gate a person can open stays a
gate the script still owns. `approve` reads `merge-now` and takes it to mean
"stop giving review more room"; every other rule in that gate still has to
pass, so a person cannot press a red build into main. The engine never learns
what any action means, exactly as it never learns what a `kind` means.

Pairs with the waiting report in §6: one says what the script is waiting for,
the other lets a person say "not any more".
