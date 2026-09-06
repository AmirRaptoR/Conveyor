package server

// The board is no longer one file: index.html links a stylesheet per section
// of the design and loads one ES module, which imports the rest. None of that
// is visible to a Go test unless something reads it, and every one of those
// files is a silent 404 away from a blank page — including a filename that
// begins with "_" or ".", which //go:embed omits without a word.
//
// So the list is derived, never written down: index.html is parsed out of the
// same embedded FS the server serves from, and each module it names is walked
// for its own imports. A stylesheet or a module added later is covered here
// without editing this file, and TestEveryRouteIsBehindTheSameWall reuses the
// same list to prove none of it is readable without a password.

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var (
	// href/src on a <link> or <script>, taken from the tag rather than from
	// the whole file so a URL inside a comment or a string is not mistaken
	// for an asset the page loads.
	linkTagRe   = regexp.MustCompile(`<link\b[^>]*>`)
	scriptTagRe = regexp.MustCompile(`<script\b[^>]*>`)
	urlAttrRe   = regexp.MustCompile(`\b(?:href|src)="([^"]+)"`)
	// A module's own static imports: `import ... from "./x.js";` and the
	// side-effect form `import "./x.js";`.
	importRe = regexp.MustCompile(`(?m)^import\b[^"']*["']([^"']+)["']`)
)

// pageAssets returns every local path the board loads, in the order it reaches
// them: each <link href>/<script src> in index.html, then the modules those
// scripts import, transitively.
func pageAssets(t *testing.T) []string {
	t.Helper()
	var out []string
	seen := map[string]bool{}

	add := func(path string) bool {
		if path == "" || !strings.HasPrefix(path, "/") || seen[path] {
			return false
		}
		seen[path] = true
		out = append(out, path)
		return true
	}

	html := boardHTML(t)
	tags := linkTagRe.FindAllString(html, -1)
	tags = append(tags, scriptTagRe.FindAllString(html, -1)...)
	for _, tag := range tags {
		m := urlAttrRe.FindStringSubmatch(tag)
		if m == nil {
			continue
		}
		add(m[1])
	}

	// Walk the module graph. `out` grows as imports are found, which is what
	// makes this transitive without a second structure.
	for i := 0; i < len(out); i++ {
		if !strings.HasSuffix(out[i], ".js") && !strings.HasSuffix(out[i], ".mjs") {
			continue
		}
		b, err := webFS.ReadFile("web" + out[i])
		if err != nil {
			// Reported by the serving test below, which is where a missing
			// file belongs; nothing more can be walked from here.
			continue
		}
		for _, m := range importRe.FindAllStringSubmatch(string(b), -1) {
			add("/" + strings.TrimPrefix(strings.TrimPrefix(m[1], "./"), "/"))
		}
	}
	return out
}

// Every asset the board loads is actually served, with a type a browser will
// act on: a stylesheet served as text/plain is ignored, and a module served as
// anything but a JavaScript type is refused outright.
func TestEveryAssetTheBoardLoadsIsServed(t *testing.T) {
	assets := pageAssets(t)
	if len(assets) < 2 {
		t.Fatalf("parsed %d assets out of index.html; the page loads more than that", len(assets))
	}

	h, err := webHandler()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range assets {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200 (a file //go:embed skipped, or one that moved)", path, w.Code)
			continue
		}
		ct := w.Header().Get("Content-Type")
		var want string
		switch {
		case strings.HasSuffix(path, ".css"):
			want = "text/css"
		case strings.HasSuffix(path, ".js"), strings.HasSuffix(path, ".mjs"):
			want = "text/javascript"
		default:
			continue // icons and the manifest are the app shell, not page code
		}
		if !strings.HasPrefix(ct, want) {
			t.Errorf("GET %s Content-Type = %q, want %s", path, ct, want)
		}
	}
}

// //go:embed omits any file whose name begins with "_" or "." without saying
// so: the binary builds, the page loads, and the asset 404s at runtime. The
// rule is cheap to keep and impossible to notice breaking.
func TestNoAssetNameIsSkippedByEmbed(t *testing.T) {
	entries, err := webFS.ReadDir("web")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("the embedded web/ directory is empty")
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "_") || strings.HasPrefix(e.Name(), ".") {
			t.Errorf("web/%s begins with %q: //go:embed skips it and it 404s at runtime",
				e.Name(), e.Name()[:1])
		}
	}
}

// The 600-line ceiling the split was made to keep: a file a reviewer can hold
// at once. Test files are exempt — they are read one case at a time.
func TestNoServedFileIsOversized(t *testing.T) {
	check := func(dir string) {
		entries, err := webFS.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, e := range entries {
			if e.IsDir() || strings.HasSuffix(e.Name(), ".test.mjs") || strings.HasSuffix(e.Name(), "_test.mjs") {
				continue
			}
			if !strings.HasSuffix(e.Name(), ".js") && !strings.HasSuffix(e.Name(), ".css") &&
				!strings.HasSuffix(e.Name(), ".html") {
				continue
			}
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			b, err := webFS.ReadFile(dir + "/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(string(b), "\n"); n >= 600 {
				t.Errorf("%s/%s is %d lines; split it (the ceiling is 600)", dir, name, n)
			}
		}
	}
	check("web")
}
