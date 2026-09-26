package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

type SoakRecord struct {
	Schema     int       `json:"schema"`
	ID         string    `json:"id"`
	Revision   string    `json:"revision"`
	StartedAt  time.Time `json:"startedAt"`
	EvidenceID string    `json:"evidenceId"`
}

func NewSoakRecord(revision, evidenceID string, now time.Time) (SoakRecord, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return SoakRecord{}, fmt.Errorf("create soak identity: %w", err)
	}
	return SoakRecord{Schema: 1, ID: hex.EncodeToString(id), Revision: revision,
		StartedAt: now.UTC(), EvidenceID: evidenceID}, nil
}

type Soak struct {
	path string
	mu   sync.Mutex
	data SoakRecord
}

func OpenSoak(path string) *Soak {
	s := &Soak{path: path}
	b, _ := os.ReadFile(path)
	_ = json.Unmarshal(b, &s.data)
	return s
}

func (s *Soak) Get() SoakRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

func (s *Soak) Set(record SoakRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.Schema = 1
	b, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(s.path, append(b, '\n')); err != nil {
		return err
	}
	s.data = record
	return nil
}
