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

type WatchdogState struct {
	LastProgressAt      time.Time       `json:"lastProgressAt"`
	IncidentKey         string          `json:"incidentKey,omitempty"`
	DetectedAt          time.Time       `json:"detectedAt,omitempty"`
	DeliveryStatus      string          `json:"deliveryStatus,omitempty"`
	DeliveryAttemptedAt time.Time       `json:"deliveryAttemptedAt,omitempty"`
	DeliveredAt         time.Time       `json:"deliveredAt,omitempty"`
	DeliveryError       string          `json:"deliveryError,omitempty"`
	DeliveryAcks        map[string]bool `json:"deliveryAcks,omitempty"`
}

type WatchdogStatus struct {
	Established bool   `json:"established"`
	Healthy     bool   `json:"healthy"`
	Identity    string `json:"identity,omitempty"`
	Error       string `json:"error,omitempty"`
}

type watchdogSnapshot struct {
	Schema   int           `json:"schema"`
	Identity string        `json:"identity"`
	State    WatchdogState `json:"state"`
}

type watchdogEvidence struct {
	Schema   int    `json:"schema"`
	Identity string `json:"identity"`
	SHA256   string `json:"sha256"`
}

type Watchdog struct {
	path     string
	marker   string
	mu       sync.Mutex
	data     WatchdogState
	status   WatchdogStatus
	expected string
}

func OpenWatchdog(path string) *Watchdog {
	w := &Watchdog{path: path, marker: path + ".evidence.json"}
	w.load()
	return w
}

func (w *Watchdog) Establish(initial WatchdogState) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status.Established {
		if w.status.Healthy {
			return nil
		}
		return errors.New(w.status.Error)
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	w.status = WatchdogStatus{Established: true, Healthy: true, Identity: hex.EncodeToString(id)}
	return w.persistLocked(initial)
}

func (w *Watchdog) Get() WatchdogState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return copyWatchdogState(w.data)
}

func (w *Watchdog) Status() WatchdogStatus {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.validateLocked()
	return w.status
}

func (w *Watchdog) Set(next WatchdogState) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.validateLocked()
	if !w.status.Healthy {
		return errors.New(w.status.Error)
	}
	return w.persistLocked(next)
}

func (w *Watchdog) load() {
	data, dataErr := os.ReadFile(w.path)
	marker, markerErr := os.ReadFile(w.marker)
	if errors.Is(dataErr, os.ErrNotExist) && errors.Is(markerErr, os.ErrNotExist) {
		return
	}
	w.status.Established = true
	if dataErr != nil || markerErr != nil {
		w.status.Error = fmt.Sprintf("watchdog evidence continuity lost: state: %v; marker: %v", dataErr, markerErr)
		return
	}
	var snapshot watchdogSnapshot
	var evidence watchdogEvidence
	if json.Unmarshal(data, &snapshot) != nil || json.Unmarshal(marker, &evidence) != nil {
		w.status.Error = "invalid watchdog evidence"
		return
	}
	digest := sha256.Sum256(data)
	if snapshot.Schema != 1 || evidence.Schema != 1 || snapshot.Identity == "" || snapshot.Identity != evidence.Identity || evidence.SHA256 != hex.EncodeToString(digest[:]) {
		w.status.Error = "watchdog evidence marker does not match state"
		return
	}
	w.data = copyWatchdogState(snapshot.State)
	w.status = WatchdogStatus{Established: true, Healthy: true, Identity: snapshot.Identity}
	w.expected = evidence.SHA256
}

func (w *Watchdog) persistLocked(next WatchdogState) error {
	snapshot := watchdogSnapshot{Schema: 1, Identity: w.status.Identity, State: copyWatchdogState(next)}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	marker, _ := json.Marshal(watchdogEvidence{Schema: 1, Identity: w.status.Identity, SHA256: hash})
	if err := writeAtomic(w.path, data); err != nil {
		w.status.Healthy, w.status.Error = false, err.Error()
		return err
	}
	if err := writeAtomic(w.marker, append(marker, '\n')); err != nil {
		w.status.Healthy, w.status.Error = false, err.Error()
		return err
	}
	w.data = copyWatchdogState(next)
	w.expected = hash
	w.status.Healthy, w.status.Error = true, ""
	return nil
}

func (w *Watchdog) validateLocked() {
	if !w.status.Healthy {
		return
	}
	data, err := os.ReadFile(w.path)
	if err != nil {
		w.status.Healthy, w.status.Error = false, "watchdog state missing after establishment: "+err.Error()
		return
	}
	marker, err := os.ReadFile(w.marker)
	if err != nil {
		w.status.Healthy, w.status.Error = false, "watchdog marker missing after establishment: "+err.Error()
		return
	}
	var evidence watchdogEvidence
	digest := sha256.Sum256(data)
	if json.Unmarshal(marker, &evidence) != nil || evidence.Identity != w.status.Identity || evidence.SHA256 != hex.EncodeToString(digest[:]) || evidence.SHA256 != w.expected {
		w.status.Healthy, w.status.Error = false, "watchdog evidence changed outside its validated write path"
	}
}

func copyWatchdogState(state WatchdogState) WatchdogState {
	out := state
	if state.DeliveryAcks != nil {
		out.DeliveryAcks = make(map[string]bool, len(state.DeliveryAcks))
		for endpoint, delivered := range state.DeliveryAcks {
			out.DeliveryAcks[endpoint] = delivered
		}
	}
	return out
}
