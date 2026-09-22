package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/release"
)

func TestStateExposesVerifiedReleaseIdentity(t *testing.T) {
	cfg, r := boardFor(t)
	want := release.Info{
		Managed: true, Revision: "abc123", Dir: "/opt/conveyor/releases/abc123",
		ManifestSchema: 1, ConfigSchema: 1,
	}
	s := New(cfg, r, want)
	w := httptest.NewRecorder()
	s.handleState(w, httptest.NewRequest("GET", "/api/state", nil))
	var got State
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Release != want {
		t.Fatalf("release = %+v, want %+v", got.Release, want)
	}
}
