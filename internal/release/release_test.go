package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func staged(t *testing.T, revision string) string {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"agents/opencode", "providers/github"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		"conveyor":              "binary",
		"agents/opencode/run":   "agent",
		"providers/github/list": "provider",
	} {
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Generate(dir, revision)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, ManifestName), append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestVerifyAcceptsCompleteMatchingRelease(t *testing.T) {
	dir := staged(t, "abc123")
	got, err := Verify(dir, filepath.Join(dir, "conveyor"), "abc123")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Managed || got.Revision != "abc123" || got.Dir != dir {
		t.Fatalf("info = %+v", got)
	}
}

func TestVerifyRejectsRevisionMismatch(t *testing.T) {
	dir := staged(t, "abc123")
	_, err := Verify(dir, filepath.Join(dir, "conveyor"), "different")
	if err == nil || !strings.Contains(err.Error(), "does not match binary revision") {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyRejectsMissingOrChangedAssets(t *testing.T) {
	tests := []struct {
		name string
		edit func(string) error
		want string
	}{
		{"missing", func(dir string) error { return os.Remove(filepath.Join(dir, "agents/opencode/run")) }, "required file agents/opencode/run is missing"},
		{"changed", func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "providers/github/list"), []byte("changed"), 0o755)
		}, "contents of providers/github/list"},
		{"unexpected", func(dir string) error { return os.WriteFile(filepath.Join(dir, "agents/new"), []byte("new"), 0o755) }, "unexpected file agents/new"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := staged(t, "abc123")
			if err := tc.edit(dir); err != nil {
				t.Fatal(err)
			}
			_, err := Verify(dir, filepath.Join(dir, "conveyor"), "abc123")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestVerifyRejectsExecutableOutsideRelease(t *testing.T) {
	dir := staged(t, "abc123")
	outside := filepath.Join(t.TempDir(), "conveyor")
	if err := os.WriteFile(outside, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Verify(dir, outside, "abc123")
	if err == nil || !strings.Contains(err.Error(), "is not") {
		t.Fatalf("error = %v", err)
	}
}
