package pipeline

import (
	"fmt"
	"sort"
	"strings"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// Hold is why an item with somewhere to go is not going there.
//
// Deliberately not a mark. Nobody clears a hold: it disappears on its own
// when the dependency moves, and a board that drew the two the same way
// would lie about what a person has to do.
type Hold struct {
	By      string `json:"by"`      // the item holding it, by id
	Stage   string `json:"stage"`   // where that item is standing
	Target  string `json:"target"`  // the stage this item wanted to enter
	Blocked bool   `json:"blocked"` // and whether it is marked there
	// Until is the stage the dependency has to reach before the hold lifts:
	// Target itself under the default rule, or the target stage's
	// `dependenciesAt` when it declares one.
	Until string `json:"until"`
	// Invalid distinguishes a dependency-graph error from an ordinary hold.
	// It never clears merely because a stage moves: the declaration itself
	// must be repaired. Reason is safe to show directly to an operator.
	Invalid bool   `json:"invalid,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

// standing is where an item stands, as the gate needs it.
type standing struct {
	stage   string
	depth   int
	blocked bool
	order   int // position in the listing NewDeps was built from
}

// Deps is what the ladder needs to know about the rest of the listing: where
// every item sits right now, and whether it is marked there.
//
// Built from the FULL listing, never from a filtered slice. A dependency that
// is itself running or marked is precisely the one that must still hold its
// followers — and precisely the one a scheduler's own "free" filter has
// already dropped before Pick ever sees it.
//
// The zero value holds nothing, so a caller with no listing to hand over is a
// pipeline with no sequencing rather than a nil dereference.
type Deps struct {
	at     map[string]standing
	stages map[string]int
	edges  map[string][]string
	// until is, per stage that declares `dependenciesAt`, the name of the
	// stage a dependency must have reached before an item may enter it.
	// A stage absent here uses the default rule: the target stage itself.
	until map[string]string
	// invalid is the first deterministic graph error for each affected item.
	// Errors carries every unique error for the board-wide warning strip.
	invalid  map[string]Hold
	errorSet map[string]bool
	Errors   []string
	// Cycles names every dependency cycle found and dropped, one line each,
	// retained separately for callers/tests that specifically count cycles.
	Cycles []string
}

// NewDeps reads the listing into a graph. Invalid declarations fail closed:
// a missing item, self-reference, unknown dependency stage, or cycle holds
// the affected item without consuming an execution slot and produces a
// visible error. Silently discarding one of those edges would let work run in
// an order the issue explicitly says is unsafe. Empty provider values are the
// sole exception: they declare no dependency at all.
func NewDeps(cfg *config.Config, items []model.Item) Deps {
	d := Deps{
		at:       make(map[string]standing, len(items)),
		stages:   stageDepths(cfg),
		edges:    make(map[string][]string, len(items)),
		until:    make(map[string]string),
		invalid:  make(map[string]Hold),
		errorSet: make(map[string]bool),
	}
	if cfg != nil {
		// A threshold remains true after the item passes the stage that
		// declared it. This reconciles items already in flight when a gate is
		// introduced: an item found in review cannot keep moving if its
		// implement gate required the dependency to have merged first.
		for target, targetDepth := range d.stages {
			required, requiredDepth := target, targetDepth
			for _, s := range cfg.Stages {
				gateDepth, gateOK := d.stages[s.Name]
				atDepth, atOK := d.stages[s.DependenciesAt]
				if s.DependenciesAt != "" && gateOK && atOK && gateDepth <= targetDepth && atDepth > requiredDepth {
					required, requiredDepth = s.DependenciesAt, atDepth
				}
			}
			if required != target {
				d.until[target] = required
			}
		}
	}
	listed := make(map[string]model.Item, len(items))
	for i, it := range items {
		listed[it.ID] = it
		// A stage the config does not declare has no position on the line.
		// Recording it as depth 0 would silently hold every follower at the
		// front, so such an item is simply not in `at` and holds nobody.
		depth, ok := d.stages[it.Stage]
		if !ok {
			continue
		}
		d.at[it.ID] = standing{stage: it.Stage, depth: depth, blocked: it.Blocked, order: i}
	}
	for _, it := range items {
		seen := map[string]bool{}
		for _, dep := range it.DependsOn {
			if dep == "" || seen[dep] {
				continue
			}
			seen[dep] = true
			switch {
			case dep == it.ID:
				d.addInvalid(it.ID, dep, fmt.Sprintf("%s depends on itself", it.ID))
				continue
			case listed[dep].ID == "":
				d.addInvalid(it.ID, dep, fmt.Sprintf("%s depends on missing item %s", it.ID, dep))
				continue
			case d.at[dep].stage == "":
				d.addInvalid(it.ID, dep, fmt.Sprintf("%s depends on %s, whose stage %q is not declared", it.ID, dep, listed[dep].Stage))
				continue
			}
			d.edges[it.ID] = append(d.edges[it.ID], dep)
		}
	}
	d.dropCycles()
	return d
}

func (d *Deps) addInvalid(id, by, reason string) {
	if _, exists := d.invalid[id]; !exists {
		d.invalid[id] = Hold{By: by, Invalid: true, Reason: reason}
	}
	if !d.errorSet[reason] {
		d.errorSet[reason] = true
		d.Errors = append(d.Errors, reason)
	}
}

// dropCycles removes every edge taking part in a cycle, and records what it
// removed so the board can say so.
//
// Depth-first, three-colour. The walk order is sorted because the same
// listing must produce the same warning twice: a map iteration would name a
// different member of the cycle each poll and the strip would flicker. Only
// the edges inside a cycle are dropped — an unrelated edge out of the same
// item survives.
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
				reason := "dependency cycle: " + strings.Join(cyc, " -> ") + " -> " + cyc[0]
				d.Cycles = append(d.Cycles, reason)
				for i, from := range cyc {
					d.addInvalid(from, cyc[(i+1)%len(cyc)], reason)
				}
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
		sort.Strings(d.Errors)
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
	sort.Strings(d.Errors)
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
// behaviour worth having. It may never PASS it, which is the whole point.
// And it may not walk into a stage its dependency is MARKED in: a mark means
// that station did not finish, and putting a second item into it is the case
// this rule exists to prevent.
//
// A stage may ask for more than sharing. `dependenciesAt: merged` on the
// implement stage means a follower enters it only once every dependency has
// reached `merged` or gone past it — because implementing on top of code
// that is not on main yet produces a pull request that cannot merge on its
// own, and a line that lets that happen fills its columns with items nothing
// can finish. The comparison is the same one; only the bar moves, from the
// target stage to the stage the config names.
//
// Several dependencies hold until all of them permit. The most restrictive —
// the one at the lowest depth, i.e. the one least far along — is the one
// reported; ties are broken by listing order, so the reported holder is
// stable across polls with unchanged input.
func (d Deps) Held(it *model.Item, target string) (Hold, bool) {
	want, ok := d.stages[target]
	if !ok {
		return Hold{}, false
	}
	until := target
	if u, ok := d.until[target]; ok {
		until, want = u, d.stages[u]
	}
	if invalid, bad := d.invalid[it.ID]; bad {
		invalid.Target, invalid.Until = target, until
		return invalid, true
	}
	var best standing
	var bestBy string
	found := false
	for _, dep := range d.edges[it.ID] {
		on, known := d.at[dep]
		if !known {
			continue
		}
		if want > on.depth || (want == on.depth && on.blocked) {
			if !found || on.depth < best.depth || (on.depth == best.depth && on.order < best.order) {
				best, bestBy, found = on, dep, true
			}
		}
	}
	if !found {
		return Hold{}, false
	}
	return Hold{By: bestBy, Stage: best.stage, Target: target, Blocked: best.blocked, Until: until}, true
}

// DependsOnAny reports whether id's declared dependency graph reaches any id
// in unavailable. It is used by dispatchers for facts orthogonal to stage
// ordering — notably a provider whose latest listing failed. A dependency's
// cached stage may still be useful to draw, but it must not authorize work.
//
// Cyclic edges have already been removed and their owners fail closed through
// invalid, but seen keeps this helper total even for a zero-value or partially
// constructed Deps.
func (d Deps) DependsOnAny(id string, unavailable map[string]bool) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(from string) bool {
		if seen[from] {
			return false
		}
		seen[from] = true
		for _, dep := range d.edges[from] {
			if unavailable[dep] || walk(dep) {
				return true
			}
		}
		return false
	}
	return walk(id)
}
