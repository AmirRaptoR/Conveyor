package pipeline

import (
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
	// Cycles names every dependency cycle found and dropped, one line each,
	// for the board's warning strip.
	Cycles []string
}

// NewDeps reads the listing into a graph with every unusable edge already
// gone, so the gate itself needs no guarding.
//
// Five ways an edge can be unusable, and all five drop it rather than hold
// the item: an id nobody listed (an un-onboarded issue, another repository,
// a typo), an empty string, an item naming itself, a dependency standing in a
// stage this config does not declare, and a cycle. Failing open is the same
// choice agents/_deps already makes — "a number from another repository or a
// typo must not hold an item forever" — and the reason is that the
// alternative is a pipeline a person can wedge by mistyping one line in an
// issue body.
func NewDeps(cfg *config.Config, items []model.Item) Deps {
	d := Deps{
		at:     make(map[string]standing, len(items)),
		stages: stageDepths(cfg),
		edges:  make(map[string][]string, len(items)),
	}
	for i, it := range items {
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
			if dep == "" || dep == it.ID || seen[dep] {
				continue
			}
			if _, known := d.at[dep]; !known {
				continue
			}
			seen[dep] = true
			d.edges[it.ID] = append(d.edges[it.ID], dep)
		}
	}
	d.dropCycles()
	return d
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
// behaviour worth having. It may never PASS it, which is the whole point.
// And it may not walk into a stage its dependency is MARKED in: a mark means
// that station did not finish, and putting a second item into it is the case
// this rule exists to prevent.
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
	return Hold{By: bestBy, Stage: best.stage, Target: target, Blocked: best.blocked}, true
}
