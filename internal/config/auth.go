package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"crypto/pbkdf2"
)

// Auth is who may see the board.
//
// It lives here rather than in front of the process because the reverse proxy
// that used to do it was a second server, a second config file and a second
// password store for one line of behaviour — and the one thing it added,
// terminating the connection somewhere else, is not something a board bound to
// one machine needs. Removing it removes a whole component from the system.
//
// Empty means no authentication, which is only allowed on a loopback address;
// Server.Run refuses anything else. That refusal is the point of the type: a
// board is a control plane — it starts agents, reorders work and hands items
// back — and reaching it is enough to drive the pipeline.
type Auth struct {
	// Realm is what the browser shows in its prompt. Cosmetic.
	Realm string `yaml:"realm"`
	// Users maps a name to a password hash in the format Hash writes. Plaintext
	// is rejected at load rather than accepted with a warning: a password in a
	// config file is a password in every backup and every `cat` of that file,
	// and a warning nobody reads is not a control.
	Users map[string]string `yaml:"users"`
}

// Enabled reports whether anything has to authenticate.
func (a Auth) Enabled() bool { return len(a.Users) > 0 }

// Check verifies a name and password in constant time.
//
// The lookup itself is not constant time — a name that does not exist is
// distinguishable from one that does by timing — and that is accepted: the
// usernames on a personal board are not the secret, and pretending otherwise
// would mean hashing against a dummy record for a threat model that does not
// apply here.
func (a Auth) Check(user, pass string) bool {
	want, known := a.Users[user]
	if !known {
		return false
	}
	got, err := hashWith(want, pass)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// pbkdf2 parameters. 600,000 iterations of SHA-256 is OWASP's 2023 floor and
// costs a few hundred milliseconds here — paid once per request, on a board one
// person opens, which is the right side of that trade.
const (
	hashScheme = "pbkdf2-sha256"
	hashIters  = 600000
	saltLen    = 16
)

// Hash turns a password into the string that goes in the config.
//
//	pbkdf2-sha256$<iterations>$<salt b64>$<derived key b64>
//
// Self-describing on purpose: the iteration count can go up later without
// invalidating every existing line, because each line carries its own.
func Hash(pass string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, pass, salt, hashIters, sha256.Size)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashScheme, hashIters,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk)), nil
}

// hashWith re-derives a password using the parameters recorded in an existing
// hash, so the result is comparable with it.
func hashWith(encoded, pass string) (string, error) {
	scheme, iters, salt, err := parseHash(encoded)
	if err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, pass, salt, iters, sha256.Size)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", scheme, iters,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk)), nil
}

func parseHash(encoded string) (scheme string, iters int, salt []byte, err error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 4 {
		return "", 0, nil, fmt.Errorf("want %s$<iterations>$<salt>$<key>", hashScheme)
	}
	if parts[0] != hashScheme {
		return "", 0, nil, fmt.Errorf("unknown password scheme %q; this build writes %s", parts[0], hashScheme)
	}
	if iters, err = strconv.Atoi(parts[1]); err != nil || iters < 1 {
		return "", 0, nil, fmt.Errorf("iteration count %q is not a positive integer", parts[1])
	}
	if salt, err = base64.RawStdEncoding.DecodeString(parts[2]); err != nil {
		return "", 0, nil, fmt.Errorf("salt is not base64: %v", err)
	}
	if _, err = base64.RawStdEncoding.DecodeString(parts[3]); err != nil {
		return "", 0, nil, fmt.Errorf("key is not base64: %v", err)
	}
	return parts[0], iters, salt, nil
}

// validate is called from Config.validate. Every user is checked at load so a
// typo in a hash is a startup error rather than a locked-out board at 2am.
func (a Auth) validate() []string {
	var problems []string
	for user, encoded := range a.Users {
		if user == "" {
			problems = append(problems, "auth: a user with no name")
			continue
		}
		if _, _, _, err := parseHash(encoded); err != nil {
			problems = append(problems, fmt.Sprintf(
				"auth: user %q: %v. Run `conveyor passwd %s` and paste what it prints — "+
					"a plaintext password here would be a password in every backup of this file", user, err, user))
		}
	}
	return problems
}
