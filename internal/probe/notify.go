package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/AmirRaptoR/Conveyor/internal/push"
)

// contact is the VAPID contact URL, the same one Server.notify passes
// (internal/server/push.go) — a push service may use it to reach the
// operator if it has a complaint about the traffic.
const contact = "https://github.com/AmirRaptoR/Conveyor"

// NotifyPayload builds the push body for a failed probe, in the exact shape
// the service worker renders (internal/server/web/sw.js:13-24): a JSON
// object with "title" and "body", so a failure reported by a detached run
// with nobody watching still reaches a phone as a real notification rather
// than an empty "Conveyor".
func NotifyPayload(failing []Verdict) []byte {
	lines := make([]string, len(failing))
	for i, v := range failing {
		lines[i] = v.String()
	}
	body := "conveyor probe found the board unreachable:\n" + strings.Join(lines, "\n")
	if len(body) > 160 {
		body = body[:157] + "…"
	}
	payload, _ := json.Marshal(map[string]string{
		"title": "Deploy check failed",
		"body":  body,
		"tag":   "probe",
	})
	return payload
}

// Notify sends one push naming the failing origins to every subscription
// already recorded in dataDir, and returns one line describing what
// happened, for the caller to print.
//
// It reads <dataDir>/vapid.json only if that file already exists: push.LoadKeys
// mints a key pair and writes it on a missing path, and probe must never
// write inside the data directory — it is not the process that owns it. A
// missing key pair, no subscriptions, or a push that itself errors are all
// reported here as "push was unavailable", never as an error of the probe's
// own: the exit status is decided by the probe result alone.
func Notify(ctx context.Context, dataDir string, failing []Verdict) string {
	vapidPath := filepath.Join(dataDir, "vapid.json")
	if _, err := os.Stat(vapidPath); err != nil {
		return "push: no vapid.json in the data directory; not notifying"
	}
	keys, err := push.LoadKeys(vapidPath)
	if err != nil {
		return fmt.Sprintf("push: %v; not notifying", err)
	}
	// OpenStore only reads; a missing or empty push.json leaves it with no
	// subscriptions rather than writing anything.
	subs := push.OpenStore(filepath.Join(dataDir, "push.json")).All()
	if len(subs) == 0 {
		return "push: no subscriptions in the data directory; not notifying"
	}

	payload := NotifyPayload(failing)
	sent, failed := 0, 0
	for _, sub := range subs {
		// Unlike Server.notify, a subscription the push service says is gone
		// (push.Gone) is not removed here: probe never writes inside the
		// data directory, not even to prune.
		if err := keys.Send(ctx, sub, payload, contact); err != nil {
			failed++
			continue
		}
		sent++
	}
	if failed > 0 {
		return fmt.Sprintf("push: sent to %d/%d subscription(s), %d failed", sent, len(subs), failed)
	}
	return fmt.Sprintf("push: sent to %d subscription(s)", sent)
}
