package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

const (
	auditWindow     = 7 * 24 * time.Hour
	maxAuditRecords = 100000
	maxAuditBytes   = 64 << 20
)

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

type AuditStatus struct {
	Healthy         bool      `json:"healthy"`
	Error           string    `json:"error,omitempty"`
	ContinuityID    string    `json:"continuityId,omitempty"`
	ContinuitySince time.Time `json:"continuitySince,omitempty"`
	Records         int       `json:"records"`
	Bytes           int64     `json:"bytes"`
}

type auditSnapshot struct {
	Schema          int          `json:"schema"`
	ContinuityID    string       `json:"continuityId"`
	ContinuitySince time.Time    `json:"continuitySince"`
	Events          []AuditEvent `json:"events"`
}

type auditEvidence struct {
	Schema       int    `json:"schema"`
	ContinuityID string `json:"continuityId"`
	SHA256       string `json:"sha256"`
}

type Audit struct {
	path         string
	evidence     string
	mu           sync.Mutex
	events       []AuditEvent
	status       AuditStatus
	unstarted    bool
	expectedHash string
	dataMod      time.Time
	markerMod    time.Time
	markerSize   int64
}

func OpenAudit(path string) *Audit {
	a := &Audit{path: path, evidence: path + ".evidence.json"}
	a.load()
	return a
}

// Establish creates the durable continuity identity. It is independent of a
// soak: collecting trustworthy evidence may begin at process startup, while a
// person explicitly decides when a soak begins.
func (a *Audit) Establish(now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.unstarted {
		if a.status.Healthy {
			return nil
		}
		return errors.New(a.status.Error)
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return a.failLocked(fmt.Errorf("create audit continuity identity: %w", err))
	}
	snapshot := auditSnapshot{Schema: 1, ContinuityID: hex.EncodeToString(idBytes), ContinuitySince: now.UTC()}
	if err := a.persistLocked(snapshot); err != nil {
		return err
	}
	a.unstarted = false
	return nil
}

// Append atomically replaces the bounded evidence snapshot. Run events are
// canonical by RunID: a provider-write retry replaces its pending version
// instead of becoming a second run in operational metrics.
func (a *Audit) Append(event AuditEvent, now time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.validateFilesLocked()
	if !a.status.Healthy {
		if a.status.Error == "" {
			return errors.New("audit evidence is not established")
		}
		return errors.New(a.status.Error)
	}
	cutoff := now.Add(-auditWindow)
	next := make([]AuditEvent, 0, len(a.events)+1)
	replaced := false
	for _, existing := range a.events {
		if existing.At.Before(cutoff) {
			continue
		}
		if event.Kind == "run" && event.RunID != "" && existing.Kind == "run" && existing.RunID == event.RunID {
			if !replaced {
				if existing.At.Before(event.At) {
					event.At = existing.At
				}
				next = append(next, event)
				replaced = true
			}
			continue
		}
		next = append(next, existing)
	}
	if !replaced {
		next = append(next, event)
	}
	if len(next) > maxAuditRecords {
		return a.failLocked(fmt.Errorf("audit evidence exceeds %d records", maxAuditRecords))
	}
	snapshot := auditSnapshot{Schema: 1, ContinuityID: a.status.ContinuityID,
		ContinuitySince: a.status.ContinuitySince, Events: next}
	if err := a.persistLocked(snapshot); err != nil {
		return err
	}
	a.events = next
	return nil
}

// Since reads only the validated bounded in-memory projection loaded at open.
func (a *Audit) Since(cutoff time.Time) []AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AuditEvent, 0, len(a.events))
	for _, event := range a.events {
		if !event.At.Before(cutoff) {
			out = append(out, event)
		}
	}
	return out
}

func (a *Audit) Status() AuditStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.validateFilesLocked()
	status := a.status
	status.Records = len(a.events)
	return status
}

func (a *Audit) load() {
	data, dataErr := os.ReadFile(a.path)
	evidenceData, evidenceErr := os.ReadFile(a.evidence)
	if errors.Is(dataErr, os.ErrNotExist) && errors.Is(evidenceErr, os.ErrNotExist) {
		a.unstarted = true
		a.status.Error = "audit evidence is not established"
		return
	}
	if dataErr != nil || evidenceErr != nil {
		a.status.Error = fmt.Sprintf("audit evidence continuity lost: snapshot: %v; marker: %v", dataErr, evidenceErr)
		return
	}
	if len(data) > maxAuditBytes {
		a.status.Error = fmt.Sprintf("audit evidence exceeds %d bytes", maxAuditBytes)
		return
	}
	var snapshot auditSnapshot
	var evidence auditEvidence
	if err := json.Unmarshal(data, &snapshot); err != nil {
		a.status.Error = "invalid audit evidence snapshot: " + err.Error()
		return
	}
	if err := json.Unmarshal(evidenceData, &evidence); err != nil {
		a.status.Error = "invalid audit evidence marker: " + err.Error()
		return
	}
	digest := sha256.Sum256(data)
	if snapshot.Schema != 1 || evidence.Schema != 1 || snapshot.ContinuityID == "" ||
		snapshot.ContinuityID != evidence.ContinuityID || evidence.SHA256 != hex.EncodeToString(digest[:]) || snapshot.ContinuitySince.IsZero() {
		a.status.Error = "audit evidence marker does not match the snapshot"
		return
	}
	if len(snapshot.Events) > maxAuditRecords {
		a.status.Error = fmt.Sprintf("audit evidence exceeds %d records", maxAuditRecords)
		return
	}
	for i, event := range snapshot.Events {
		if event.At.IsZero() || event.Kind == "" {
			a.status.Error = fmt.Sprintf("invalid audit evidence event %d", i)
			return
		}
	}
	a.events = snapshot.Events
	a.expectedHash = evidence.SHA256
	a.status = AuditStatus{Healthy: true, ContinuityID: snapshot.ContinuityID,
		ContinuitySince: snapshot.ContinuitySince, Records: len(snapshot.Events), Bytes: int64(len(data))}
	a.rememberFilesLocked()
}

func (a *Audit) persistLocked(snapshot auditSnapshot) error {
	data, err := json.Marshal(snapshot)
	if err != nil {
		return a.failLocked(err)
	}
	data = append(data, '\n')
	if len(data) > maxAuditBytes {
		return a.failLocked(fmt.Errorf("audit evidence exceeds %d bytes", maxAuditBytes))
	}
	digest := sha256.Sum256(data)
	marker, err := json.Marshal(auditEvidence{Schema: 1, ContinuityID: snapshot.ContinuityID, SHA256: hex.EncodeToString(digest[:])})
	if err != nil {
		return a.failLocked(err)
	}
	if err := writeAtomic(a.path, data); err != nil {
		return a.failLocked(fmt.Errorf("write audit evidence snapshot: %w", err))
	}
	if err := writeAtomic(a.evidence, append(marker, '\n')); err != nil {
		return a.failLocked(fmt.Errorf("write audit evidence marker: %w", err))
	}
	a.status = AuditStatus{Healthy: true, ContinuityID: snapshot.ContinuityID,
		ContinuitySince: snapshot.ContinuitySince, Records: len(snapshot.Events), Bytes: int64(len(data))}
	a.expectedHash = hex.EncodeToString(digest[:])
	a.rememberFilesLocked()
	return nil
}

func (a *Audit) rememberFilesLocked() {
	if info, err := os.Stat(a.path); err == nil {
		a.dataMod = info.ModTime()
		a.status.Bytes = info.Size()
	}
	if info, err := os.Stat(a.evidence); err == nil {
		a.markerMod = info.ModTime()
		a.markerSize = info.Size()
	}
}

// validateFilesLocked makes deletion and external mutation visible without
// reparsing the event projection. Unchanged files cost two stats; changed files
// are hashed against the last atomically committed marker.
func (a *Audit) validateFilesLocked() {
	if !a.status.Healthy {
		return
	}
	dataInfo, dataErr := os.Stat(a.path)
	markerInfo, markerErr := os.Stat(a.evidence)
	if dataErr != nil || markerErr != nil {
		a.status.Healthy = false
		a.status.Error = fmt.Sprintf("audit evidence continuity lost: snapshot: %v; marker: %v", dataErr, markerErr)
		return
	}
	if dataInfo.Size() == a.status.Bytes && dataInfo.ModTime().Equal(a.dataMod) &&
		markerInfo.Size() == a.markerSize && markerInfo.ModTime().Equal(a.markerMod) {
		return
	}
	data, err := os.ReadFile(a.path)
	if err != nil {
		a.failLocked(fmt.Errorf("read audit evidence snapshot: %w", err))
		return
	}
	marker, err := os.ReadFile(a.evidence)
	if err != nil {
		a.failLocked(fmt.Errorf("read audit evidence marker: %w", err))
		return
	}
	var evidence auditEvidence
	digest := sha256.Sum256(data)
	if json.Unmarshal(marker, &evidence) != nil || evidence.ContinuityID != a.status.ContinuityID ||
		evidence.SHA256 != hex.EncodeToString(digest[:]) || evidence.SHA256 != a.expectedHash {
		a.status.Healthy = false
		a.status.Error = "audit evidence changed outside its validated append path"
		return
	}
	a.rememberFilesLocked()
}

func (a *Audit) failLocked(err error) error {
	a.status.Healthy = false
	a.status.Error = err.Error()
	return err
}
