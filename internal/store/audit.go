package store

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
	"time"
)

const auditWindow = 7 * 24 * time.Hour

// AuditEvent is structured evidence that run metadata cannot reconstruct.
// Human records count operator interventions; recovery records carry the
// duration from a provider mark to its confirmed removal.
type AuditEvent struct {
	At         time.Time `json:"at"`
	Kind       string    `json:"kind"`
	Action     string    `json:"action"`
	ItemID     string    `json:"itemId,omitempty"`
	By         string    `json:"by,omitempty"`
	DurationNs int64     `json:"durationNs,omitempty"`
	RunID      string    `json:"runId,omitempty"`
	Stage      string    `json:"stage,omitempty"`
	Outcome    string    `json:"outcome,omitempty"`
	NextStage  string    `json:"nextStage,omitempty"`
	BlockKind  string    `json:"blockKind,omitempty"`
	ModelRun   bool      `json:"modelRun,omitempty"`
	Confirmed  bool      `json:"confirmed,omitempty"`
}

type Audit struct {
	path string
	mu   sync.Mutex
}

func OpenAudit(path string) *Audit { return &Audit{path: path} }

// Append rewrites the bounded JSONL snapshot atomically. Existing records are
// never changed; records older than the fixed seven-day metrics window leave
// the snapshot before the new record is appended.
func (a *Audit) Append(event AuditEvent, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	events := a.read()
	cutoff := now.Add(-auditWindow)
	kept := events[:0]
	for _, existing := range events {
		if !existing.At.Before(cutoff) {
			kept = append(kept, existing)
		}
	}
	kept = append(kept, event)
	var out []byte
	for _, existing := range kept {
		line, err := json.Marshal(existing)
		if err != nil {
			return err
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return writeAtomic(a.path, out)
}

func (a *Audit) Since(cutoff time.Time) []AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []AuditEvent
	for _, event := range a.read() {
		if !event.At.Before(cutoff) {
			out = append(out, event)
		}
	}
	return out
}

func (a *Audit) read() []AuditEvent {
	f, err := os.Open(a.path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []AuditEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event AuditEvent
		if json.Unmarshal(scanner.Bytes(), &event) == nil && !event.At.IsZero() {
			out = append(out, event)
		}
	}
	return out
}
