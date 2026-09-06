package server

import "fmt"

// Mode is what this process is willing to do to the pipeline. It replaces the
// boolean -watch used to carry: auto drives the pipeline itself, manual only
// ever moves an item when the tick button is pressed, and observe never runs a
// stage script, a move or a doctor script at all — CONTRACTS' "watching only".
type Mode string

const (
	ModeAuto    Mode = "auto"
	ModeManual  Mode = "manual"
	ModeObserve Mode = "observe"
)

// Runs reports whether this mode ever launches a transition on its own, absent
// the tick button — the scheduler and the retryStalled sweep both gate on it.
func (m Mode) Runs() bool { return m == ModeAuto }

// Mutates reports whether this mode allows any mutation route to do anything
// at all. Observe refuses every one of them with 403.
func (m Mode) Mutates() bool { return m != ModeObserve }

// Settles reports whether a run left "running" by a previous process should
// be marked interrupted at startup. Observe leaves it alone — settling is
// itself a write to provider state, which observe never does.
func (m Mode) Settles() bool { return m != ModeObserve }

// ParseMode validates a -mode flag value, listing the three names it accepts
// when it does not recognise one.
func ParseMode(s string) (Mode, error) {
	switch Mode(s) {
	case ModeAuto, ModeManual, ModeObserve:
		return Mode(s), nil
	default:
		return "", fmt.Errorf("unknown mode %q; want one of auto, manual, observe", s)
	}
}
