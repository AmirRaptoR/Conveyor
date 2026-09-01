// Final report: a markdown summary of one item's whole passage through the
// pipeline, derived from its run history on every request. Split in two
// layers on purpose — a loader that reads the filesystem, and a pure
// formatter that does not — so the timeline rules can be tested against a
// fabricated run list instead of a directory tree.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/pipeline"
)

// runFact is one run in an item's history, paired with what it wrote to
// $CONVEYOR_RESULT. The loader reads result.json once so the formatter reads
// no files of its own — CONTRACTS §2's split, carried into this report: logs
// are never parsed, and everything structured comes back through one file.
type runFact struct {
	model.Run
	Result json.RawMessage
}

// loadReport walks run history for one item id and returns every run found,
// oldest first — the order the formatter reconstructs a timeline in. It
// reuses walkRuns rather than opening a second way of reading history, and
// inherits its behaviour: an unreadable run directory is silently skipped,
// not an error, and the report says when that leaves it incomplete.
func (s *Server) loadReport(itemID string) []runFact {
	var out []runFact
	s.walkRuns(func(m RunMeta) bool {
		if m.ItemID != itemID {
			return true
		}
		rf := runFact{Run: m.Run}
		if b, err := os.ReadFile(filepath.Join(m.Dir, "result.json")); err == nil {
			rf.Result = b
		}
		out = append(out, rf)
		return true
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// handleReport answers the final report for one item. Derived on demand, on
// every request: no persisted state, and it works retroactively for items
// that finished before this endpoint existed.
func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.RLock()
	var item model.Item
	found := false
	for _, it := range s.state.Items {
		if it.ID == id {
			item, found = it, true
			break
		}
	}
	s.mu.RUnlock()
	if !found {
		http.Error(w, "no item "+id+" on the board", http.StatusNotFound)
		return
	}
	md := formatReport(item, s.loadReport(id), s.cfg.Stages, time.Now())
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	_, _ = w.Write([]byte(md))
}

// --- formatting --------------------------------------------------------

// formatReport turns one item's run history into the final report. Pure —
// it reads no files — so `now` is passed in rather than read from the clock,
// which is what keeps "time so far" testable.
func formatReport(item model.Item, runs []runFact, stages []config.Stage, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Final report — %s\n\n", oneLine(item.Title))
	b.WriteString(identityLine(item))
	b.WriteString("\n\n")

	if len(runs) == 0 {
		b.WriteString("No runs were found for this item.\n")
		return b.String()
	}

	tl := buildTimeline(item, runs, stages, now)

	b.WriteString(sentence(tl))
	b.WriteString("\n")
	if tl.incomplete {
		fmt.Fprintf(&b, "\nHistory is incomplete: no successful move into %s survives in retained run history.\n",
			oneLine(item.Stage))
	}
	b.WriteString("\n")

	if t := timingsTable(tl); t != "" {
		b.WriteString(t)
		b.WriteString("\n")
	}

	b.WriteString("## Stages\n\n")
	b.WriteString(stagesTable(tl))
	b.WriteString("\n")

	b.WriteString("## Stops and failures\n\n")
	b.WriteString(stopsSection(tl))

	if notes := notesSection(tl); notes != "" {
		b.WriteString("\n")
		b.WriteString(notes)
	}

	return b.String()
}

// identityLine is the item id, and its url when it has one — linked only when
// the scheme is http or https, since anything else is not a place a browser
// should be sent.
func identityLine(item model.Item) string {
	line := "`" + oneLine(item.ID) + "`"
	if item.URL == "" {
		return line
	}
	if u, err := url.Parse(item.URL); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		return line + " · [" + oneLine(item.URL) + "](" + item.URL + ")"
	}
	return line + " · " + oneLine(item.URL)
}

// timeline is everything the formatter derives from a run list, computed
// once so each section reads it rather than re-deriving it.
type timeline struct {
	item model.Item

	opened, started, finished          time.Time
	hasOpened, hasStarted, hasFinished bool
	// incomplete is set when the item's current stage is terminal but no
	// successful move into it survives in retained history — the journey
	// happened, but the record of arriving did not, and a time computed from
	// what is left would lie about it.
	incomplete bool

	// currentElapsed is how long the item has been in its current stage, for
	// an unfinished item's opening sentence; hasCurrentElapsed is false when
	// that stage was never entered by a recorded move (an item onboarded
	// straight into it).
	currentElapsed    time.Duration
	hasCurrentElapsed bool

	rows      []stageRow
	stops     []stopEntry
	notes     map[string][]noteEntry
	noteOrder []string

	stageRuns int // count of "stage" kind runs, for the opening sentence
}

type stageRow struct {
	name       string
	entered    time.Time
	hasEntered bool
	left       time.Time
	hasLeft    bool
	dur        time.Duration
	soFar      bool
	visits     int
	runsCell   string
}

type stopEntry struct {
	kind, stage, reason, runID string
	at                         time.Time
	retried                    bool
}

type noteEntry struct {
	runID, text string
}

// buildTimeline reconstructs an item's passage from its runs, following the
// rules in docs/CONTRACTS.md (issue #8's scope): a stage entry is a
// successful move whose from and to differ, and a stage is left at the next
// entry into any other stage — never at startedAt, because source.Client.Move
// only updates item.Stage after the provider script exits 0.
func buildTimeline(item model.Item, runs []runFact, stages []config.Stage, now time.Time) *timeline {
	tl := &timeline{item: item, notes: map[string][]noteEntry{}}

	if t, ok := parseTime(item.CreatedAt); ok {
		tl.opened, tl.hasOpened = t, true
	}
	for _, rf := range runs {
		if rf.StartedAt.IsZero() {
			continue
		}
		if !tl.hasStarted || rf.StartedAt.Before(tl.started) {
			tl.started, tl.hasStarted = rf.StartedAt, true
		}
	}

	terminal := map[string]bool{}
	inConfig := map[string]bool{}
	stageByName := map[string]*config.Stage{}
	for i, st := range stages {
		terminal[st.Name] = st.Terminal
		inConfig[st.Name] = true
		stageByName[st.Name] = &stages[i]
	}

	type interval struct {
		entered, left time.Time
		open          bool
	}
	intervals := map[string][]interval{}
	var seenStages []string
	seen := map[string]bool{}
	remember := func(name string) {
		if !seen[name] {
			seen[name] = true
			seenStages = append(seenStages, name)
		}
	}

	runsByStage := map[string][]string{}

	var curStage string
	var curStart time.Time
	haveCur := false
	var lastTerminalAt time.Time
	var lastTerminalStage string
	haveLastTerminal := false

	for _, rf := range runs {
		switch rf.Kind {
		case "move":
			if rf.Outcome != model.OutcomeSuccess || rf.From == rf.To {
				continue // a failed move never happened; from==to is not a transition
			}
			remember(rf.To)
			if haveCur {
				intervals[curStage] = append(intervals[curStage], interval{entered: curStart, left: rf.FinishedAt})
			}
			curStage, curStart, haveCur = rf.To, rf.FinishedAt, true
			if terminal[rf.To] {
				lastTerminalAt, lastTerminalStage, haveLastTerminal = rf.FinishedAt, rf.To, true
			}
		case "stage":
			tl.stageRuns++
			runsByStage[rf.To] = append(runsByStage[rf.To], string(rf.Outcome))
		}
	}
	if haveCur {
		intervals[curStage] = append(intervals[curStage], interval{entered: curStart, open: true})
	}

	// Finished: the entry into the last terminal stage entered — only
	// trustworthy when it is an entry into the item's own current stage. A
	// terminal stage never routes onward, so the item cannot have left again.
	if terminal[item.Stage] {
		if haveLastTerminal && lastTerminalStage == item.Stage {
			tl.finished, tl.hasFinished = lastTerminalAt, true
		} else {
			tl.incomplete = true
		}
	}

	// Row order: the stages the item entered, in config order, then anything
	// history remembers that the current config no longer names.
	var names []string
	for _, st := range stages {
		if _, ok := intervals[st.Name]; ok {
			names = append(names, st.Name)
		}
	}
	for _, n := range seenStages {
		if !inConfig[n] {
			names = append(names, n)
		}
	}

	for _, name := range names {
		ivs := intervals[name]
		row := stageRow{name: name, visits: len(ivs), hasEntered: true, entered: ivs[0].entered}
		var dur time.Duration
		open := ivs[len(ivs)-1].open
		for _, iv := range ivs {
			if iv.open {
				continue
			}
			if d := iv.left.Sub(iv.entered); d > 0 {
				dur += d
			}
		}
		if !open {
			row.hasLeft, row.left = true, ivs[len(ivs)-1].left
		} else if name == item.Stage && !terminal[item.Stage] {
			// The item's current, still-open stay, and it has somewhere left
			// to go: time so far is meaningful, unlike a stage it is resting
			// in for good.
			last := ivs[len(ivs)-1]
			d := now.Sub(last.entered)
			if d < 0 {
				d = 0
			}
			dur += d
			row.soFar = true
			tl.currentElapsed, tl.hasCurrentElapsed = dur, true
		}
		row.dur = dur

		outcomes := runsByStage[name]
		switch st, known := stageByName[name]; {
		case len(outcomes) > 0:
			row.runsCell = strings.Join(outcomes, ", ")
		case known && !st.Runs():
			row.runsCell = "queued"
		}
		tl.rows = append(tl.rows, row)
	}

	// Stops and failures, and notes: one pass over the stage runs, in order.
	lastStageIdx := map[string]int{}
	for i, rf := range runs {
		if rf.Kind == "stage" {
			lastStageIdx[rf.To] = i
		}
	}
	var noteSeen []string
	noteSeenSet := map[string]bool{}
	for i, rf := range runs {
		if rf.Kind != "stage" {
			continue
		}
		switch rf.Outcome {
		case model.OutcomeBlocked, model.OutcomeFailure, model.OutcomeTimeout, model.OutcomeInterrupted:
			var timeout time.Duration
			if st, ok := stageByName[rf.To]; ok {
				timeout = st.Timeout.D()
			}
			mark := pipeline.Marked(rf.To, rf.Run, rf.Result, timeout, 0)
			tl.stops = append(tl.stops, stopEntry{
				kind: mark.Kind, stage: rf.To, reason: mark.Reason, runID: rf.ID,
				at: rf.FinishedAt, retried: lastStageIdx[rf.To] > i,
			})
		}
		if len(rf.Result) > 0 {
			var v struct {
				Summary any `json:"summary"`
			}
			if json.Unmarshal(rf.Result, &v) == nil {
				if text, ok := v.Summary.(string); ok {
					tl.notes[rf.To] = append(tl.notes[rf.To], noteEntry{runID: rf.ID, text: text})
					if !noteSeenSet[rf.To] {
						noteSeenSet[rf.To] = true
						noteSeen = append(noteSeen, rf.To)
					}
				}
			}
		}
	}
	for _, st := range stages {
		if _, ok := tl.notes[st.Name]; ok {
			tl.noteOrder = append(tl.noteOrder, st.Name)
		}
	}
	for _, n := range noteSeen {
		if !inConfig[n] {
			tl.noteOrder = append(tl.noteOrder, n)
		}
	}

	return tl
}

func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// sentence is the report's one-line summary, up top.
func sentence(tl *timeline) string {
	counts := countsPhrase(len(tl.rows), tl.stageRuns, len(tl.stops))
	switch {
	case tl.hasFinished && tl.hasStarted:
		return fmt.Sprintf("Finished in %s — %s", formatDuration(tl.finished.Sub(tl.started)), counts)
	case tl.hasFinished:
		return fmt.Sprintf("Finished — %s", counts)
	case tl.hasCurrentElapsed:
		return fmt.Sprintf("In %s for %s — %s", oneLine(tl.item.Stage), formatDuration(tl.currentElapsed), counts)
	default:
		return fmt.Sprintf("In %s — %s", oneLine(tl.item.Stage), counts)
	}
}

func countsPhrase(nStages, mRuns, kStops int) string {
	return fmt.Sprintf("%s, %s, %s.", plural(nStages, "stage"), plural(mRuns, "stage run"), stopsPhrase(kStops))
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func stopsPhrase(n int) string {
	switch n {
	case 0:
		return "no stops"
	case 1:
		return "1 stop"
	default:
		return fmt.Sprintf("%d stops", n)
	}
}

func timingsTable(tl *timeline) string {
	type row struct{ label, value string }
	var rows []row
	if tl.hasOpened {
		rows = append(rows, row{"Opened", formatTime(tl.opened)})
	}
	if tl.hasStarted {
		rows = append(rows, row{"Started", formatTime(tl.started)})
	}
	if tl.hasFinished {
		rows = append(rows, row{"Finished", formatTime(tl.finished)})
	}
	if tl.hasOpened && tl.hasFinished {
		rows = append(rows, row{"Lead time", formatDuration(tl.finished.Sub(tl.opened))})
	}
	if tl.hasStarted && tl.hasFinished {
		rows = append(rows, row{"Working time", formatDuration(tl.finished.Sub(tl.started))})
	}
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("| | |\n| --- | --- |\n")
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s |\n", r.label, escapeCell(r.value))
	}
	return b.String()
}

func stagesTable(tl *timeline) string {
	var b strings.Builder
	b.WriteString("| Stage | Entered | Left | Time | Visits | Runs |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, row := range tl.rows {
		entered, left, timeCell := "—", "—", "—"
		if row.hasEntered {
			entered = formatTime(row.entered)
		}
		if row.hasLeft {
			left = formatTime(row.left)
			timeCell = formatDuration(row.dur)
		} else if row.soFar {
			timeCell = formatDuration(row.dur) + " (so far)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %d | %s |\n",
			escapeCell(row.name), escapeCell(entered), escapeCell(left), escapeCell(timeCell),
			row.visits, escapeCell(row.runsCell))
	}
	return b.String()
}

func stopsSection(tl *timeline) string {
	if len(tl.stops) == 0 {
		return "No stops.\n"
	}
	var b strings.Builder
	for _, s := range tl.stops {
		verb := "marked"
		if s.retried {
			verb = "retried"
		}
		fmt.Fprintf(&b, "- **%s** in `%s` — %s — %s — run `%s` — %s\n",
			oneLine(s.kind), oneLine(s.stage), formatTime(s.at), oneLine(s.reason), oneLine(s.runID), verb)
	}
	return b.String()
}

func notesSection(tl *timeline) string {
	if len(tl.noteOrder) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Notes\n")
	for _, stage := range tl.noteOrder {
		fmt.Fprintf(&b, "\n### %s\n\n", oneLine(stage))
		for i, n := range tl.notes[stage] {
			if i > 0 {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "> %s\n> — run `%s`\n", escapeLeading(oneLine(n.text)), oneLine(n.runID))
		}
	}
	return b.String()
}

// formatTime renders every timestamp the same way, in UTC: 2006-01-02 15:04Z.
func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04Z")
}

// formatDuration renders at most two units, dropping a zero smaller one:
// 3d 5h, 1h 40m, 12m, 45s, <1s.
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Second {
		return "<1s"
	}
	days := int(d / (24 * time.Hour))
	d -= time.Duration(days) * 24 * time.Hour
	hours := int(d / time.Hour)
	d -= time.Duration(hours) * time.Hour
	mins := int(d / time.Minute)
	d -= time.Duration(mins) * time.Minute
	secs := int(d / time.Second)

	switch {
	case days > 0:
		if hours > 0 {
			return fmt.Sprintf("%dd %dh", days, hours)
		}
		return fmt.Sprintf("%dd", days)
	case hours > 0:
		if mins > 0 {
			return fmt.Sprintf("%dh %dm", hours, mins)
		}
		return fmt.Sprintf("%dh", hours)
	case mins > 0:
		return fmt.Sprintf("%dm", mins)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

// oneLine collapses embedded newlines, so free text can never turn one row —
// a table cell, a bullet — into more than one.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.ReplaceAll(s, "\r", " ")
}

// escapeCell prepares text for a table cell: a pipe ends a cell early.
func escapeCell(s string) string {
	return strings.ReplaceAll(oneLine(s), "|", "\\|")
}

// escapeLeading guards a blockquote's own first line from starting a new
// block — a summary beginning "# " or "- " must stay quoted text, not become
// a heading or a nested list.
func escapeLeading(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '#', '-', '*', '+', '>', '`':
		return "\\" + s
	}
	if s[0] >= '0' && s[0] <= '9' {
		i := 0
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
		}
		if i > 0 && i < len(s) && (s[i] == '.' || s[i] == ')') {
			return "\\" + s
		}
	}
	return s
}
