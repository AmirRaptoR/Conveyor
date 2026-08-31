package config

import (
	"strings"
	"testing"
)

func TestAPasswordRoundTrips(t *testing.T) {
	h, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	a := Auth{Users: map[string]string{"amir": h}}
	if !a.Check("amir", "correct horse battery staple") {
		t.Error("the right password was rejected")
	}
	if a.Check("amir", "correct horse battery stapl") {
		t.Error("a wrong password was accepted")
	}
	if a.Check("someone", "correct horse battery staple") {
		t.Error("an unknown user was accepted")
	}

	// Salted: the same password twice is two different lines, so the config
	// does not show that two users chose the same one.
	h2, err := Hash("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if h == h2 {
		t.Error("two hashes of one password are identical; the salt is not doing anything")
	}
}

// A typo in a hash has to be a startup error. The alternative is a board that
// looks fine and refuses the only person who can fix it, at whatever hour they
// next try to open it.
func TestABadHashIsRefusedAtLoad(t *testing.T) {
	cases := map[string]string{
		"plaintext":       "hunter2",
		"wrong scheme":    "bcrypt$12$abc$def",
		"missing a field": "pbkdf2-sha256$600000$c2FsdA",
		"bad iterations":  "pbkdf2-sha256$never$c2FsdA$a2V5",
		"salt not base64": "pbkdf2-sha256$600000$!!!$a2V5",
	}
	for name, bad := range cases {
		problems := Auth{Users: map[string]string{"amir": bad}}.validate()
		if len(problems) != 1 {
			t.Errorf("%s (%q): %d problem(s), want 1", name, bad, len(problems))
			continue
		}
		// And it says what to do about it, because the person reading this is
		// locked out of the thing that would have told them.
		if !strings.Contains(problems[0], "conveyor passwd") {
			t.Errorf("%s: %q does not say how to fix it", name, problems[0])
		}
	}

	if problems := (Auth{}).validate(); len(problems) != 0 {
		t.Errorf("an empty auth block is not a problem, got %v", problems)
	}
}

// The parameters live in the line, so raising the cost later leaves every hash
// already written still verifiable.
func TestAHashCarriesItsOwnParameters(t *testing.T) {
	// 1000 iterations: a line written by some earlier, cheaper build.
	old := "pbkdf2-sha256$1000$c2FsdHNhbHRzYWx0c2E$" // key filled in below
	dk, err := hashWith(old+"AAAA", "letmein")
	if err != nil {
		t.Fatal(err)
	}
	a := Auth{Users: map[string]string{"amir": dk}}
	if !a.Check("amir", "letmein") {
		t.Error("a hash at a different iteration count no longer verifies")
	}
	if !strings.HasPrefix(dk, "pbkdf2-sha256$1000$") {
		t.Errorf("re-derivation did not keep the recorded parameters: %q", dk)
	}
}
