package store

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

type WatchdogState struct {
	LastProgressAt      time.Time `json:"lastProgressAt"`
	IncidentKey         string    `json:"incidentKey,omitempty"`
	DetectedAt          time.Time `json:"detectedAt,omitempty"`
	DeliveryStatus      string    `json:"deliveryStatus,omitempty"`
	DeliveryAttemptedAt time.Time `json:"deliveryAttemptedAt,omitempty"`
	DeliveredAt         time.Time `json:"deliveredAt,omitempty"`
	DeliveryError       string    `json:"deliveryError,omitempty"`
}

type Watchdog struct {
	path string
	mu   sync.Mutex
	data WatchdogState
}

func OpenWatchdog(path string) *Watchdog {
	w := &Watchdog{path: path}
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &w.data)
	return w
}

func (w *Watchdog) Get() WatchdogState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data
}

func (w *Watchdog) Set(next WatchdogState) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(w.path, append(b, '\n')); err != nil {
		return err
	}
	w.data = next
	return nil
}
