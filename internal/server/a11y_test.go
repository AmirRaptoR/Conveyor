package server

// Static checks against the board's markup and stylesheet, run against the
// same //go:embed FS the server actually serves (see webFS in server.go) so
// this fails the moment the shipped file regresses, not just a copy of it.
// These are the criteria from #44 that a Go test — no DOM, no JS runtime —
// can actually decide: the accessibility regressions the issue describes are
// all things a static read of the markup can catch.

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func boardHTML(t *testing.T) string {
	t.Helper()
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatalf("reading web/index.html from the embedded FS: %v", err)
	}
	return string(b)
}

func styleBlock(t *testing.T, html string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)<style>(.*?)</style>`).FindStringSubmatch(html)
	if m == nil {
		t.Fatal("no <style> block found in index.html")
	}
	return m[1]
}

// #log and #history must carry no aria-live: a running agent otherwise has
// every appended log line, and every history rebuild, read aloud (see #44's
// problem statement). Checked against the tag itself, not just "the string
// aria-live does not appear anywhere", so a stray aria-live on some other
// element cannot hide a regression here.
func TestLogAndHistoryCarryNoAriaLive(t *testing.T) {
	html := boardHTML(t)
	for _, id := range []string{"log", "history"} {
		tag := regexp.MustCompile(`<div[^>]*\bid="` + id + `"[^>]*>`).FindString(html)
		if tag == "" {
			t.Fatalf("no element with id=%q found", id)
		}
		if strings.Contains(tag, "aria-live") {
			t.Errorf("#%s still carries aria-live: %s", id, tag)
		}
	}
}

// Once #log and #history stop being live regions, something else has to
// carry the run-level announcements (setLogStatus, setHistoryStatus, a stop
// appearing) and the keyboard-move announcement — a polite live region that
// is neither of those two.
func TestAnnouncerLiveRegionExists(t *testing.T) {
	html := boardHTML(t)
	tag := regexp.MustCompile(`<[^>]*\bid="announcer"[^>]*>`).FindString(html)
	if tag == "" {
		t.Fatal("no element with id=\"announcer\" found")
	}
	if !regexp.MustCompile(`aria-live="polite"`).MatchString(tag) {
		t.Errorf("#announcer is not a polite live region: %s", tag)
	}
	if strings.Contains(tag, `id="log"`) || strings.Contains(tag, `id="history"`) {
		t.Error("#announcer must not be #log or #history")
	}
}

// A placeholder is not an accessible name (it disappears the moment there is
// a value, and many assistive technologies never announce it as a name at
// all) — the reply textarea needs a real one, and the existing placeholder
// text has to stay exactly as it was.
func TestReplyTextareaHasAccessibleName(t *testing.T) {
	html := boardHTML(t)
	m := regexp.MustCompile(`<textarea class="answer"[^>]*>`).FindString(html)
	if m == "" {
		t.Fatal("no textarea.answer found")
	}
	if !regexp.MustCompile(`aria-label="[^"]+"`).MatchString(m) {
		t.Errorf("textarea.answer has no aria-label: %s", m)
	}
	if !strings.Contains(m, "placeholder=") {
		t.Error("the existing placeholder was removed, not just supplemented")
	}
}

// The keyboard/touch reorder and start controls (#44) must be real touch
// targets wherever they are offered — at least 44x44 CSS px — and that has
// to hold unconditionally, not only for a mouse: gating it behind a
// hover/pointer media query is exactly what would make it disappear for the
// touch-only device the criterion is about.
func TestActionControlsMeetMinimumHitTarget(t *testing.T) {
	html := boardHTML(t)
	css := styleBlock(t, html)

	sel := ".pactions .ctl"
	idx := strings.Index(css, sel)
	if idx < 0 {
		t.Fatalf("no CSS rule for %q", sel)
	}
	open := strings.Index(css[idx:], "{")
	if open < 0 {
		t.Fatalf("rule for %q has no body", sel)
	}
	open += idx
	closeIdx := matchingBrace(css, open)
	if closeIdx < 0 {
		t.Fatalf("rule for %q is never closed", sel)
	}
	body := css[open+1 : closeIdx]

	minPx := regexp.MustCompile(`min-(?:width|height)\s*:\s*(\d+(?:\.\d+)?)px`)
	matches := minPx.FindAllStringSubmatch(body, -1)
	if len(matches) < 2 {
		t.Fatalf("expected both min-width and min-height on %q, got: %s", sel, body)
	}
	for _, m := range matches {
		px, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatalf("parsing %q: %v", m[1], err)
		}
		if px < 44 {
			t.Errorf("%q sets a minimum of %vpx, want >= 44px", sel, px)
		}
	}

	if insideHoverOrPointerMedia(css, open) {
		t.Errorf("%q's 44x44 rule is gated inside a hover/pointer media query", sel)
	}
}

// insideHoverOrPointerMedia reports whether byte offset pos in css falls
// inside an @media block whose condition mentions "hover" or "pointer" —
// walked with simple brace counting, which is all this stylesheet ever
// nests (no @media inside @media here).
func insideHoverOrPointerMedia(css string, pos int) bool {
	mediaRe := regexp.MustCompile(`@media[^{]*\{`)
	for _, loc := range mediaRe.FindAllStringIndex(css, -1) {
		start, braceStart := loc[0], loc[1]-1
		end := matchingBrace(css, braceStart)
		if end < 0 || pos < braceStart || pos > end {
			continue
		}
		cond := css[start:braceStart]
		if strings.Contains(cond, "hover") || strings.Contains(cond, "pointer") {
			return true
		}
	}
	return false
}

// matchingBrace returns the index of the "}" that closes the "{" at open, or
// -1 if the braces never balance.
func matchingBrace(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
