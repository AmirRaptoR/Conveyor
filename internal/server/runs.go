package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/AmirRaptoR/Conveyor/internal/model"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// RunMeta is one run directory, as the board needs it.
type RunMeta struct {
	model.Run
	// Lines, not a blob: a log carries structure the writer already knew —
	// which stream a line came from — and handing back one string throws it
	// away, so a finished run could not be rendered the way a live one is.
	//
	// Bounded: at most tail lines (2000 by default), from offset if given —
	// see logWindow. TotalLines is the true count, so a caller can page to
	// the rest instead of being handed a log's whole size in one response.
	Lines      []runner.LogLine `json:"lines,omitempty"`
	TotalLines int              `json:"totalLines"`
}

// handleRuns lists recent runs, newest first, optionally for one item.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	item := r.URL.Query().Get("item")
	runs, err := s.listRuns(item, 50)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, runs)
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !runner.ValidID(id) {
		// Never interpret an arbitrary request path as a filesystem path: a
		// malformed id is rejected before it is joined onto anything.
		http.Error(w, "invalid run id", http.StatusBadRequest)
		return
	}
	run, ok := s.findRun(id)
	if !ok {
		s.mu.RLock()
		swept := s.everSwept
		horizon := s.sweepHorizon
		s.mu.RUnlock()
		if swept {
			// Genuinely honest but coarse: day directories are the unit of
			// retention and no per-run tombstone is kept, so this cannot
			// prove the id was swept rather than never having existed — only
			// that runs before the horizon are gone. See CONTRACTS §6.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":   "retained",
				"horizon": horizon.Format("2006-01-02"),
			})
			return
		}
		http.NotFound(w, r)
		return
	}
	b, _ := os.ReadFile(filepath.Join(run.Dir, "log.txt"))
	all := parseLog(string(b))
	run.TotalLines = len(all)
	run.Lines = logWindow(all, r.URL.Query().Get("tail"), r.URL.Query().Get("offset"))
	writeJSON(w, run)
}

// defaultTailLines is what GET /api/runs/{id} returns when the caller asks
// for nothing in particular: a large log must not be handed over whole.
const defaultTailLines = 2000

// logWindow slices a bounded piece out of a run's full log, in units of
// lines: tail with no offset is the last N lines (2000 if tail is absent or
// invalid); an offset pages from that line index for up to tail lines.
func logWindow(all []runner.LogLine, tailParam, offsetParam string) []runner.LogLine {
	tail := defaultTailLines
	if n, err := strconv.Atoi(tailParam); err == nil && n >= 0 {
		tail = n
	}
	total := len(all)
	if offsetParam == "" {
		start := total - tail
		if start < 0 {
			start = 0
		}
		return all[start:]
	}
	offset, err := strconv.Atoi(offsetParam)
	if err != nil || offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := offset + tail
	if end > total {
		end = total
	}
	return all[offset:end]
}

// findRun resolves a run ID directly against the run root: the ID is the run
// directory's own name (runner.Run's leaf), so the lookup is a stat per day
// directory rather than a walk that reads every meta.json to find a match —
// O(retained days), not O(retained runs). id is assumed already validated by
// runner.ValidID; findRun additionally refuses a match that turns out to be a
// symlink escaping the run root.
func (s *Server) findRun(id string) (RunMeta, bool) {
	root := s.run.Root
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return RunMeta{}, false
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		resolvedRoot = absRoot
	}
	days, err := os.ReadDir(root)
	if err != nil {
		return RunMeta{}, false
	}
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		dir := filepath.Join(root, day.Name(), id)
		fi, err := os.Stat(dir)
		if err != nil || !fi.IsDir() {
			continue
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		if resolved != resolvedRoot && !strings.HasPrefix(resolved, resolvedRoot+string(filepath.Separator)) {
			continue // a symlink inside the run root pointing outside it
		}
		b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil || len(b) == 0 {
			continue
		}
		var m RunMeta
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		m.Dir = dir
		return m, true
	}
	return RunMeta{}, false
}

// listRuns walks the run root newest-day-first and stops once it has enough.
func (s *Server) listRuns(itemID string, limit int) ([]RunMeta, error) {
	out := []RunMeta{}
	s.walkRuns(func(m RunMeta) bool {
		if itemID != "" && m.ItemID != itemID {
			return true
		}
		out = append(out, m)
		return len(out) < limit
	})
	return out, nil
}

// walkRuns visits every run newest first, and stops when the visitor says so.
// One walk, one definition of "newest": the directory names are the clock, day
// then time-ordered id, so sorting them descending is the whole ordering.
func (s *Server) walkRuns(visit func(RunMeta) bool) {
	root := s.run.Root
	days, err := os.ReadDir(root)
	if err != nil {
		return
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Name() > days[j].Name() })

	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(root, day.Name()))
		if err != nil {
			continue
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() > entries[j].Name() })
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root, day.Name(), e.Name())
			b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
			if err != nil || len(b) == 0 {
				continue
			}
			var m RunMeta
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			m.Dir = dir
			if !visit(m) {
				return
			}
		}
	}
}

// parseLog turns a written log back into the lines it was made of. The format
// is the runner's: a timestamp, the stream, then the text, which may itself
// contain anything at all — so it is split exactly twice and no further.
func parseLog(s string) []runner.LogLine {
	out := []runner.LogLine{}
	for _, ln := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if ln == "" {
			out = append(out, runner.LogLine{Stream: "stdout"})
			continue
		}
		parts := strings.SplitN(ln, " ", 3)
		if len(parts) < 3 {
			out = append(out, runner.LogLine{Stream: "stdout", Text: ln})
			continue
		}
		// Verbatim: the writer pads the stream to a fixed width, and every
		// stream name is already that width, so anything after the second
		// space is the script's own text — indentation included.
		out = append(out, runner.LogLine{Stream: strings.TrimSpace(parts[1]), Text: parts[2]})
	}
	return out
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
