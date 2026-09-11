// Package preflight holds the shape of a readiness check and the logic for
// turning a provider preflight script's — or an agent status script's —
// finished run into one, so conveyor preflight, conveyor enroll and conveyor
// run -explain share one definition of a check and one reading of what a
// script wrote, rather than growing second copies of either.
//
// See docs/CONTRACTS.md §3.
package preflight

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/AmirRaptoR/Conveyor/internal/model"
)

// Status is one check's outcome.
type Status string

const (
	StatusPass    Status = "pass"
	StatusFail    Status = "fail"
	StatusWarn    Status = "warn"
	StatusSkip    Status = "skip"
	StatusUnknown Status = "unknown"
)

// Failed reports whether this status counts against conveyor preflight's
// aggregate exit code: fail and unknown do; pass, warn and skip do not. A
// check whose outcome cannot be read has not passed, which is why unknown is
// here beside fail and not beside warn.
func (s Status) Failed() bool { return s == StatusFail || s == StatusUnknown }

// NormalizeStatus reads a provider preflight script's own word for a check's
// status. Any word outside the closed vocabulary, or an absent one, becomes
// StatusUnknown — CONTRACTS.md §3.
func NormalizeStatus(s string) Status {
	switch Status(s) {
	case StatusPass, StatusFail, StatusWarn, StatusSkip:
		return Status(s)
	default:
		return StatusUnknown
	}
}

// Check is one line of a readiness report: CONTRACTS.md §3's
// {name, status, detail, fix}.
//
// What a script puts into Detail or Fix is that script's own promise, the
// same standing move's own comment on an issue already carries — the engine
// prints and persists it, but does not read, police or rewrite its content.
type Check struct {
	Name   string `json:"name"`
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// ParseChecks reads a provider preflight script's raw $CONVEYOR_RESULT bytes.
//
// ok is false for a malformed envelope — not JSON, not an object, no
// "checks" key, or "checks" not an array — which CONTRACTS.md §3 has the
// caller report as one synthetic fail naming the run directory; this
// function has no run directory to name, so that is the caller's job, not
// this one's.
//
// A malformed *entry* inside an otherwise well-formed array does not fail
// the whole parse: it becomes one Check with StatusUnknown, identified by
// its index, so one bad row does not erase the checks that did come back.
// An entry with no name is named by its index too.
func ParseChecks(data []byte) (checks []Check, ok bool) {
	if len(data) == 0 {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, false
	}
	raw, present := obj["checks"]
	if !present {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false
	}
	out := make([]Check, len(items))
	for i, item := range items {
		var c struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Detail string `json:"detail"`
			Fix    string `json:"fix"`
		}
		if err := json.Unmarshal(item, &c); err != nil {
			out[i] = Check{Name: fmt.Sprintf("check %d", i), Status: StatusUnknown, Detail: "malformed check entry"}
			continue
		}
		name := c.Name
		if name == "" {
			name = fmt.Sprintf("check %d", i)
		}
		out[i] = Check{Name: name, Status: NormalizeStatus(c.Status), Detail: c.Detail, Fix: c.Fix}
	}
	return out, true
}

// FromRun turns one finished provider-preflight run into checks: the
// verdicts the script wrote, whatever they found, when it can be trusted to
// have actually run them — or one synthetic Check naming why not.
func FromRun(run model.Run, data []byte) []Check {
	if run.Outcome != model.OutcomeSuccess {
		return []Check{Synthetic(run)}
	}
	checks, ok := ParseChecks(data)
	if !ok {
		return []Check{{Name: "preflight script", Status: StatusFail,
			Detail: fmt.Sprintf("wrote a malformed or empty result; run: %s", run.Dir)}}
	}
	return checks
}

// Synthetic is what a preflight run that cannot be trusted to have actually
// checked anything becomes: a non-zero exit, a timeout, or (run.Error set) a
// failure to start.
func Synthetic(run model.Run) Check {
	switch {
	case run.TimedOut:
		return Check{Name: "preflight script", Status: StatusFail,
			Detail: fmt.Sprintf("timed out; run: %s", run.Dir)}
	case run.Error != "":
		return Check{Name: "preflight script", Status: StatusFail,
			Detail: fmt.Sprintf("%s; run: %s", run.Error, run.Dir)}
	default:
		return Check{Name: "preflight script", Status: StatusFail,
			Detail: fmt.Sprintf("exited %d; run: %s", run.ExitCode, run.Dir)}
	}
}

// NormalizeAgentState reads a status script's own word for state.
// docs/CONTRACTS.md §3: the engine knows three words, and anything else —
// including absent — is "unknown". One definition, shared by the board's own
// discovery poll (internal/server) and conveyor preflight, so both read the
// one field of an agent's self-report identically.
func NormalizeAgentState(s string) string {
	if s == "ok" || s == "limited" {
		return s
	}
	return "unknown"
}

// AgentCheck turns one agents/<name>/status invocation's outcome into a
// readiness Check, in the same closed vocabulary NormalizeAgentState
// applies: ok -> pass, limited -> warn (carrying the script's summary),
// anything else -> unknown (carrying what went wrong — a non-zero exit, a
// timeout, an empty result, a result that is not a JSON object, or an
// unrecognised state).
func AgentCheck(name string, run model.Run, data json.RawMessage, runErr error) Check {
	fail := func(detail string) Check {
		return Check{Name: name, Status: StatusUnknown, Detail: detail}
	}
	switch {
	case runErr != nil:
		return fail(runErr.Error())
	case run.Outcome != model.OutcomeSuccess:
		return fail(fmt.Sprintf("status exited %d (%s)", run.ExitCode, run.Outcome))
	case len(data) == 0:
		return fail("status wrote nothing to $CONVEYOR_RESULT")
	}
	var got struct {
		State   string `json:"state"`
		Summary string `json:"summary"`
	}
	if json.Unmarshal(data, &got) != nil {
		return fail("status result is not a JSON object")
	}
	switch NormalizeAgentState(got.State) {
	case "ok":
		return Check{Name: name, Status: StatusPass, Detail: got.Summary}
	case "limited":
		return Check{Name: name, Status: StatusWarn, Detail: got.Summary}
	default:
		return fail(fmt.Sprintf("unrecognised state %q", got.State))
	}
}

// Render writes checks as the checklist CONTRACTS.md §3 describes: one line
// per check, status upper-cased, the detail beside it, and a fix: line under
// any check that supplied one.
func Render(w io.Writer, checks []Check) {
	for _, c := range checks {
		fmt.Fprintf(w, "  %-7s %s", strings.ToUpper(string(c.Status)), c.Name)
		if c.Detail != "" {
			fmt.Fprintf(w, " — %s", c.Detail)
		}
		fmt.Fprintln(w)
		if c.Fix != "" {
			fmt.Fprintf(w, "          fix: %s\n", c.Fix)
		}
	}
}

// Counts tallies checks by status, for the summary line under a source's
// checklist.
func Counts(checks []Check) map[Status]int {
	out := map[Status]int{}
	for _, c := range checks {
		out[c.Status]++
	}
	return out
}

// AnyFailed reports whether any check in the slice counts as a failure —
// the aggregate conveyor preflight's exit code is built from.
func AnyFailed(checks []Check) bool {
	for _, c := range checks {
		if c.Status.Failed() {
			return true
		}
	}
	return false
}
