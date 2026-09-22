package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func unusableConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "conveyor.yaml")
	body := `version: 1
stages:
  - name: backlog
  - name: done
    terminal: true
sources:
  - name: broken
    provider: missing
    workdir: ./missing
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateStrictSourcesFailsForUnusableSource(t *testing.T) {
	err := cmdValidate([]string{"-strict-sources", "-c", unusableConfig(t)})
	if err == nil || !strings.Contains(err.Error(), "1 of 1 source(s) unusable") {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateDefaultStillReportsWithoutFailing(t *testing.T) {
	if err := cmdValidate([]string{"-c", unusableConfig(t)}); err != nil {
		t.Fatalf("default validate = %v", err)
	}
}
