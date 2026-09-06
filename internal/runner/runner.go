// Package runner executes one script and persists everything about it.
//
// Two rules from docs/CONTRACTS.md shape this package:
//
//   - stdout and stderr are LOGS and are never parsed. Structured data comes
//     back only through the file at $CONVEYOR_RESULT. An AI stage script
//     writes megabytes of prose to stdout; treating that as a data channel is
//     how this system would break.
//   - a run is a self-contained directory, so a failure can be handed to
//     another person or agent with everything needed to understand it.
package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// LogLine is one line of script output, stamped and tagged with its stream.
type LogLine struct {
	At     time.Time `json:"at"`
	Stream string    `json:"stream"` // "stdout" | "stderr" | "engine"
	Text   string    `json:"text"`
}

// Spec is one invocation.
type Spec struct {
	Script string // absolute path; empty when Inline is set
	// Inline is a script body given in the config instead of a file. It is
	// written into the run directory and executed from there, so the exact
	// text that ran is archived with its own logs. It needs a shebang: the
	// engine writes the file and execs it, and never picks an interpreter.
	Inline  string
	Kind    string // "list" | "move" | "stage"
	Workdir string
	Env     map[string]string
	Stdin   any           // marshalled to JSON and piped in
	Timeout time.Duration //  <= 0 means no limit
	Source  string
	Item    *model.Item
	From    string
	To      string
}

// Result is a finished run plus whatever the script wrote to the result file.
type Result struct {
	Run model.Run
	// Data is the parsed $CONVEYOR_RESULT file; nil when the script wrote
	// nothing, which is not an error.
	Data json.RawMessage
	// Log is a bounded tail of the run's output — at most maxLogLines lines,
	// each at most maxLineBytes. Nothing in production reads it (only this
	// package's own tests do); log.txt on disk is always the complete,
	// untruncated record. See CONTRACTS §6.
	Log []LogLine
}

// maxLogLines bounds the in-memory copy of a run's log kept alongside the
// file on disk. A long agent session can emit hundreds of thousands of
// lines; nothing in production reads Result.Log, so there is no reason for it
// to grow with the run instead of staying a fixed-size tail.
const maxLogLines = 1000

// maxLineBytes bounds a single log line kept in memory and published over
// SSE. A script can emit one very long line — scan() below never limits a
// read for exactly that reason — but a browser rendering it, or this slice
// holding it, must not be handed the whole thing. log.txt still gets it in
// full.
const maxLineBytes = 64 * 1024

// Runner writes run directories under Root.
type Runner struct {
	Root string
	// OnLog, if set, is called for every line as it is produced — this is what
	// makes logs live in the UI. Called from a single goroutine, in order.
	OnLog func(runID string, line LogLine)
	// OnResult, if set, is called once for every run this Runner executes,
	// after its final meta.json write (or failed attempt at one) — list,
	// move, stage, doctor, status, all of it. It is what lets a single place
	// notice a persistence fault (Result.Run.Error naming one) without
	// threading a check through every call site that starts a run.
	OnResult func(res *Result)
}

// gracePeriod is how long a script gets to exit after SIGTERM before SIGKILL.
const gracePeriod = 30 * time.Second

func New(root string) *Runner { return &Runner{Root: root} }

// idRe is the shape Run() gives a run ID: HHMMSS.mmm-<base36 suffix>. A
// caller resolving an ID from a request path must reject anything else before
// it ever becomes part of a filesystem path — "../", an absolute path, an
// encoded separator and an empty string all fail this and touch no file.
var idRe = regexp.MustCompile(`^[0-9]{6}\.[0-9]{3}-[0-9a-z]{1,10}$`)

// ValidID reports whether id has the shape Run() gives a run — the only
// vocabulary a request is allowed to name a run in.
func ValidID(id string) bool { return idRe.MatchString(id) }

// Run executes the script and returns only after the run directory is complete.
// A non-zero exit is NOT a Go error: it is an outcome, carried in Result.Run.
// An error is returned only when the script could not be run at all.
func (r *Runner) Run(ctx context.Context, spec Spec) (*Result, error) {
	started := time.Now()
	runID := fmt.Sprintf("%s-%s", started.UTC().Format("150405.000"), randSuffix())
	dir := filepath.Join(r.Root, started.UTC().Format("2006-01-02"), runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}

	run := model.Run{
		ID: runID, Source: spec.Source, Kind: spec.Kind, Script: spec.Script,
		From: spec.From, To: spec.To, StartedAt: started, Dir: dir, Item: spec.Item,
	}
	if spec.Item != nil {
		run.ItemID = spec.Item.ID
	}

	logPath := filepath.Join(dir, "log.txt")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	defer logFile.Close()

	var (
		mu    sync.Mutex
		lines []LogLine
	)
	emit := func(stream, text string) {
		now := time.Now()
		fmt.Fprintf(logFile, "%s %-6s %s\n", now.UTC().Format("15:04:05.000"), stream, text)
		line := LogLine{At: now, Stream: stream, Text: capLine(text)}
		mu.Lock()
		lines = append(lines, line)
		if len(lines) > maxLogLines {
			lines = lines[len(lines)-maxLogLines:]
		}
		mu.Unlock()
		if r.OnLog != nil {
			r.OnLog(runID, line)
		}
	}
	// persist writes meta.json and, on failure, makes that failure part of the
	// run's own record instead of a record that looks like a clean success —
	// a full disk must never be silently indistinguishable from nothing going
	// wrong at all.
	persist := func() {
		if err := writeMeta(&run, dir); err != nil {
			msg := fmt.Sprintf("persist meta.json: %v", err)
			if run.Error == "" {
				run.Error = msg
			}
			emit("engine", msg)
		}
	}

	// Recorded before the script starts, so a killed run still says what it was.
	run.Outcome = model.OutcomeRunning
	persist()

	script := spec.Script
	if spec.Inline != "" {
		script = filepath.Join(dir, "script")
		if err := os.WriteFile(script, []byte(spec.Inline), 0o755); err != nil {
			return nil, fmt.Errorf("write inline script: %w", err)
		}
		run.Script = script
	}

	stdinJSON := []byte("{}")
	if spec.Stdin != nil {
		b, err := json.MarshalIndent(spec.Stdin, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshal stdin: %w", err)
		}
		stdinJSON = b
	}
	if err := os.WriteFile(filepath.Join(dir, "stdin.json"), stdinJSON, 0o644); err != nil {
		return nil, err
	}

	// A timeout must kill the whole process group: an AI stage script spawns
	// children, and killing only the parent leaves them running and holding the
	// source's lock forever.
	//
	// Built before the environment, not after, so the deadline handed to the
	// script is the one the context will actually enforce rather than a second
	// calculation of it.
	runCtx := ctx
	var cancel context.CancelFunc
	if spec.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	deadline, _ := runCtx.Deadline()

	resultPath := filepath.Join(dir, "result.json")
	env, envMap := buildEnv(spec, resultPath, deadline)
	run.Env = envMap

	cmd := exec.Command(script)
	cmd.Dir = spec.Workdir
	cmd.Env = env
	cmd.Stdin = bytesReader(stdinJSON)
	setPgid(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		emit("engine", "failed to start: "+err.Error())
		run.Error = err.Error()
		run.Outcome = model.OutcomeFailure
		run.ExitCode = -1
		r.finish(&run, started, persist)
		res := &Result{Run: run, Log: lines}
		if r.OnResult != nil {
			r.OnResult(res)
		}
		return res, fmt.Errorf("start %s: %w", script, err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); scan(stdout, "stdout", emit) }()
	go func() { defer wg.Done(); scan(stderr, "stderr", emit) }()

	// Kill the group on deadline, escalating TERM -> KILL so a script that
	// ignores TERM cannot hold the source's lock forever. `done` stops the
	// watcher when the process exits normally, so nothing is leaked.
	killed := make(chan struct{})
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-done:
			return
		case <-runCtx.Done():
			if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
				close(killed)
			}
			killGroup(cmd, syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(gracePeriod):
				killGroup(cmd, syscall.SIGKILL)
			}
		}
	}()

	wg.Wait()
	waitErr := cmd.Wait()

	timedOut := false
	select {
	case <-killed:
		timedOut = true
		// A well-behaved parent traps TERM and exits at once, which closes
		// `done` before the escalation goroutine can fire. Any child that
		// ignored TERM would then survive, so sweep the group unconditionally.
		killGroup(cmd, syscall.SIGKILL)
		emit("engine", fmt.Sprintf("killed after %s (timeout)", spec.Timeout))
	default:
	}

	run.ExitCode = exitCode(waitErr)
	run.TimedOut = timedOut
	run.Outcome = model.OutcomeFor(run.ExitCode, timedOut)
	r.finish(&run, started, persist)

	res := &Result{Run: run, Log: lines}
	if b, err := os.ReadFile(resultPath); err == nil && len(b) > 0 {
		if json.Valid(b) {
			res.Data = json.RawMessage(b)
		} else {
			// Malformed result is worth surfacing: the script thought it was
			// producing data. It does not change the outcome.
			emit("engine", "result.json is not valid JSON; ignoring")
		}
	}
	if r.OnResult != nil {
		r.OnResult(res)
	}
	return res, nil
}

func (r *Runner) finish(run *model.Run, started time.Time, persist func()) {
	run.FinishedAt = time.Now()
	run.Duration = run.FinishedAt.Sub(started)
	persist()
}

// writeMeta is called twice: once before the process starts and again when it
// ends. A run killed mid-flight — a timeout, a crash, an interrupted session —
// otherwise leaves a zero-byte meta.json and no record of what it was doing,
// which is exactly the run someone needs to read afterwards.
//
// The write is atomic: marshalled to a temp file in the same directory (so
// the rename is on one filesystem) and renamed into place, so a write that
// fails partway — a full disk — leaves the previous meta.json intact rather
// than truncated, and a caller can tell the failure apart from success instead
// of it being silently dropped.
func writeMeta(run *model.Run, dir string) error {
	b, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".meta-*.json.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, writeErr := tmp.Write(b)
	closeErr := tmp.Close()
	if writeErr != nil || closeErr != nil {
		os.Remove(tmpName)
		if writeErr != nil {
			return writeErr
		}
		return closeErr
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, "meta.json")); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// capLine bounds a line kept in memory and published live: a single line over
// maxLineBytes is truncated with an explicit marker. log.txt, written before
// this is ever called, always has the line in full.
func capLine(text string) string {
	if len(text) <= maxLineBytes {
		return text
	}
	return fmt.Sprintf("%s... [truncated, %d more bytes]", text[:maxLineBytes], len(text)-maxLineBytes)
}

// scan reads lines without a length limit: an AI script can emit a single very
// long line, and bufio.Scanner would fail the whole stream on it.
func scan(rc io.Reader, stream string, emit func(string, string)) {
	br := bufio.NewReader(rc)
	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			emit(stream, trimNewline(line))
		}
		if err != nil {
			return
		}
	}
}

func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func buildEnv(spec Spec, resultPath string, deadline time.Time) ([]string, map[string]string) {
	own := map[string]string{
		"CONVEYOR_RESULT":  resultPath,
		"CONVEYOR_WORKDIR": spec.Workdir,
		"CONVEYOR_SOURCE":  spec.Source,
		"CONVEYOR_STAGE":   spec.To,
	}
	// When the process group will be killed, as an instant rather than a
	// duration.
	//
	// A duration is only useful to something that knows when it started, and an
	// agent sixty turns into a run does not: it cannot feel elapsed time and has
	// no reason to have kept count. An absolute timestamp it can check against
	// `date` at any point costs it one command to know exactly where it stands.
	//
	// Absent when the stage has no timeout, which is a real state and not zero:
	// an adapter reading this must treat empty as "no deadline", never as one
	// that has already passed.
	if !deadline.IsZero() {
		own["CONVEYOR_DEADLINE"] = deadline.UTC().Format(time.RFC3339)
	}
	if spec.Item != nil {
		own["CONVEYOR_ITEM_ID"] = spec.Item.ID
		own["CONVEYOR_ITEM_REF"] = spec.Item.Ref
	}
	for k, v := range spec.Env {
		own[k] = v
	}
	env := os.Environ()
	for k, v := range own {
		env = append(env, k+"="+v)
	}
	return env, own
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// SweepInterrupted settles runs left claiming to be running. Nothing this
// process started is alive yet, and only one scheduler works a source at a
// time, so a run still marked running at startup was killed — a crash, a
// timeout the process did not survive, a session closed mid-stage.
//
// Recording that is the difference between a history that shows two runs in
// flight at once, which cannot happen, and one that says which was abandoned.
func SweepInterrupted(root string) (int, error) {
	days, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n := 0
	var errs []error
	for _, day := range days {
		if !day.IsDir() {
			continue
		}
		dayDir := filepath.Join(root, day.Name())
		entries, err := os.ReadDir(dayDir)
		if err != nil {
			errs = append(errs, fmt.Errorf("read %s: %w", dayDir, err))
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(dayDir, e.Name())
			meta := filepath.Join(dir, "meta.json")
			b, err := os.ReadFile(meta)
			if err != nil {
				// A run dir with no meta.json at all (mkdir happened, nothing
				// was ever written) is not a fault worth reporting; anything
				// else — permission denied, an I/O error — is.
				if !os.IsNotExist(err) {
					errs = append(errs, fmt.Errorf("read %s: %w", meta, err))
				}
				continue
			}
			if len(b) == 0 {
				continue
			}
			var run model.Run
			if json.Unmarshal(b, &run) != nil || run.Outcome != model.OutcomeRunning {
				continue
			}
			run.Outcome = model.OutcomeInterrupted
			if run.FinishedAt.IsZero() {
				run.FinishedAt = run.StartedAt
			}
			if err := writeMeta(&run, dir); err != nil {
				errs = append(errs, fmt.Errorf("write %s: %w", meta, err))
				continue
			}
			n++
		}
	}
	return n, errors.Join(errs...)
}
