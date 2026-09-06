package probe

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/push"
)

// The payload is exactly the shape the service worker renders — see
// internal/server/web/sw.js and internal/server/push.go — and names the
// failing origins and their verdicts, not just a generic "something broke".
func TestNotifyPayloadShapeAndContent(t *testing.T) {
	failing := []Verdict{
		{URL: "https://board.example.com", Status: 403, Detail: "host rejected (403) — add it to auth.origins"},
	}
	raw := NotifyPayload(failing)

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	title, _ := decoded["title"].(string)
	body, _ := decoded["body"].(string)
	if title == "" {
		t.Fatal("title is empty")
	}
	if body == "" {
		t.Fatal("body is empty")
	}
	if !strings.Contains(body, "board.example.com") || !strings.Contains(body, "403") {
		t.Fatalf("body %q does not name the failing origin and its verdict", body)
	}
}

// probe must never call push.LoadKeys on a path that does not exist yet —
// LoadKeys mints a key pair and writes it there, and probe must never write
// inside the data directory.
func TestNotifySkipsWithoutExistingVapidFile(t *testing.T) {
	dataDir := t.TempDir()
	msg := Notify(context.Background(), dataDir, []Verdict{{URL: "https://x", Status: 403}})

	if _, err := os.Stat(filepath.Join(dataDir, "vapid.json")); err == nil {
		t.Fatal("vapid.json was created; Notify must never mint a key pair")
	}
	if msg == "" {
		t.Fatal("Notify returned nothing; want one line saying push was unavailable")
	}
}

// An existing vapid.json with no subscribers is reported, not an error, and
// still never writes.
func TestNotifySkipsWithNoSubscribers(t *testing.T) {
	dataDir := t.TempDir()
	if _, err := push.LoadKeys(filepath.Join(dataDir, "vapid.json")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dataDir, "vapid.json"))

	msg := Notify(context.Background(), dataDir, []Verdict{{URL: "https://x", Status: 403}})
	if msg == "" {
		t.Fatal("Notify returned nothing; want one line saying push was unavailable")
	}

	after, _ := os.ReadFile(filepath.Join(dataDir, "vapid.json"))
	if string(before) != string(after) {
		t.Fatal("vapid.json changed; Notify must never write inside the data directory")
	}
	if _, err := os.Stat(filepath.Join(dataDir, "push.json")); err == nil {
		t.Fatal("push.json was created; Notify must never write inside the data directory")
	}
}
