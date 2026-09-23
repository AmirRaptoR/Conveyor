package pipeline

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// RelationshipWarnings validates informational parent/child topology.
// Parent and Children never affect scheduling; malformed topology remains
// visible on the item and is reported so an operator can repair its source.
func RelationshipWarnings(items []model.Item) []string {
	byID := make(map[string]model.Item, len(items))
	for _, it := range items {
		byID[it.ID] = it
	}

	warnings := map[string]bool{}
	warn := func(format string, args ...any) {
		warnings[fmt.Sprintf(format, args...)] = true
	}
	for _, it := range items {
		if it.Parent != "" {
			validateRelationID(it, "parent", it.Parent, byID, warn)
			if it.Parent != it.ID {
				switch parent, ok := byID[it.Parent]; {
				case !ok:
				case !slices.Contains(parent.Children, it.ID):
					warn("%s names parent %s, but %s does not name it as a child", it.ID, it.Parent, it.Parent)
				}
			}
		}

		seen := map[string]bool{}
		for _, childID := range it.Children {
			if seen[childID] {
				warn("%s names child %s more than once", it.ID, childID)
				continue
			}
			seen[childID] = true
			validateRelationID(it, "child", childID, byID, warn)
			if childID == it.ID {
				continue
			}
			if child, ok := byID[childID]; ok && child.Parent != it.ID {
				if child.Parent == "" {
					warn("%s names child %s, but %s names no parent", it.ID, childID, childID)
				} else {
					warn("%s names child %s, but %s names parent %s", it.ID, childID, childID, child.Parent)
				}
			}
		}
	}

	// A parent cycle is contradictory even when every edge is reciprocal.
	for _, it := range items {
		path, positions := []string{}, map[string]int{}
		for id := it.ID; id != ""; {
			if at, found := positions[id]; found {
				cycle := canonicalCycle(path[at:])
				warn("parent cycle: %s", strings.Join(cycle, " -> "))
				break
			}
			positions[id] = len(path)
			path = append(path, id)
			next, ok := byID[id]
			if !ok {
				break
			}
			if next.Parent == id {
				break // already reported as a self-parent; not a second cycle too
			}
			id = next.Parent
		}
	}

	out := make([]string, 0, len(warnings))
	for warning := range warnings {
		out = append(out, warning)
	}
	sort.Strings(out)
	return out
}

func canonicalCycle(nodes []string) []string {
	if len(nodes) == 0 {
		return nil
	}
	first := 0
	for i := 1; i < len(nodes); i++ {
		if nodes[i] < nodes[first] {
			first = i
		}
	}
	cycle := make([]string, 0, len(nodes)+1)
	cycle = append(cycle, nodes[first:]...)
	cycle = append(cycle, nodes[:first]...)
	cycle = append(cycle, nodes[first])
	return cycle
}

func validateRelationID(it model.Item, kind, id string, byID map[string]model.Item, warn func(string, ...any)) {
	if id == it.ID {
		warn("%s names itself as its %s", it.ID, kind)
		return
	}
	if !strings.Contains(id, ":") {
		warn("%s has unqualified %s id %q; want <source>:<ref>", it.ID, kind, id)
		return
	}
	peer, exists := byID[id]
	peerSource := strings.SplitN(id, ":", 2)[0]
	if exists {
		peerSource = peer.Source
	}
	if peerSource != it.Source {
		warn("%s has cross-source %s %s (%s -> %s)", it.ID, kind, id, it.Source, peerSource)
	}
}
