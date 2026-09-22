// Package release describes and verifies an immutable Conveyor installation.
// Development builds need no manifest. A production process opts in by setting
// CONVEYOR_RELEASE_DIR to the directory containing conveyor, agents/, providers/
// and release.json; every byte and executable bit is then checked at startup.
package release

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
)

const (
	EnvDir         = "CONVEYOR_RELEASE_DIR"
	ManifestName   = "release.json"
	ManifestSchema = 1
	ConfigSchema   = 1
)

// BuildRevision is populated by the release build's -ldflags. Source builds
// fall back to Go's VCS build information so `conveyor version` stays useful.
var BuildRevision string

type File struct {
	SHA256 string      `json:"sha256"`
	Mode   os.FileMode `json:"mode"`
}

type Manifest struct {
	Schema       int             `json:"schema"`
	Revision     string          `json:"revision"`
	ConfigSchema int             `json:"configSchema"`
	Files        map[string]File `json:"files"`
}

// Info is safe to expose from /api/state. Dir intentionally names the active
// immutable release so an operator can distinguish a stale service process
// from the current symlink without shell access.
type Info struct {
	Managed        bool   `json:"managed"`
	Revision       string `json:"revision"`
	Modified       bool   `json:"modified,omitempty"`
	Dir            string `json:"dir,omitempty"`
	ManifestSchema int    `json:"manifestSchema,omitempty"`
	ConfigSchema   int    `json:"configSchema"`
}

func Current() Info {
	rev := strings.TrimSpace(BuildRevision)
	modified := false
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if rev == "" {
					rev = s.Value
				}
			case "vcs.modified":
				modified = s.Value == "true"
			}
		}
	}
	if rev == "" {
		rev = "development"
	}
	return Info{Revision: rev, Modified: modified, ConfigSchema: ConfigSchema}
}

// Detect verifies the release selected for this process. With no environment
// variable it reports an unmanaged development build and performs no checks.
func Detect() (Info, error) {
	info := Current()
	dir := strings.TrimSpace(os.Getenv(EnvDir))
	if dir == "" {
		return info, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return Info{}, fmt.Errorf("release: locate executable: %w", err)
	}
	return Verify(dir, exe, info.Revision)
}

// Generate inventories a staged release. The result is deterministic: paths
// are slash-separated and JSON's map encoder sorts its keys.
func Generate(dir, revision string) (Manifest, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return Manifest{}, err
	}
	revision = strings.TrimSpace(revision)
	if revision == "" || revision == "development" {
		return Manifest{}, errors.New("release: a production revision is required")
	}
	files := make(map[string]File)
	for _, rel := range []string{"conveyor", "agents", "providers"} {
		root := filepath.Join(dir, rel)
		fi, err := os.Lstat(root)
		if err != nil {
			return Manifest{}, fmt.Errorf("release: required %s: %w", rel, err)
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return Manifest{}, fmt.Errorf("release: %s is a symlink", rel)
		}
		if rel == "conveyor" && !fi.Mode().IsRegular() {
			return Manifest{}, fmt.Errorf("release: conveyor is not a regular file")
		}
		if rel != "conveyor" && !fi.IsDir() {
			return Manifest{}, fmt.Errorf("release: %s is not a directory", rel)
		}
		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			fi, err := entry.Info()
			if err != nil {
				return err
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("release: %s is a symlink", path)
			}
			if fi.IsDir() {
				return nil
			}
			if !fi.Mode().IsRegular() {
				return fmt.Errorf("release: %s is not a regular file", path)
			}
			digest, err := hashFile(path)
			if err != nil {
				return err
			}
			relPath, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			files[filepath.ToSlash(relPath)] = File{SHA256: digest, Mode: fi.Mode().Perm()}
			return nil
		})
		if err != nil {
			return Manifest{}, err
		}
	}
	return Manifest{Schema: ManifestSchema, Revision: revision, ConfigSchema: ConfigSchema, Files: files}, nil
}

// Verify proves that the running executable and all executable assets are the
// exact release described by release.json. executable is a parameter so the
// invariants can be unit-tested without spawning another process.
func Verify(dir, executable, revision string) (Info, error) {
	realDir, err := canonicalDir(dir)
	if err != nil {
		return Info{}, fmt.Errorf("release: %w", err)
	}
	realExe, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return Info{}, fmt.Errorf("release: executable: %w", err)
	}
	realExe, err = filepath.Abs(realExe)
	if err != nil {
		return Info{}, err
	}
	if realExe != filepath.Join(realDir, "conveyor") {
		return Info{}, fmt.Errorf("release: executable %s is not %s", realExe, filepath.Join(realDir, "conveyor"))
	}
	raw, err := os.ReadFile(filepath.Join(realDir, ManifestName))
	if err != nil {
		return Info{}, fmt.Errorf("release: read %s: %w", ManifestName, err)
	}
	var want Manifest
	if err := json.Unmarshal(raw, &want); err != nil {
		return Info{}, fmt.Errorf("release: parse %s: %w", ManifestName, err)
	}
	if want.Schema != ManifestSchema {
		return Info{}, fmt.Errorf("release: manifest schema %d, binary requires %d", want.Schema, ManifestSchema)
	}
	if want.ConfigSchema != ConfigSchema {
		return Info{}, fmt.Errorf("release: config schema %d, binary requires %d", want.ConfigSchema, ConfigSchema)
	}
	if want.Revision != revision {
		return Info{}, fmt.Errorf("release: manifest revision %q does not match binary revision %q", want.Revision, revision)
	}
	got, err := Generate(realDir, revision)
	if err != nil {
		return Info{}, err
	}
	if !reflect.DeepEqual(want.Files, got.Files) {
		return Info{}, describeDifference(want.Files, got.Files)
	}
	return Info{Managed: true, Revision: revision, Dir: realDir, ManifestSchema: ManifestSchema, ConfigSchema: ConfigSchema}, nil
}

func canonicalDir(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("release directory: %w", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("release directory: %w", err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("release directory %s is not a directory", dir)
	}
	return dir, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("release: hash %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("release: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func describeDifference(want, got map[string]File) error {
	keys := make(map[string]bool, len(want)+len(got))
	for k := range want {
		keys[k] = true
	}
	for k := range got {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	for _, k := range ordered {
		w, wok := want[k]
		g, gok := got[k]
		switch {
		case !wok:
			return fmt.Errorf("release: unexpected file %s", k)
		case !gok:
			return fmt.Errorf("release: required file %s is missing", k)
		case w.Mode != g.Mode:
			return fmt.Errorf("release: mode of %s changed from %04o to %04o", k, w.Mode, g.Mode)
		case w.SHA256 != g.SHA256:
			return fmt.Errorf("release: contents of %s do not match the manifest", k)
		}
	}
	return errors.New("release: files do not match the manifest")
}
