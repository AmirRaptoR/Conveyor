# Dependency-Aware Scheduling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An item may enter a stage its dependency has already entered, and no further — it may share that stage, never pass it, and never walk into a stage its dependency is marked in.

**Architecture:** The GitHub provider translates `Depends on #33` in an issue body into an item ID on `Item.DependsOn`; the engine never sees a `#`. The scheduler builds one `pipeline.Deps` view per pass from the full listing and refuses an overtaking target at rung 1 of the ladder (`Target`), which also makes the drag endpoint refuse for free. A new `prioritising` stage between `refining` and `ready` runs an agent once per item to write those dependency lines without a person.

**Tech Stack:** Go 1.27 (at `~/.local/go/bin`, already on PATH via `.zshrc`), bash + `jq` for providers and agent adapters, vanilla HTML/CSS/JS for the board (`internal/server/web/index.html`, `go:embed`ed).

**Spec:** `docs/superpowers/specs/2026-09-06-dependency-aware-scheduling-design.md`

## Global Constraints

- **The engine never learns GitHub's vocabulary.** No `#`, no issue numbers, no label names in `internal/`. The provider translates; the engine compares opaque IDs. (CLAUDE.md, "A source names a `provider:`".)
- **Fail open, always.** An unknown ID, a self-edge, a cycle, or a dependency in a stage the config does not declare drops the edge and lets the line move. A typo must never wedge the pipeline.
- **Blocked is a mark on the item; held is not.** Nobody clears a hold — it disappears when the dependency moves. The board must never draw one as the other.
- **`stdout`/`stderr` are logs and are never parsed.** Structured data comes back only via `$CONVEYOR_RESULT`.
- **Every label this pipeline owns begins with `LABEL_PREFIX`** (`conveyor:`, a provider param, never a config key).
- Item ID convention is `"<source>:<ref>"`.
- Build and test: `export PATH="$HOME/.local/go/bin:$PATH"` then `go build ./... && go test ./...`.
- Do not run `git checkout -b` in `~/codes/Conveyor`. `refine` and `deploy` run in the source's own checkout, and a live pipeline is enrolled on this repo; leaving it on a feature branch makes those stages operate on the wrong code. Commit to the current branch.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `internal/model/model.go` | `Item.DependsOn` — provider-supplied, opaque to the engine |
| `internal/pipeline/deps.go` | **new.** `Deps`, `NewDeps`, cycle dropping, `Held` — everything about the dependency graph, kept out of `pipeline.go` which is already 736 lines |
| `internal/pipeline/deps_test.go` | **new.** Table tests for the graph and the gate |
| `internal/pipeline/pipeline.go` | `Target`/`rate`/`Pick`/`Order` take a `Deps`; the gate is one call in `Target` |
| `internal/server/server.go` | `State.Held`, `whyStuck`, four call sites |
| `internal/server/doctor.go` | one call site |
| `internal/server/web/index.html` | `.item.held`, the hold chip, the warnings strip |
| `cmd/conveyor/main.go` | one call site |
| `providers/github/list.sh` | parse the body into `dependsOn` |
| `providers/github/selfcheck.sh` | the shared body-fixture corpus |
| `providers/github/onboard.sh` | the `conveyor:prioritising` label |
| `agents/claude/prioritise` | **new.** Records the sequence, once per item |
| `agents/claude/prioritise-selfcheck` | **new.** Stubbed `gh` |
| `agents/mock/prioritise` | **new.** So the demo pipeline still drains |
| `conveyor.example.yaml` | the shipped stage list |
| `~/codes/conveyor.yaml` | the live stage list (outside the repo, machine state) |
| `docs/CONTRACTS.md`, `docs/DESIGN.md`, `CLAUDE.md` | the contract and the invariant |

`deps.go` is a new file rather than more of `pipeline.go` because the graph is a self-contained unit with one job and a clear interface (`NewDeps` in, `Held` out), and `pipeline.go` is already the largest file in `internal/`.

---

## Task 1: `dependsOn` on the item and in the listing

**Files:**
- Modify: `internal/model/model.go:41-45`
- Modify: `providers/github/list.sh:112`
- Modify: `providers/github/selfcheck.sh:44-70` (stub fixtures), and append checks after line 97
- Modify: `docs/CONTRACTS.md:34` (item schema)

**Interfaces:**
- Consumes: nothing.
- Produces: `model.Item.DependsOn []string` — item IDs in `"<source>:<ref>"` form, empty when the body declares nothing. Every later task reads this field.

- [ ] **Step 1: Write the failing test**

In `providers/github/selfcheck.sh`, add three fixtures to the `data=` array inside the `gh` stub heredoc (after the `number:19` entry, keeping the JSON commas correct):

```json
 {"state":"OPEN","number":23,"title":"Improvement child 3","body":"Depends on #21\n",
  "labels":[{"name":"conveyor"}],
  "url":"https://example.test/23","assignees":[]},
 {"state":"OPEN","number":25,"title":"Two at once","body":"Blocked by: #21, #23\n",
  "labels":[{"name":"conveyor"}],
  "url":"https://example.test/25","assignees":[]},
 {"state":"OPEN","number":27,"title":"Not a dependency","body":"Related: #9\nthe record the board depends on: x\nDepends on: ADR 9 (merged). Blocks: #12, #13 depend on this one\n",
  "labels":[{"name":"conveyor"}],
  "url":"https://example.test/27","assignees":[]},
 {"state":"OPEN","number":29,"title":"Mid-line, not line-start","body":"Part of the live duel epic (#8). Depends on #21, #23 and #25, all still open.\n",
  "labels":[{"name":"conveyor"}],
  "url":"https://example.test/29","assignees":[]},
```

Then append these checks immediately after the `check "an unmarked issue is not blocked"` block (around line 97):

```bash
# The body is the only place a sequence is written down, and the provider is
# the only thing that reads it: the engine never learns what "#21" means.
check "a dependency line becomes an item id" \
	"midgame:21" "$(jq -r '.[] | select(.ref == "23") | .dependsOn | join(",")' "$tmp/out.json")"
check "two numbers on one line become two ids" \
	"midgame:21,midgame:23" "$(jq -r '.[] | select(.ref == "25") | .dependsOn | join(",")' "$tmp/out.json")"
# Three traps in one body, and none of them is a dependency: a "Related:" line,
# the word "depends" mid-sentence, and a sentence whose own dependency clause
# ends before it starts — "Blocks: #12, #13 depend on this one" describes who
# depends on IT, in the sentence right after "Depends on: ADR 9 (merged)."
# agents/_deps splits into sentences for exactly this reason.
check "prose that merely says 'depends' is not a dependency" \
	"" "$(jq -r '.[] | select(.ref == "27") | .dependsOn | join(",")' "$tmp/out.json")"
check "an issue declaring nothing has no dependencies" \
	"" "$(jq -r '.[] | select(.ref == "19") | .dependsOn // [] | join(",")' "$tmp/out.json")"
# The declaring sentence is reached only after an earlier, unrelated one on
# the same line — the case a line-start anchor alone would miss, and the one
# both parsers have to keep agreeing on as they evolve.
check "a mid-line declaration is read, not just a line-start one" \
	"midgame:21,midgame:23,midgame:25" "$(jq -r '.[] | select(.ref == "29") | .dependsOn | join(",")' "$tmp/out.json")"
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `./providers/github/selfcheck.sh`
Expected: FAIL on all five new checks — `want: midgame:21`, `got:` (empty), because `list.sh` emits no `dependsOn` field at all.

- [ ] **Step 3: Implement the parse**

In `providers/github/list.sh`, inside the `jq` object, immediately after the `priority:` line (line 112), add:

```jq
				# The sequence, translated out of GitHub's vocabulary and into
				# item ids — the engine must never see a "#". Same syntax
				# agents/_deps reads, and stated twice on purpose: providers
				# and agents are separately-resolvable roots, and coupling
				# them to share a regex would be worse than the duplication.
				# The shared fixture corpus in both selfchecks is what keeps
				# the two from drifting.
				#
				# The body is split into sentences — at a line break, or at
				# "." / "!" / "?" followed by whitespace — before a sentence
				# is checked for a keyword, so a declaration reached only
				# after an earlier, unrelated sentence on the same line is
				# still read, and a sentence that merely shares a line with a
				# declaration is not. "depends on" / "blocked by" / "blocks
				# on" match anywhere in the sentence; "requires" / "after" are
				# ordinary English words, so they match only where the
				# sentence opens (after markdown noise). Only the numbers
				# from the keyword onward, within that sentence, are taken —
				# so "Depends on: ADR 9 (merged). Blocks: #12, #13 depend on
				# this one" donates nothing: the first sentence's own keyword
				# ends before #12/#13, and the second sentence's "depend on"
				# (no s) is not a keyword of its own.
				#
				# The anchored branch is checked first: when a sentence opens
				# with "requires"/"after", that keyword is always the
				# earliest possible match in it, so the cut is taken there
				# even if "depends on"/"blocked by"/"blocks on" also occurs
				# later in the same sentence ("Requires #4, blocked by #12"
				# donates both #4 and #12). Only a sentence that does *not*
				# open with the anchored pair falls through to the
				# phrase-anywhere cut ("This requires #4, but depends on #12"
				# donates #12 only — "requires" is mid-sentence here, not a
				# declaration of its own).
				dependsOn:  ([
					(.body // "")
					| [splits("\n")]
					| map(splits("(?<=[.!?])[ \t]+"))
					| .[]
					| if test("^[\\s*_~`>+-]*(requires|after)\\b"; "i") then
						sub("^[\\s*_~`>+-]*(?:requires|after)\\b"; ""; "i")
					elif test("\\b(depends on|blocked by|blocks on)\\b"; "i") then
						sub("^.*?\\b(?:depends on|blocked by|blocks on)\\b"; ""; "i")
					else
						empty
					end
					| scan("#[0-9]+")
				] | map(ltrimstr("#")) | unique | map("\($source):\(.)")),
```

- [ ] **Step 4: Run it to make sure it passes**

Run: `./providers/github/selfcheck.sh`
Expected: PASS — all five new checks `ok`, and every pre-existing check still `ok`.

- [ ] **Step 5: Add the model field**

In `internal/model/model.go`, immediately after the `Blocked bool` field (line 41) and before `Raw`:

```go
	// DependsOn names the items this one is sequenced behind, by item ID. The
	// engine never parses it out of anything: the provider translates its own
	// vocabulary ("Depends on #33" in a GitHub issue body) into IDs, exactly
	// as it already does for Priority and Blocked.
	//
	// An ID absent from the listing is ignored rather than held forever — a
	// dependency on an un-onboarded issue, another repository, or a typo must
	// not stop the line. agents/_deps still catches those at implement time,
	// which is where a fact about the outside world belongs.
	DependsOn []string `json:"dependsOn,omitempty"`
```

- [ ] **Step 6: Document the field**

In `docs/CONTRACTS.md`, in the item schema around line 34, add after the `priority` line:

```
  "dependsOn": ["midgame:33"],  // items this one is sequenced behind, by id.
                                // The provider translates its own vocabulary;
                                // the engine only ever compares ids.
```

- [ ] **Step 7: Verify it compiles and nothing regressed**

Run: `export PATH="$HOME/.local/go/bin:$PATH" && go build ./... && go test ./...`
Expected: build succeeds, all packages `ok`.

- [ ] **Step 8: Commit**

```bash
git add internal/model/model.go providers/github/list.sh providers/github/selfcheck.sh docs/CONTRACTS.md
git commit -m "feat(provider): translate dependency lines into item ids

The engine must never see a '#'. list.sh reads the same body syntax
agents/_deps reads and emits item ids on Item.DependsOn.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MFdzEwdmWA6MVYuTUTAfqe"
```

---

## Task 2: The dependency graph

**Files:**
- Create: `internal/pipeline/deps.go`
- Create: `internal/pipeline/deps_test.go`

**Interfaces:**
- Consumes: `model.Item.DependsOn` (Task 1).
- Produces:
  - `type Hold struct { By, Stage, Target string; Blocked bool }`
  - `type Deps struct { ...unexported...; Cycles []string }`
  - `func NewDeps(cfg *config.Config, items []model.Item) Deps`
  - `func (d Deps) Held(it *model.Item, target string) (Hold, bool)`
  - The zero `Deps{}` holds nothing — every call on it returns `false`. Task 3 relies on this so existing tests that pass no graph keep passing.

- [ ] **Step 1: Write the failing test**

Create `internal/pipeline/deps_test.go`:

```go
package pipeline

import (
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// line is the pipeline these tests reason about: five stages, the last
// terminal, in the order the config declares them. Depth is that position.
func line() *config.Config {
	return &config.Config{Stages: []config.Stage{
		{Name: "backlog", OnSuccess: "ready"},
		{Name: "ready", OnSuccess: "in-progress"},
		{Name: "in-progress", Script: "implement", OnSuccess: "review"},
		{Name: "review", Script: "review", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
}

func item(id, stage string, deps ...string) model.Item {
	return model.Item{ID: id, Stage: stage, DependsOn: deps}
}

func marked(it model.Item) model.Item { it.Blocked = true; return it }

// The rule, stated as a table. "A" is the dependency; "B" is what depends on
// it and wants to enter `target`.
func TestHeldNeverLetsAFollowerPassItsDependency(t *testing.T) {
	for _, tc := range []struct {
		name    string
		a       model.Item
		target  string
		want    bool
		wantBy  string
	}{
		{
			name:   "behind: the dependency is further along, so the move is free",
			a:      item("s:1", "review"),
			target: "in-progress",
			want:   false,
		},
		{
			name:   "level: a follower may share the stage its dependency is in",
			a:      item("s:1", "in-progress"),
			target: "in-progress",
			want:   false,
		},
		{
			name:   "ahead: a follower may never pass its dependency",
			a:      item("s:1", "ready"),
			target: "in-progress",
			want:   true,
			wantBy: "s:1",
		},
		{
			name:   "marked at the shared stage: not into a station that is stuck",
			a:      marked(item("s:1", "in-progress")),
			target: "in-progress",
			want:   true,
			wantBy: "s:1",
		},
		{
			name:   "marked further along: the follower is still free to move up",
			a:      marked(item("s:1", "review")),
			target: "in-progress",
			want:   false,
		},
		{
			name:   "terminal: a finished dependency is maximum depth and holds nobody",
			a:      item("s:1", "done"),
			target: "review",
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := item("s:2", "ready", "s:1")
			d := NewDeps(line(), []model.Item{tc.a, b})
			hold, held := d.Held(&b, tc.target)
			if held != tc.want {
				t.Fatalf("Held(%s -> %s) = %v, want %v", b.ID, tc.target, held, tc.want)
			}
			if held && hold.By != tc.wantBy {
				t.Fatalf("held by %q, want %q", hold.By, tc.wantBy)
			}
		})
	}
}

// A chain needs no transitive closure: every link is enforced, so child 3 held
// behind child 2 which is held behind child 1 falls out of the same one rule.
func TestAChainNeedsNoClosure(t *testing.T) {
	one := item("s:1", "ready")
	two := item("s:2", "ready", "s:1")
	three := item("s:3", "ready", "s:2")
	d := NewDeps(line(), []model.Item{one, two, three})

	if _, held := d.Held(&two, "in-progress"); !held {
		t.Fatal("child 2 was allowed past child 1")
	}
	if _, held := d.Held(&three, "in-progress"); !held {
		t.Fatal("child 3 was allowed past child 2")
	}
	// #3 names only #2. It must not need to know about #1 for the chain to hold.
	if got := len(three.DependsOn); got != 1 {
		t.Fatalf("the fixture declares %d dependencies, want 1", got)
	}
}

// Fail open, every way an edge can be wrong. A typo must never wedge the line.
func TestBadEdgesFailOpen(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []model.Item
	}{
		{
			name:  "an id nobody listed (un-onboarded, another repo, a typo)",
			items: []model.Item{item("s:2", "ready", "s:999")},
		},
		{
			name:  "an item depending on itself",
			items: []model.Item{item("s:2", "ready", "s:2")},
		},
		{
			name: "a dependency sitting in a stage the config does not declare",
			items: []model.Item{
				item("s:1", "somewhere-else"),
				item("s:2", "ready", "s:1"),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := NewDeps(line(), tc.items)
			b := tc.items[len(tc.items)-1]
			if _, held := d.Held(&b, "in-progress"); held {
				t.Fatal("a bad edge held the item instead of being dropped")
			}
		})
	}
}

// A cycle is a person's mistake in an issue body. Holding both items forever
// would be a pipeline wedged by a typo, so the edges go and the board is told.
func TestCyclesAreDroppedAndReported(t *testing.T) {
	two := item("s:2", "ready", "s:1")
	three := item("s:3", "ready", "s:2")
	one := item("s:1", "ready", "s:3") // closes the loop
	d := NewDeps(line(), []model.Item{one, two, three})

	for _, it := range []model.Item{one, two, three} {
		if _, held := d.Held(&it, "in-progress"); held {
			t.Fatalf("%s was held by an edge that is part of a cycle", it.ID)
		}
	}
	if len(d.Cycles) != 1 {
		t.Fatalf("reported %d cycles, want 1: %v", len(d.Cycles), d.Cycles)
	}
	for _, id := range []string{"s:1", "s:2", "s:3"} {
		if !strings.Contains(d.Cycles[0], id) {
			t.Fatalf("the warning does not name %s: %q", id, d.Cycles[0])
		}
	}
}

// A cycle must not take unrelated edges down with it.
func TestACycleDoesNotDisarmTheRestOfTheGraph(t *testing.T) {
	one := item("s:1", "ready", "s:2")
	two := item("s:2", "ready", "s:1") // a 2-cycle
	four := item("s:4", "ready")
	five := item("s:5", "ready", "s:4") // innocent, and behind s:4
	d := NewDeps(line(), []model.Item{one, two, four, five})

	if _, held := d.Held(&five, "in-progress"); !held {
		t.Fatal("an edge with nothing to do with the cycle was dropped too")
	}
}

// The zero value holds nothing. Callers that have no listing to build from —
// and every existing test — must keep working.
func TestTheZeroDepsHoldsNothing(t *testing.T) {
	b := item("s:2", "ready", "s:1")
	if _, held := (Deps{}).Held(&b, "in-progress"); held {
		t.Fatal("the zero Deps held an item")
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `export PATH="$HOME/.local/go/bin:$PATH" && go test ./internal/pipeline/ -run 'Held|Chain|BadEdges|Cycle|ZeroDeps' -v`
Expected: FAIL to compile — `undefined: NewDeps`, `undefined: Deps`.

- [ ] **Step 3: Implement the graph**

Create `internal/pipeline/deps.go`:

```go
package pipeline

import (
	"sort"
	"strings"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// Hold is why an item with somewhere to go is not going there.
//
// Deliberately not a mark. Nobody clears a hold: it disappears on its own when
// the dependency moves, and a board that drew the two the same way would lie
// about what a person has to do.
type Hold struct {
	By      string `json:"by"`      // the item holding it, by id
	Stage   string `json:"stage"`   // where that item is standing
	Blocked bool   `json:"blocked"` // and whether it is marked there
	Target  string `json:"target"`  // the stage this item wanted to enter
}

// where an item stands, as the gate needs it.
type standing struct {
	stage   string
	depth   int
	blocked bool
}

// Deps is what the ladder needs to know about the rest of the listing: where
// every item sits right now, and whether it is marked there.
//
// Built from the FULL listing, never from a filtered slice. A dependency that
// is itself running or marked is precisely the one that must still hold its
// followers — and precisely the one server.go's `free` filter has already
// dropped before Pick ever sees it.
//
// The zero value holds nothing, so a caller with no listing to hand over is a
// pipeline with no sequencing rather than a nil dereference.
type Deps struct {
	at     map[string]standing
	stages map[string]int
	edges  map[string][]string
	// Cycles names every dependency cycle found and dropped, one line each,
	// for the board's warning strip.
	Cycles []string
}

// NewDeps reads the listing into a graph with every unusable edge already
// gone, so the gate itself needs no guarding.
//
// Four ways an edge can be unusable, and all four drop it rather than hold the
// item: an id nobody listed (an un-onboarded issue, another repository, a
// typo), an item naming itself, a dependency standing in a stage this config
// does not declare, and a cycle. Failing open is the same choice agents/_deps
// already makes — "a number from another repository or a typo must not hold an
// item forever" — and the reason is that the alternative is a pipeline a
// person can wedge by mistyping one line in an issue body.
func NewDeps(cfg *config.Config, items []model.Item) Deps {
	d := Deps{
		at:     make(map[string]standing, len(items)),
		stages: stageDepths(cfg),
		edges:  make(map[string][]string, len(items)),
	}
	for _, it := range items {
		// A stage the config does not declare has no position on the line.
		// Recording it as depth 0 would silently hold every follower at the
		// front, so such an item is simply not in `at` and holds nobody.
		depth, ok := d.stages[it.Stage]
		if !ok {
			continue
		}
		d.at[it.ID] = standing{stage: it.Stage, depth: depth, blocked: it.Blocked}
	}
	for _, it := range items {
		for _, dep := range it.DependsOn {
			if dep == it.ID {
				continue
			}
			if _, known := d.at[dep]; !known {
				continue
			}
			d.edges[it.ID] = append(d.edges[it.ID], dep)
		}
	}
	d.dropCycles()
	return d
}

// dropCycles removes every edge taking part in a cycle, and records what it
// removed so the board can say so.
//
// Depth-first, three-colour. The walk order is sorted because the same listing
// must produce the same warning twice: a map iteration would name a different
// member of the cycle each poll and the strip would flicker.
func (d *Deps) dropCycles() {
	const (
		white = iota // unvisited
		grey         // on the current path
		black        // finished
	)
	colour := make(map[string]int, len(d.at))
	drop := map[string]map[string]bool{}
	var path []string

	var walk func(id string)
	walk = func(id string) {
		colour[id] = grey
		path = append(path, id)
		for _, dep := range d.edges[id] {
			switch colour[dep] {
			case grey:
				// A back edge. Everything from dep to the end of the path is
				// the cycle, and every edge round it goes.
				at := len(path) - 1
				for at >= 0 && path[at] != dep {
					at--
				}
				if at < 0 {
					continue
				}
				cyc := append([]string(nil), path[at:]...)
				d.Cycles = append(d.Cycles, "dependency cycle, ignored: "+
					strings.Join(cyc, " -> ")+" -> "+cyc[0])
				for i, from := range cyc {
					to := cyc[(i+1)%len(cyc)]
					if drop[from] == nil {
						drop[from] = map[string]bool{}
					}
					drop[from][to] = true
				}
			case white:
				walk(dep)
			}
		}
		path = path[:len(path)-1]
		colour[id] = black
	}

	ids := make([]string, 0, len(d.edges))
	for id := range d.edges {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if colour[id] == white {
			walk(id)
		}
	}
	if len(drop) == 0 {
		return
	}
	for from, gone := range drop {
		kept := d.edges[from][:0]
		for _, to := range d.edges[from] {
			if !gone[to] {
				kept = append(kept, to)
			}
		}
		d.edges[from] = kept
	}
	sort.Strings(d.Cycles)
}

// Held reports whether moving it into target would overtake something it is
// sequenced behind, and names what is holding it.
//
// The whole rule, and it is one comparison: an item may enter a stage its
// dependency has already entered, and no further.
//
// It may SHARE that stage. Making every follower wait for its predecessor to
// finish would run the line one item deep and give away the throughput that
// makes it a pipeline — child 2 implementing while child 1 reviews is the
// behaviour worth having. It may never PASS it, which is the whole point. And
// it may not walk into a stage its dependency is MARKED in: a mark means that
// station did not finish, and putting a second item into it is the case this
// rule exists to prevent.
func (d Deps) Held(it *model.Item, target string) (Hold, bool) {
	want, ok := d.stages[target]
	if !ok {
		return Hold{}, false
	}
	for _, dep := range d.edges[it.ID] {
		on, known := d.at[dep]
		if !known {
			continue
		}
		if want > on.depth || (want == on.depth && on.blocked) {
			return Hold{By: dep, Stage: on.stage, Blocked: on.blocked, Target: target}, true
		}
	}
	return Hold{}, false
}
```

- [ ] **Step 4: Run the tests to make sure they pass**

Run: `export PATH="$HOME/.local/go/bin:$PATH" && go test ./internal/pipeline/ -run 'Held|Chain|BadEdges|Cycle|ZeroDeps' -v`
Expected: PASS — every subtest `ok`, including all six rows of the table.

- [ ] **Step 5: Commit**

```bash
git add internal/pipeline/deps.go internal/pipeline/deps_test.go
git commit -m "feat(pipeline): the dependency graph and the overtake rule

An item may enter a stage its dependency has already entered, and no
further. Unknown, self-, out-of-config and cyclic edges all fail open:
a typo must never wedge the line.

Not yet wired into the scheduler.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MFdzEwdmWA6MVYuTUTAfqe"
```

---

## Task 3: The gate at rung 1

**Files:**
- Modify: `internal/pipeline/pipeline.go:465-484` (`Target`), `:513-533` (`Pick`), `:552-569` (`Order`), `:598-629` (`rate`)
- Modify: `internal/pipeline/pipeline_test.go` (append)
- Modify: `internal/server/server.go:497`, `:878`, `:898`, `:1038`, `:1081`, `:1173`
- Modify: `internal/server/doctor.go:80`
- Modify: `cmd/conveyor/main.go:327`
- Modify: `docs/CONTRACTS.md` §5 rung list, `CLAUDE.md` invariants

**Interfaces:**
- Consumes: `NewDeps`, `Deps.Held`, `Hold` (Task 2).
- Produces: `Target(cfg *config.Config, it *model.Item, d Deps) (string, bool)`, `Pick(cfg *config.Config, items []model.Item, order []string, d Deps) (*model.Item, string)`, `Order(cfg *config.Config, items []model.Item, order []string, d Deps) []model.Item`. Task 4 calls all three.

- [ ] **Step 1: Write the failing test**

Append to `internal/pipeline/pipeline_test.go`:

```go
// The gate belongs at rung 1 — "has anywhere to go at all" — so one statement
// of the rule covers the scheduler, the board's ordering and the drag endpoint
// at once. A held item has nowhere to go, and Target says so.
func TestTargetRefusesAnOvertakingMove(t *testing.T) {
	cfg := line()
	one := model.Item{ID: "s:1", Stage: "ready"}
	two := model.Item{ID: "s:2", Stage: "ready", DependsOn: []string{"s:1"}}
	d := NewDeps(cfg, []model.Item{one, two})

	if _, ok := Target(cfg, &one, d); !ok {
		t.Fatal("the item at the head of the sequence was refused")
	}
	if target, ok := Target(cfg, &two, d); ok {
		t.Fatalf("the follower was sent to %q while its dependency sat in ready", target)
	}
}

// Pick is handed a FILTERED slice by server.go's launch loop — a dependency
// that is running or marked has already been dropped from it. Deps must
// therefore come from the full listing, or the one item that should hold its
// followers is exactly the one Pick cannot see.
func TestPickHoldsEvenWhenTheDependencyIsNotInTheSlice(t *testing.T) {
	cfg := line()
	one := model.Item{ID: "s:1", Stage: "ready"}
	two := model.Item{ID: "s:2", Stage: "ready", DependsOn: []string{"s:1"}}
	d := NewDeps(cfg, []model.Item{one, two}) // full listing

	// `free` excludes s:1 — it is already being worked.
	got, _ := Pick(cfg, []model.Item{two}, nil, d)
	if got != nil {
		t.Fatalf("Pick chose %s past a dependency missing from the slice", got.ID)
	}
}

// A held item is not in the queue, so it sorts with the rest of the tail —
// but it keeps its relative place among its siblings rather than being
// scrambled, because better() falls through to the same rungs Pick would use.
func TestOrderPutsHeldItemsBehindTheOnesThatCanRun(t *testing.T) {
	cfg := line()
	items := []model.Item{
		{ID: "s:3", Stage: "ready", DependsOn: []string{"s:2"}},
		{ID: "s:2", Stage: "ready", DependsOn: []string{"s:1"}},
		{ID: "s:1", Stage: "ready"},
	}
	d := NewDeps(cfg, items)
	got := Order(cfg, items, nil, d)
	if got[0].ID != "s:1" {
		t.Fatalf("the runnable item sorted %s first, want s:1", got[0].ID)
	}
	if got[1].ID != "s:2" || got[2].ID != "s:3" {
		t.Fatalf("held items lost their order: %s then %s", got[1].ID, got[2].ID)
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `export PATH="$HOME/.local/go/bin:$PATH" && go test ./internal/pipeline/ -run 'TargetRefuses|PickHolds|OrderPuts' -v`
Expected: FAIL to compile — `too many arguments in call to Target`.

- [ ] **Step 3: Add the gate and thread the value**

In `internal/pipeline/pipeline.go`, replace the body of `Target` (line 465) with:

```go
func Target(cfg *config.Config, it *model.Item, d Deps) (string, bool) {
	// A marked item is waiting for a person, and not picking it is the whole
	// mechanism: it is what stops the stage it stopped in from being re-run on
	// every poll, which is the job the old terminal `blocked` stage did by
	// carrying the item out of the line.
	if it.Blocked {
		return "", false
	}
	stage, ok := cfg.Stage(it.Stage)
	if !ok || stage.Terminal {
		return "", false
	}
	next := stage.OnSuccess
	if stage.Runs() {
		next = stage.Name // re-run: recover an interrupted stage
	} else if next == "" {
		return "", false // a queue with nowhere to go: items rest here
	}
	// An item sequenced behind something has nowhere to go until that thing
	// gets there first. Here rather than in `rate`, so the drag endpoint and
	// the stall counter get the same answer the scheduler does — one statement
	// of the rule, not three.
	if _, held := d.Held(it, next); held {
		return "", false
	}
	return next, true
}
```

Then change the three remaining signatures, threading `d` down:

```go
func Pick(cfg *config.Config, items []model.Item, order []string, d Deps) (*model.Item, string) {
```
and inside it, `c, target := rate(cfg, &items[i], i, pos, depths, d)`.

```go
func Order(cfg *config.Config, items []model.Item, order []string, d Deps) []model.Item {
```
and inside it, `rated[i], _ = rate(cfg, &items[i], i, pos, depths, d)`.

```go
func rate(cfg *config.Config, it *model.Item, listed int, pos map[string]int, depth map[string]int, d Deps) (candidate, string) {
	target, ok := Target(cfg, it, d)
```

- [ ] **Step 4: Fix the eight call sites**

`cmd/conveyor/main.go:327` — the tick loop is per source, and `res.Items` is that source's whole listing:

```go
			// The graph comes from the full listing, never from a subset: the
			// dependency that must hold a follower is often the very item a
			// filter has already dropped.
			deps := pipeline.NewDeps(cfg, res.Items)
			item, target := pipeline.Pick(cfg, res.Items, order.IDs(), deps)
```

`internal/server/server.go:497` — inside `retryStalled`'s loop over `s.state.Items`. Build the graph once, immediately after `s.mu.RLock()` and before `var held []model.Item`:

```go
		deps := pipeline.NewDeps(s.cfg, s.state.Items)
```
and change the call to `pipeline.Target(s.cfg, &it, deps)`.

`internal/server/server.go:878` — in `launch`. After `order := s.order.IDs()` add:

```go
	// Built from the full board, not from `free` below: a dependency that is
	// running or marked has already been filtered out of `free`, and it is
	// exactly the one that must still hold its followers.
	deps := pipeline.NewDeps(s.cfg, items)
```
then `target, ok := pipeline.Target(s.cfg, &it, deps)` and `item, target := pipeline.Pick(s.cfg, free, order, deps)`.

`internal/server/server.go:1038` — in `advance`:

```go
	deps := pipeline.NewDeps(s.cfg, items)
	item, target := pipeline.Pick(s.cfg, items, s.order.IDs(), deps)
```

`internal/server/server.go:1081` — in `handleState`:

```go
	st.Items = pipeline.Order(s.cfg, s.state.Items, s.state.Order, pipeline.NewDeps(s.cfg, s.state.Items))
```

`internal/server/server.go:1173` — the start endpoint. Capture the listing while the read lock is held (it is released just above at `s.mu.RUnlock()`), so move the `NewDeps` call inside the locked section by adding, just before that `s.mu.RUnlock()`:

```go
	deps := pipeline.NewDeps(s.cfg, s.state.Items)
```
then `target, ok := pipeline.Target(s.cfg, &item, deps)`.

`internal/server/doctor.go:80`:

```go
	for _, it := range pipeline.Order(s.cfg, s.state.Items, s.state.Order, pipeline.NewDeps(s.cfg, s.state.Items)) {
```

Any remaining compile error is a call site this list missed; fix it the same way — build `Deps` from the fullest listing in scope.

- [ ] **Step 5: Run the whole suite**

Run: `export PATH="$HOME/.local/go/bin:$PATH" && go build ./... && go test ./...`
Expected: build succeeds; every package `ok`, including the three new tests and every pre-existing `internal/pipeline` and `internal/server` test.

- [ ] **Step 6: Document the rung**

In `docs/CONTRACTS.md` §5, extend rung 1 of the ladder:

```
1. has anywhere to go at all — a marked item, a terminal stage, a queue with
   no exit, or an item sequenced behind something that has not reached the
   stage it wants to enter, is not in the running
```

and add below the rung list:

```
An item declaring `dependsOn` may enter a stage its dependency has already
entered, and no further. It may share that stage — a line where every follower
waited for its predecessor to *finish* would run one item deep and give away
the throughput that makes it a pipeline — but it may never pass it, and it may
not enter a stage its dependency is marked in. The chain needs no transitive
closure: every link is enforced, so a family sequences itself one edge at a
time.
```

In `CLAUDE.md`, add to the invariants list, immediately after the "An item sequenced behind an open issue is not started" bullet:

```
- **A follower never passes what it depends on.** `pipeline.Deps` is built from
  the full listing each pass and gates rung 1: an item may enter a stage its
  dependency has already entered, may share it, may never pass it, and may not
  walk into a stage its dependency is marked in. Stated in `Target`, so the
  scheduler, `Order` and the drag endpoint cannot disagree. Every unusable edge
  — an unknown id, a self-edge, a stage the config does not declare, a cycle —
  is dropped rather than held, because the alternative is a pipeline a person
  wedges by mistyping one line of an issue body. This is the line's own
  sequencing and is not the same thing as `agents/_deps`, which still asks
  GitHub about issues the board cannot see.
```

- [ ] **Step 7: Commit**

```bash
git add internal/pipeline/pipeline.go internal/pipeline/pipeline_test.go \
        internal/server/server.go internal/server/doctor.go \
        cmd/conveyor/main.go docs/CONTRACTS.md CLAUDE.md
git commit -m "feat(pipeline): a follower never passes what it depends on

The gate goes at rung 1, in Target, so the scheduler, Order and the drag
endpoint all get the same answer. Deps is built from the full listing:
the dependency that must hold a follower is often the one launch's
filter has already dropped.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MFdzEwdmWA6MVYuTUTAfqe"
```

---

## Task 4: The board says what is holding an item

**Files:**
- Modify: `internal/server/server.go:44-47` (`State`), `:1081-1090` (`handleState`), `:1566-1578` (`whyStuck`)
- Modify: `internal/server/web/index.html:251-258` (CSS), `:1401-1425` (`card`)
- Test: `internal/server/state_test.go` (append)

**Interfaces:**
- Consumes: `pipeline.Hold`, `pipeline.Deps.Held`, `Deps.Cycles` (Task 2).
- Produces: `State.Held map[string]pipeline.Hold` on `/api/state`, keyed by item ID; `.item.held` in the page.

- [ ] **Step 1: Write the failing test**

Append to `internal/server/state_test.go`:

```go
// A held item is not a blocked item. Blocked is a mark somebody has to clear;
// held disappears on its own when the dependency moves. The board is told
// which is which, or it lies about what needs a person.
func TestHeldIsReportedAndIsNotAMark(t *testing.T) {
	cfg := &config.Config{Stages: []config.Stage{
		{Name: "ready", OnSuccess: "in-progress"},
		{Name: "in-progress", Script: "implement", OnSuccess: "done"},
		{Name: "done", Terminal: true},
	}}
	items := []model.Item{
		{ID: "s:1", Stage: "ready"},
		{ID: "s:2", Stage: "ready", DependsOn: []string{"s:1"}},
	}
	held := heldOf(cfg, items)

	h, ok := held["s:2"]
	if !ok {
		t.Fatal("the follower was not reported as held")
	}
	if h.By != "s:1" || h.Stage != "ready" || h.Target != "in-progress" {
		t.Fatalf("hold = %+v, want by s:1 in ready, target in-progress", h)
	}
	if h.Blocked {
		t.Fatal("an unmarked dependency was reported as blocked")
	}
	if _, ok := held["s:1"]; ok {
		t.Fatal("the item at the head of the sequence was reported as held")
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `export PATH="$HOME/.local/go/bin:$PATH" && go test ./internal/server/ -run TestHeldIsReported -v`
Expected: FAIL to compile — `undefined: heldOf`.

- [ ] **Step 3: Implement the server side**

In `internal/server/server.go`, add to `State` immediately after the `Blocks` field (around line 50):

```go
	// Held is why an item with somewhere to go is not going there: it is
	// sequenced behind something that has not reached that stage yet. A
	// sibling of Blocks, and deliberately not the same thing — nobody has to
	// clear this, and it disappears on its own when the dependency moves.
	Held map[string]pipeline.Hold `json:"held,omitempty"`
```

Add the helper next to `whyStuck` (near line 1566):

```go
// heldOf is every item the sequencing rule is currently holding, keyed by id.
//
// Computed rather than remembered: a hold is a fact about where two items are
// standing right now, so a cached one would be wrong the moment either moves.
func heldOf(cfg *config.Config, items []model.Item) map[string]pipeline.Hold {
	deps := pipeline.NewDeps(cfg, items)
	out := map[string]pipeline.Hold{}
	for i := range items {
		it := &items[i]
		if it.Blocked {
			continue // a mark is the whole story; a hold behind it says nothing
		}
		stage, ok := cfg.Stage(it.Stage)
		if !ok || stage.Terminal {
			continue
		}
		next := stage.OnSuccess
		if stage.Runs() {
			next = stage.Name
		} else if next == "" {
			continue
		}
		if h, held := deps.Held(it, next); held {
			out[it.ID] = h
		}
	}
	return out
}
```

In `handleState` (line 1081), reuse one graph for both the ordering and the holds:

```go
	deps := pipeline.NewDeps(s.cfg, s.state.Items)
	st.Items = pipeline.Order(s.cfg, s.state.Items, s.state.Order, deps)
	st.Held = heldOf(s.cfg, s.state.Items)
	st.Warnings = append(append([]string(nil), s.state.Warnings...), deps.Cycles...)
```

Extend `whyStuck` so a refused drag says what is actually holding the card. Insert immediately after the `it.Blocked` check at line 1567:

```go
	s.mu.RLock()
	held := heldOf(s.cfg, s.state.Items)[it.ID]
	s.mu.RUnlock()
	if held.By != "" {
		if held.Blocked {
			return fmt.Sprintf("%s is held behind %s, which is marked in %s — clear that mark and this moves on its own",
				it.ID, held.By, held.Stage)
		}
		return fmt.Sprintf("%s is held behind %s, which is still in %s — it cannot enter %s first",
			it.ID, held.By, held.Stage, held.Target)
	}
```

Note: `whyStuck`'s callers must not already hold `s.mu`. `handleStart` releases the read lock before calling it (`server.go:1170`), which is the only caller.

- [ ] **Step 4: Run the test to make sure it passes**

Run: `export PATH="$HOME/.local/go/bin:$PATH" && go test ./internal/server/ -run TestHeldIsReported -v`
Expected: PASS.

- [ ] **Step 5: Draw it**

In `internal/server/web/index.html`, add after the `.item.blocked.asks` rule (around line 258):

```css
  /* Held, not stopped. The item is sequenced behind something that has not
     reached its next stage yet — nobody has to clear this and it goes away by
     itself, so it borrows the grey of a condition rather than the red of a
     mark. Drawing it as blocked would make the board lie about what needs a
     person, which is the first question it answers. */
  .item.held {
    border-color:var(--rule);
    background:color-mix(in srgb, var(--dim) 5%, var(--raised));
  }
  .item.held .title { color:var(--dim); }
  .item .behind { color:var(--faint); }
```

In the `card` function (line 1401), add `held` to the class list and the chip to the foot. Change the `cls` line to:

```js
  const hold = held[it.id];
  const cls = ["item", hasPrio ? "p" + it.priority : "", working ? "working" : "",
               it.blocked ? "blocked " + tone(blocks[it.id]) : "",
               !it.blocked && hold ? "held" : "", ranked ? "ranked" : ""].filter(Boolean).join(" ");
```

and add this line inside `<span class="foot">`, immediately after the `${it.blocked ? why(it) : ""}` line:

```js
      ${!it.blocked && hold ? `<span class="behind">behind ${esc(hold.by.split(":").pop())}</span>` : ""}
```

Then declare the lookup beside the existing `blocks` one — find `let blocks` (or `const blocks`) near the top of the script and add a sibling `let held = {};`, assigning it wherever `blocks` is assigned from the state payload:

```js
  held = st.held || {};
```

- [ ] **Step 6: See it**

Run:
```bash
export PATH="$HOME/.local/go/bin:$PATH"
go build -o conveyor ./cmd/conveyor && ./conveyor tick -n 4 -c conveyor.example.yaml
```
Expected: the mock pipeline still drains and nothing panics. Then check the payload shape directly:
```bash
go test ./internal/server/ -run TestHeldIsReported -v
```
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/server/server.go internal/server/web/index.html internal/server/state_test.go
git commit -m "feat(board): say what is holding a card, without calling it blocked

Held is a sibling of Blocks and deliberately not the same thing: nobody
clears a hold, it goes away when the dependency moves. Grey, like the
other conditions that pass on their own. whyStuck now names the item and
the stage a refused drag is waiting on.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MFdzEwdmWA6MVYuTUTAfqe"
```

---

## Task 5: The `prioritise` adapter

**Files:**
- Create: `agents/claude/prioritise`
- Create: `agents/claude/prioritise-selfcheck`

**Interfaces:**
- Consumes: `Item.DependsOn` is what this eventually produces *through* `list.sh`; this task writes the issue body it parses.
- Produces: an issue body carrying `Depends on #N` and `<!-- conveyor:sequence N/M after:#K -->` (or `<!-- conveyor:sequence standalone -->`), plus exactly one `priority:pN` label. Exits 0 always, except the agent-convention stops the shared `_blocked` helper defines.

- [ ] **Step 1: Write the failing test**

Create `agents/claude/prioritise-selfcheck`:

```bash
#!/usr/bin/env bash
# Self-check for agents/claude/prioritise. Runs the adapter against a stubbed
# `gh` and a stubbed `claude`, so it touches no network, no repository and no
# model — the approve-selfcheck precedent, and for the same reason: most of
# these cases cannot be reproduced against a live repository on demand.
#
#   ./agents/claude/prioritise-selfcheck
set -euo pipefail
cd "$(dirname "$0")"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0

check() { # check <label> <expected> <actual>
	if [[ "$2" == "$3" ]]; then
		echo "  ok   $1"
	else
		echo "  FAIL $1"
		echo "       want: $2"
		echo "       got:  $3"
		fail=1
	fi
}

mkdir -p "$tmp/stub"
# The model is stubbed to write the sequence the real prompt asks for, so this
# checks the adapter's plumbing — marker detection, the write-back, the label —
# and never the model's judgement, which is not testable here.
cat >"$tmp/stub/claude" <<'STUB'
#!/usr/bin/env bash
cat >/dev/null
echo "decided: child 3 of 5, after #34"
printf '%s' '{"after": 34, "part": 3, "of": 5, "priority": 2}' >"$CONVEYOR_RESULT"
STUB
chmod +x "$tmp/stub/claude"

cat >"$tmp/stub/gh" <<'STUB'
#!/usr/bin/env bash
echo "gh $*" >>"$GH_LOG"
case "$*" in
	*"issue edit"*) exit 0 ;;
	*"issue view"*) echo '{"body":"the body"}' ;;
	*)              echo "[]" ;;
esac
STUB
chmod +x "$tmp/stub/gh"

item() { # item <ref> <body>
	jq -n --arg ref "$1" --arg body "$2" \
		'{item:{id:"conveyor:\($ref)",ref:$ref,title:"Improvement child 3",description:$body,url:""}}'
}

run() { # run <ref> <body>
	: >"$GH_LOG"
	PATH="$tmp/stub:$PATH" \
	CONVEYOR_SOURCE=conveyor CONVEYOR_STAGE=prioritising \
	CONVEYOR_ITEM_REF="$1" CONVEYOR_RESULT="$tmp/result.json" \
	REPO="owner/repo" PROMPT="/prioritise \$REF" \
		./prioritise <<<"$(item "$1" "$2")" >"$tmp/out.log" 2>&1
}

export GH_LOG="$tmp/gh.log"

echo "prioritise"

# A fresh item: the model runs, and what it decided is written into the issue
# so the next poll — and every later stage — can read it without a model.
run 35 "no marker here"
check "writes the dependency line into the body" \
	"1" "$(grep -c -- '--body' "$GH_LOG")"
check "labels the priority the model chose" \
	"1" "$(grep -c -- '--add-label priority:p2' "$GH_LOG")"
check "exits 0 so the item moves on to ready" "0" "$?"

# The marker is what makes the model run once and never again. approve uses
# the same trick for the same reason: without it, every poll is a model run.
run 35 "body
<!-- conveyor:sequence 3/5 after:#34 -->"
check "an item already carrying a marker runs no model" \
	"0" "$(grep -c 'issue edit' "$GH_LOG")"

if [[ $fail -eq 0 ]]; then echo "prioritise: all checks passed"; else echo "prioritise: FAILED"; fi
exit $fail
```

```bash
chmod +x agents/claude/prioritise-selfcheck
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `./agents/claude/prioritise-selfcheck`
Expected: FAIL — `./prioritise: No such file or directory`.

- [ ] **Step 3: Write the adapter**

Create `agents/claude/prioritise`:

```bash
#!/usr/bin/env bash
# Record where an item sits in its sequence, once, and let the engine enforce
# it from then on.
#
# The engine holds a follower behind what it depends on (pipeline.Deps), but
# only if something wrote the dependency down. That is this stage's whole job:
# turn "Improvement child 3" into a line the provider can translate and a
# person can correct.
#
# **The model runs once per item, ever.** An item already carrying the
# `<!-- conveyor:sequence ... -->` marker is passed straight through — the same
# trick agents/claude/approve uses for its conflict marker, and for the same
# reason: without it, every poll is a model run.
#
# This is the one place the pipeline lets a model influence order, so the
# defence has to be explicit. The model writes ONE durable, human-readable line
# into the issue; the ENGINE reads only that line, deterministically; a person
# can correct or delete it and the pipeline obeys. The model is a worker inside
# one stage, which is the thesis in CLAUDE.md rather than an exception to it.
#
# Configuration, from the source's scripts.prioritise.params:
#   PROMPT           what to decide. Required. "$REF" is expanded.
#   ALLOWED_TOOLS    --allowedTools (default: read-only plus gh via Bash)
#   MODEL            --model (a cheap one: this is a small judgement)
#   MAX_TURNS        --max-turns (default 30)
set -euo pipefail

source "$(dirname "$0")/_stream"
source "$(dirname "$0")/../_blocked"

: "${PROMPT:?PROMPT is required (set it in the source scripts: block)}"
: "${REPO:?REPO is required (set it in the source env: block)}"

payload=$(cat)
ref=$(jq -r '.item.ref' <<<"$payload")
title=$(jq -r '.item.title' <<<"$payload")
body=$(jq -r '.item.description // ""' <<<"$payload")

# Already decided. Nothing to run, nothing to write: the item goes to ready.
if grep -q '<!-- conveyor:sequence' <<<"$body"; then
	echo "$ref already carries a sequence marker; no model run"
	exit 0
fi

echo "prioritising $CONVEYOR_SOURCE #$ref — $title"

task=${PROMPT//\$REF/$ref}
full="${task}

--- the item, as the pipeline sees it ---
Item #${ref} in ${REPO}: ${title}
${body}
--- end item ---

Decide two things and write them to \$CONVEYOR_RESULT as one JSON object.

1. Whether this item is part of a sequence of issues that must be worked in
   order — a numbered family (\"child 3\", \"part 2 of 5\", \"batch 4\"), or an
   unnumbered series whose parts plainly build on one another. Look at the
   other open issues in ${REPO} to decide. If it is, name the ONE issue that
   must come immediately before it as {\"after\": N}. Never a whole chain:
   each link is enforced separately, so naming only the predecessor is both
   sufficient and easier for a person to correct. Omit \"after\" entirely if
   this item stands alone.
2. A priority 0-3, as {\"priority\": N}: 0 users are broken right now, 1 a
   committed promise or a daily bug, 2 normal work, 3 nice to have. Default 2.

Also give {\"part\": N, \"of\": M} when you found a numbered family.
Do not open, close, edit or comment on any issue yourself — this script writes
the result back. Report what you concluded and why in your normal output.

$(decision_brief)"

set +e
MAX_TURNS="${MAX_TURNS:-30}" \
	ALLOWED_TOOLS="${ALLOWED_TOOLS:-Bash,Read,Glob,Grep}" \
	run_claude "$full"
rc=$?
set -e

record_model

if hit_limit; then
	blocked limit "the agent was out of quota and stopped before deciding the \
sequence. Nothing is wrong with this item; it is waiting for the window to reset."
fi
if hit_max_turns; then
	record_session
	blocked turns "the agent hit its ${MAX_TURNS:-30}-turn budget before \
deciding the sequence. Answering this mark with anything (e.g. \"continue\") \
resumes the same conversation with a fresh turn budget."
fi
if [[ -s "${CONVEYOR_RESULT:-}" ]] &&
	[[ "$(jq -r '.blocked // false' "$CONVEYOR_RESULT" 2>/dev/null)" == "true" ]]; then
	agent_asked
	echo "blocked: $(jq -r '.reason // "no reason given"' "$CONVEYOR_RESULT")" >&2
	exit 20
fi
[[ $rc -eq 0 ]] || exit $rc

after=$(jq -r '.after // empty' "$CONVEYOR_RESULT" 2>/dev/null || true)
part=$(jq -r '.part // empty' "$CONVEYOR_RESULT" 2>/dev/null || true)
of=$(jq -r '.of // empty' "$CONVEYOR_RESULT" 2>/dev/null || true)
prio=$(jq -r '.priority // 2' "$CONVEYOR_RESULT" 2>/dev/null || echo 2)

# An item cannot be sequenced behind itself, whatever the model decided. The
# engine drops a self-edge anyway; refusing to write one keeps the issue body
# honest for the person who reads it.
[[ "$after" == "$ref" ]] && after=""

marker="<!-- conveyor:sequence standalone -->"
line=""
if [[ -n "$after" ]]; then
	marker="<!-- conveyor:sequence ${part:-?}/${of:-?} after:#${after} -->"
	line="Depends on #${after}"
fi

# Read the body back rather than reusing the one on stdin: the listing that
# produced it is up to a poll old, and a person may have edited the issue since.
current=$(gh issue view "$ref" --repo "$REPO" --json body --jq .body)
printf '%s\n\n%s\n%s\n' "$current" "$line" "$marker" >"$CONVEYOR_RESULT.body"
gh issue edit "$ref" --repo "$REPO" --body-file "$CONVEYOR_RESULT.body"
gh issue edit "$ref" --repo "$REPO" --add-label "priority:p${prio}"

echo "sequenced $ref: ${line:-standalone}, priority p${prio}"
exit 0
```

```bash
chmod +x agents/claude/prioritise
```

- [ ] **Step 4: Run the selfcheck to make sure it passes**

Run: `./agents/claude/prioritise-selfcheck`
Expected: PASS — `prioritise: all checks passed`.

- [ ] **Step 5: Commit**

```bash
git add agents/claude/prioritise agents/claude/prioritise-selfcheck
git commit -m "feat(agents): record an item's place in its sequence, once

The model runs once per item and writes one durable line the engine can
read and a person can correct. A sequence marker in the body is what
stops every later poll being another model run, exactly as approve's
conflict marker does.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MFdzEwdmWA6MVYuTUTAfqe"
```

---

## Task 6: The stage, in the shipped template

**Files:**
- Create: `agents/mock/prioritise`
- Modify: `conveyor.example.yaml` (stages list, every source's `STAGE_LABELS` and `scripts:`)
- Modify: `providers/github/onboard.sh:34` (`KINDS` is unchanged; the stage label comes from `STAGE_LABELS`, so confirm the loop covers it)
- Modify: `docs/DESIGN.md`

**Interfaces:**
- Consumes: `agents/claude/prioritise` (Task 5).
- Produces: a `prioritising` stage between `refining` and `ready`, so `Target` routes `refining -> prioritising -> ready`.

- [ ] **Step 1: Write the failing test**

Run the demo pipeline and record that it currently has no such stage:

Run: `export PATH="$HOME/.local/go/bin:$PATH" && go build -o conveyor ./cmd/conveyor && ./conveyor validate -c conveyor.example.yaml && ./conveyor list -c conveyor.example.yaml`
Expected: valid, and the stage list prints `backlog refining ready ...` with **no** `prioritising`. That absence is what the next steps change.

- [ ] **Step 2: Write the mock adapter**

Create `agents/mock/prioritise`:

```bash
#!/usr/bin/env bash
# Mock prioritiser: a deterministic stand-in for agents/claude/prioritise, so
# the demo pipeline drains with no GitHub and no model.
#
# Reads the part number straight out of the title — which is exactly what the
# real stage asks a model to do, for the cases where a regex would have been
# enough — and sequences the item behind the ref one lower.
set -euo pipefail
payload=$(cat)
ref=$(jq -r '.item.ref' <<<"$payload")
title=$(jq -r '.item.title' <<<"$payload")

echo "prioritising $ref — $title"
part=$(grep -oiE '(child|part|batch|step)[[:space:]]*([0-9]+)' <<<"$title" | grep -oE '[0-9]+' | head -1 || true)
if [[ -n "$part" && "$part" -gt 1 ]]; then
	echo "  part $part; sequenced behind #$((ref - 1))"
	jq -n --argjson a "$((ref - 1))" --argjson p "$part" '{after:$a, part:$p, priority:2}' >"$CONVEYOR_RESULT"
else
	echo "  standalone"
	jq -n '{priority:2}' >"$CONVEYOR_RESULT"
fi
exit 0
```

```bash
chmod +x agents/mock/prioritise
```

- [ ] **Step 3: Add the stage to the template**

In `conveyor.example.yaml`, change the `refining` stage's route and insert the new stage before `ready`:

```yaml
  - name: refining
    script: refine
    timeout: 30m
    onSuccess: prioritising   # was: ready

  - name: prioritising
    script: prioritise
    timeout: 15m
    onSuccess: ready
    # Where an item's place in its sequence gets written down. The engine holds
    # a follower behind what it depends on, but only if something recorded the
    # dependency — this is that something, and it runs a model once per item,
    # ever. An item already carrying a sequence marker passes straight through.
    #
    # The one place a model influences order, and the defence is that it writes
    # one durable line a person can read and correct while the engine still
    # reads only that line. See agents/claude/prioritise.
```

Then, for **every** source in the file, add to its `STAGE_LABELS` block, between the `refining=` and `ready=` lines:

```
          prioritising=conveyor:prioritising
```

and add to its `scripts:` block:

```yaml
      prioritise:
        agent: mock
```

(For a source whose other scripts use `agent: claude`, use `agent: claude` with `params:` naming a cheap model, a small turn budget and `PROMPT: "/prioritise $REF"` — see Task 7 for the live shape.)

- [ ] **Step 4: Verify the pipeline still drains**

Run:
```bash
export PATH="$HOME/.local/go/bin:$PATH"
rm -f /tmp/conveyor-mock.json
go build -o conveyor ./cmd/conveyor
./conveyor validate -c conveyor.example.yaml
./conveyor list -c conveyor.example.yaml
./conveyor tick -n 10 -c conveyor.example.yaml
```
Expected: `validate` passes; `list` now shows `prioritising` between `refining` and `ready`; `tick` moves items through the new stage and the run output includes a `prioritising <ref>` line from the mock.

- [ ] **Step 5: Confirm onboarding creates the label**

`onboard.sh` derives stage labels from `STAGE_LABELS`, so the new one is covered by the existing loop. Confirm:

Run: `grep -n 'STAGE_LABELS' providers/github/onboard.sh`
Expected: the script reads `STAGE_LABELS` and creates a label per line. If it instead carries a hard-coded stage list, add `prioritising` to it.

- [ ] **Step 6: Document the stage**

In `docs/DESIGN.md`, add `prioritising` to the stage walkthrough wherever `refining` and `ready` are described, in one paragraph: what it records, that the model runs once per item, and that the engine — not the stage — enforces the order.

- [ ] **Step 7: Commit**

```bash
git add agents/mock/prioritise conveyor.example.yaml docs/DESIGN.md providers/github/onboard.sh
git commit -m "feat(config): a prioritising stage between refining and ready

Refined and prioritised is what conveyor.yaml already claimed 'ready'
meant. Now something makes it true.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01MFdzEwdmWA6MVYuTUTAfqe"
```

---

## Task 7: Live rollout

**Files:**
- Modify: `~/codes/conveyor.yaml` (outside the repo — machine state, not project source, and therefore not committed)
- GitHub: the five enrolled repositories' labels, and this repo's issues #33–#37

**Interfaces:**
- Consumes: everything above.
- Produces: the running pipeline works `Improvement child 1` before `child 2`.

- [ ] **Step 1: Prove the current behaviour is wrong**

Run:
```bash
cd ~/codes/Conveyor
gh issue list -R AmirRaptoR/Conveyor --state open --limit 40 \
  --json number,title,labels \
  -q '.[] | select(.title|test("Improvement child")) | "#\(.number) \(.title)"'
```
Expected: #33–#37 listed, none carrying a `priority:` label — which is why they tie down to listing order and child 5 goes first. Record this output; it is the before-evidence.

- [ ] **Step 2: Add the stage to the live config**

In `~/codes/conveyor.yaml`, apply the same two edits as Task 6 step 3 (`refining` routes to `prioritising`; the new stage before `ready`), then for each of the five sources add the label line to `STAGE_LABELS`:

```
          prioritising=conveyor:prioritising
```

and this `scripts:` entry:

```yaml
      prioritise:
        agent: claude
        params:
          ALLOWED_TOOLS: "Bash,Read,Glob,Grep"
          # A small judgement about ordering, not a spec. The cheap model is
          # the point: this runs once per item and the expensive one is spent
          # in refine, where a bad answer costs a whole implement cycle.
          MAX_TURNS: "30"
          MODEL: claude-haiku-4-5-20251001
          PROMPT: "/prioritise $REF"
```

- [ ] **Step 3: Validate the live config before anything runs against it**

Run: `export PATH="$HOME/.local/go/bin:$PATH" && ./conveyor validate -c ~/codes/conveyor.yaml`
Expected: valid, no errors. If it reports an unknown script name for a source, that source is missing its `prioritise:` entry — add it.

- [ ] **Step 4: Create the label in every enrolled repository**

Run, once per repo:
```bash
for repo in AmirRaptoR/Conveyor RaptoR-Soft/midgame AmirRaptoR/quesshi RaptoR-Soft/caravan-v2 miimra/meal-planner; do
  gh label create "conveyor:prioritising" --repo "$repo" \
    --color "C5DEF5" --description "Recording where this item sits in its sequence" 2>/dev/null \
    && echo "created in $repo" || echo "exists in $repo"
done
```
Expected: `created` or `exists` for each. Neither is an error — `onboard.sh` is idempotent for the same reason.

- [ ] **Step 5: Migrate the items already past the new stage**

Items sitting in `conveyor:ready` never passed through `prioritising`, so nothing sequenced them. Send them back through:

```bash
for n in 33 34 35 36 37; do
  gh issue edit $n --repo AmirRaptoR/Conveyor \
    --remove-label "conveyor:ready" --add-label "conveyor:prioritising" 2>/dev/null \
    && echo "#$n -> prioritising" || echo "#$n was not in ready; left alone"
done
```

Only relabel issues actually wearing `conveyor:ready`. An issue in `in-progress`, `review` or `approving` is mid-flight and must not be dragged backwards.

- [ ] **Step 6: Watch one pass and confirm the order**

Run: `export PATH="$HOME/.local/go/bin:$PATH" && ./conveyor tick -n 5 -c ~/codes/conveyor.yaml`
Expected: each child passes through `prioritising`, gains a `Depends on #N` line and a `priority:p2` label, and lands in `ready`. Then confirm the gate holds:

```bash
gh issue view 34 -R AmirRaptoR/Conveyor --json body -q .body | grep -i "depends on"
```
Expected: `Depends on #33`.

- [ ] **Step 7: Confirm the board agrees**

Open the board and check that children 2–5 draw grey with a `behind 33` chip and that only child 1 is runnable. A red card here is a bug: held is not blocked.

- [ ] **Step 8: Commit the repo-side leftovers**

`~/codes/conveyor.yaml` is machine state and lives outside the checkout, so it is not committed. Confirm the repo tree is clean — a dirty tree blocks conveyor's own deploys:

```bash
git status --short
```
Expected: empty. If anything is listed, commit or revert it before finishing.

---

## Self-Review

**Spec coverage.** Every section of the spec maps to a task: item contract → Task 1; provider → Task 1; scheduler (`Deps`, the gate, `retryStalled`) → Tasks 2–3; board (`State.Held`, `.item.held`, cycle warnings) → Task 4; the `prioritising` stage → Tasks 5–6; testing → distributed into the task that owns each behaviour; migration → Task 7; build order → Tasks 1–4 are the gate, Tasks 5–7 are the stage, and the gate ships useful on its own.

**One deliberate gap.** The spec notes that `State.Warnings` is collected but not drawn by the page (open issue #43). Task 4 appends cycle warnings to it correctly; they will not be visible until #43 ships. Rendering the strip is out of scope here rather than forgotten.

**Type consistency.** `Deps`, `NewDeps`, `Hold` and `Held` carry the same names and signatures in Tasks 2, 3 and 4. `Hold` fields are `By`, `Stage`, `Blocked`, `Target` throughout, in Go and in the `held[it.id]` lookup the page makes. `Item.DependsOn` is `[]string` of `"<source>:<ref>"` in Task 1 and read as such in Task 2. `heldOf(cfg, items)` is defined and called only in Task 4.

**One thing an executor must not get wrong.** `Deps` is built from the **full** listing at every call site. Building it from `free` in `server.go`'s `launch` would compile, pass most tests, and be silently wrong — a dependency that is running or marked is exactly what `free` drops, and exactly what must still hold its followers. `TestPickHoldsEvenWhenTheDependencyIsNotInTheSlice` in Task 3 is the guard.
