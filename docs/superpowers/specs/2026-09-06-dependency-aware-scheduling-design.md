# Dependency-aware scheduling

A `prioritising` stage records the order of a sequenced family of issues, and
the scheduler enforces it: an item may never get further along the line than
what it depends on.

Status: design, approved 2026-09-06. Not yet planned or implemented.

## The problem

Issues arrive in sequenced families — "Improvement child 1..5" in this repo,
"How to play, batch 5 (final)" in midgame, the eight `Live duels:` issues in
quesshi. The part number is in the title and nothing reads it.

Nothing records the order either. This repo's #33–#37 carry no `priority:`
label and no dependency line, so in `pipeline.better` they tie on every rung —
depth, recovery, manual order, priority — and fall through to the last one,
`pos`, the source's own listing order. `providers/github/list.sh` lists open
issues newest-first, so the line works **child 5 before child 1**.

`agents/_deps` already exists and already does the right thing for a single
item: it reads `Depends on #N` from the body, stops `implement` before the
worktree with a `dependency` mark, and `agents/claude/doctor` clears it once
every named issue is closed. Nobody writes those lines. `/refine` does not, and
`conveyor.yaml` has no stage between `refining` and `ready` — despite `ready`'s
own comment claiming items there are "refined and prioritised".

Two gaps, then. Nothing writes the sequence down, and what reads it can only
answer a weaker question ("is that issue still open?") in one stage.

## The rule

> An item may enter a stage its dependency has already entered, and no
> further. It may share that stage; it may never pass it; and it may not enter
> a stage its dependency is marked in.

Stated over depths, where depth is the position in the config's stage list —
the same number rung 2 of CONTRACTS §5 already uses. For an item `B` with
dependency `A`, and a candidate target stage `T`:

```
B may enter T  iff  depth(T) <= depth(stage(A))
                    and not (depth(T) == depth(stage(A)) and A is marked)
```

**Why sharing a stage is allowed.** Making every follower wait for its
predecessor to *finish* would run the line one item deep and give away the
throughput that makes it a pipeline. Child 2 implementing while child 1
reviews is the behaviour worth having. The cost is real and is accepted: two
siblings implementing at once branch from the default branch independently,
so the second does not see the first's work and they meet at merge time.
`agents/claude/approve` already resolves conflicts mechanically, and this will
exercise it.

**Why the blocked clause is separate.** A dependency marked in a stage has not
finished that stage. Letting a follower walk into it would put two items in a
station where the first one is stuck, which is the case the rule exists to
prevent.

**Transitivity is free.** Child 3 declares only `Depends on #34`. It never
needs to know about #33, because child 2 is itself held behind #33 — every link
is enforced, so the chain enforces itself. There is no closure to compute and
nothing to keep consistent.

## Item contract

`model.Item` gains one field:

```go
// DependsOn names the items this one is sequenced behind, by item ID. The
// engine never parses it out of anything: the provider translates its own
// vocabulary ("Depends on #33" in a GitHub issue body) into IDs, the same
// way it already does for Priority and Blocked.
//
// An ID absent from the listing is ignored, never held forever — a
// dependency on an un-onboarded issue, another repository, or a typo must
// not stop the line.
DependsOn []string `json:"dependsOn,omitempty"`
```

`docs/CONTRACTS.md` §1 gains the field in the item schema.

Three consequences, all deliberate:

- **A closed dependency stops gating, for free.** `list.sh` already lists
  closed issues it labelled; they land in a terminal stage, which is maximum
  depth, so nothing can overtake them. No special case in the engine.
- **The engine gates only on onboarded siblings.** An issue wearing no
  conveyor label is not in the listing, so a `Depends on #99` naming one is
  ignored. This is a narrowing from `agents/_deps`, which asks `gh` directly
  and holds on any open issue. Both survive, with a clean division: the
  **engine** sequences the line it can see, and **`_deps`** still catches
  facts about the outside world at implement time, before the worktree.
- **Unknown, self- and cyclic edges fail open.** Dropping the edge keeps the
  line moving. A typo must never wedge the pipeline — the same principle
  `_deps` already states. A dependency sitting in a stage the config does not
  declare is the same case: the edge is dropped, not treated as depth 0, which
  would silently hold every follower at the front of the line.

## Provider

`providers/github/list.sh` fills `dependsOn` in the same `jq` map that already
produces `priority` and `blocked`, emitting `"<source>:<ref>"` so the value
joins directly against `Item.ID`.

The body syntax is `agents/_deps`' existing one, unchanged: a line beginning
with `depends on` / `blocked by` / `blocks on` / `requires` / `after`, cut at
its first mid-line sentence break before numbers are taken out of it.

**The parse is stated twice, on purpose.** `agents/` and `providers/` are
separately-resolvable roots and providers must not reach into agents. The two
copies answer different questions — one asks the listing where a sibling is,
the other asks `gh` whether an issue is open — and coupling them to share a
regex would be worse than the duplication. The drift is real, so **one shared
corpus of body fixtures lives in both selfchecks**, and a change to either
regex fails the other's test.

## Scheduler

One value, built once per pass:

```go
// Deps is what the ladder needs to know about the rest of the listing: where
// every item sits right now, and whether it is marked there.
//
// Built from the FULL listing, never from a filtered slice. A dependency that
// is itself running or marked is precisely the one that must still hold its
// followers — and precisely the one server.go's `free` filter has already
// dropped before Pick ever sees it.
type Deps struct{ ... }

func NewDeps(cfg *config.Config, items []model.Item) Deps
```

Signatures become `Target(cfg, it, d)`, `Pick(cfg, items, order, d)`,
`Order(cfg, items, order, d)`. Call sites build `d` from the full listing:
`server.go:497`, `server.go:878`, `server.go:1081`, `server.go:1173`,
`doctor.go:80`, and `cmd/conveyor/main.go:327`.

**The gate lives in `Target`**, which is rung 1 — "has anywhere to go at all".
One place, three benefits:

- A held item is `workable: false`, so it falls to `better`'s existing tail,
  where non-terminal unworkable items already keep the rungs `Pick` would use
  — manual order, then priority, then listing order. Held children therefore
  keep their relative order inside the `ready` column and simply sit behind
  the unheld ones.
- The **drag endpoint refuses for free**. `server.go:1173` already routes a
  false into `whyStuck`, which gains the sentence: *held behind conveyor:33,
  which is blocked in review*.
- `Deps` exposes the prose separately rather than widening `Target`'s return.
  `whyStuck` is the only caller that needs a sentence.

**`retryStalled` is correct with no change.** A held item is not moving, so
five children held behind a child 1 marked `limit` makes `moving == 0` at
`server.go:497` and the hourly sweep clears it — the line was genuinely
stopped. A *running* dependency still counts as moving, so there is no
spurious stall. A dependency marked `decision` leaves the `held` slice empty
and nothing fires, which is right: only a person has that answer.

`docs/CONTRACTS.md` §5 gains the gate at rung 1, and `CLAUDE.md` gains the
invariant.

## Board

**A held item is not a blocked item.** *Blocked is a mark on the item*; held is
a conclusion the scheduler draws about the line. Nobody has to clear it and it
disappears on its own when the dependency moves. Collapsing the two would make
the board lie about what a person needs to do.

`State` gains a sibling to the `Blocks` and `Times` maps it already carries:

```go
// Held is why an item with somewhere to go is not going there: it is
// sequenced behind something that has not reached that stage yet. A sibling
// of Blocks, and deliberately not the same thing.
Held map[string]Hold `json:"held,omitempty"`
```

`internal/server/web/index.html` gets an `.item.held` class styled off the
existing `.item.blocked.waiting` grey — the tone already reserved for
conditions that pass on their own — carrying one line, *behind #33*. No mark
chip, not red.

Cycle warnings go into `State.Warnings`, which is collected but **not drawn by
the page yet**: that is open issue #43. They will land correctly and stay
invisible until #43 ships.

## The prioritising stage

A new stage between `refining` and `ready`, in `conveyor.example.yaml` and in
`~/codes/conveyor.yaml`:

```yaml
  - name: refining
    script: refine
    onSuccess: prioritising     # was: ready

  - name: prioritising
    script: prioritise
    timeout: 15m
    onSuccess: ready
```

with `prioritising=conveyor:prioritising` in every source's `STAGE_LABELS`, the
label added to `providers/github/onboard.sh`, and a `prioritise:` entry in each
source's `scripts:` block naming the agent, a cheap model and a small turn
budget — because how the work happens is never in the config.

`agents/claude/prioritise` runs its agent **once per item, ever**, and always
exits 0. It has no waiting to do: the engine holds now. It writes back to the
issue:

- `Depends on #N` — the **immediate** predecessor only, since the chain is
  transitive through each sibling;
- `<!-- conveyor:sequence 3/5 after:#34 -->`, or `standalone` — mirroring the
  `<!-- conveyor:approve:conflict … -->` marker `approve` already uses to make
  a model run happen once rather than every poll;
- exactly one `priority:pN` label if the item has none.

The marker is read on entry: an item that already carries one is passed
straight through with no model run.

**This is the one place the design brushes the thesis** in `CLAUDE.md` — *do
not add features that let the model decide the flow*. The defence has to be
written down or the next reader deletes the stage: the model writes one
durable, human-readable line into the issue; the **engine** reads only that
line, deterministically; a person can correct or delete it and the pipeline
obeys. The model stays a worker inside one stage, which is the thesis, not an
exception to it.

The priority label matters independently of sequencing. Almost no issue in the
enrolled repositories carries one today, which is why `better` ties all the way
down to listing order even for unrelated work.

## Testing

`internal/pipeline` is where the rule lives, so that is where it is proven.
Table cases over `NewDeps` + `Target`:

- follower behind, level with, and ahead of its dependency
- dependency marked at the shared stage
- dependency closed and terminal
- unknown ID, self-edge, 2-cycle, 3-cycle — all fail open
- a three-link chain, proving transitivity needs no closure
- `Pick` given a `free` subset that excludes the dependency, proving `Deps` is
  built from the full listing

Then `agents/claude/prioritise-selfcheck` against a stubbed `gh` for the
recording half — numbered family, unnumbered family, standalone, marker
present means no model run — following `approve-selfcheck`, and
`agents/mock/prioritise` so `conveyor tick -c conveyor.example.yaml` still
drains.

## Build order

Two halves, and the first is useful on its own — worth keeping separable so a
plan can land and prove one before starting the other.

1. **The gate.** Item field, provider, scheduler, board, docs, tests. This
   alone makes every `Depends on #N` line already written by hand, or by
   `/refine`, enforceable across the whole line rather than in `implement`
   only. Nothing about it needs the new stage to exist.
2. **The stage.** `prioritising`, its adapters, the config and the labels.
   This is what starts writing those lines without a person.

## Migration

Items already sitting in `conveyor:ready` never pass through the new stage, so
they carry no `Depends on` line and nothing gates them. One-off: relabel the
affected issues to `conveyor:prioritising` and let them flow through. For this
repo's #33–#37 that is five `gh issue edit` calls.

## Files

| File | Change |
| --- | --- |
| `internal/model/model.go` | `Item.DependsOn` |
| `internal/pipeline/pipeline.go` | `Deps`, `NewDeps`, the gate in `Target`, signatures |
| `internal/pipeline/pipeline_test.go` | the table above |
| `internal/server/server.go` | `State.Held`, `Hold`, `whyStuck`, four call sites |
| `internal/server/doctor.go` | one call site |
| `internal/server/web/index.html` | `.item.held` |
| `cmd/conveyor/main.go` | one call site |
| `providers/github/list.sh` | `dependsOn` |
| `providers/github/onboard.sh` | `conveyor:prioritising` |
| `agents/claude/prioritise` | new |
| `agents/claude/prioritise-selfcheck` | new |
| `agents/mock/prioritise` | new |
| `conveyor.example.yaml` | the stage, per-source `prioritise:` |
| `~/codes/conveyor.yaml` | the same, for the five live sources (outside the repo) |
| `docs/CONTRACTS.md` | §1 item schema, §5 rung 1 |
| `docs/DESIGN.md`, `CLAUDE.md` | the invariant and the thesis note |

## Rejected alternatives

**The provider decides and marks the item.** No engine change: `list.sh` works
out that an item is ahead of its dependency and emits `blocked: true, kind:
dependency`. It needs the provider to know the pipeline's stage *order*, which
it does not have — `STAGE_LABELS` is a mapping, not a sequence. Worse, it
writes a mark, and *only a person clears a mark*; a mark that appears and
vanishes on its own every poll destroys the one thing marks mean.

**The scripts decide, and the engine exports the stage list.**
`CONVEYOR_STAGES` in the environment, and `_deps` grows a depth comparison
every adapter calls. But the engine writes provider state *before* running a
stage, so the item has already moved into `in-progress` by the time the script
refuses — it lands, then bounces, wearing a mark. The check would also be
duplicated across `implement`, `review` and `approve`, and it would not cover
the drag endpoint at all.

**Serialising on closure.** A follower waits until its dependency is closed —
what `agents/_deps` does today. Zero conflict risk and no engine change, but
five children become five full cycles end to end and the line runs one item
deep, which is most of the throughput of a pipeline given away.
