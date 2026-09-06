package server

import (
	"net/http"
	"testing"
)

// A push subscription aimed at this machine or its own network can only be
// the caller doing that on purpose, never a real push service — Google,
// Mozilla and Apple's endpoints are always public.
func TestPushSubscribeRefusesNonPublicEndpoints(t *testing.T) {
	cfg, r, _, _, _, _ := modePipeline(t)
	_, h := newModeServer(t, cfg, r, ModeAuto)

	validKeys := `"keys":{"p256dh":"x","auth":"y"}`
	for _, endpoint := range []string{
		"https://127.0.0.1/push/abc",
		"https://[::1]/push/abc",
		"https://10.0.0.5/push/abc",
		"https://192.168.1.1/push/abc",
		"https://169.254.1.1/push/abc",
		"https://[fc00::1]/push/abc",
	} {
		body := []byte(`{"endpoint":"` + endpoint + `",` + validKeys + `}`)
		code, _ := doReq(t, h, "POST", "/api/push/subscribe", body)
		if code != http.StatusBadRequest {
			t.Errorf("subscribe to %s = %d, want 400", endpoint, code)
		}
	}

	// A real-looking public endpoint is not refused by this check (it may
	// still fail to actually deliver, which is not this route's concern).
	body := []byte(`{"endpoint":"https://fcm.googleapis.com/fcm/send/abc",` + validKeys + `}`)
	if code, _ := doReq(t, h, "POST", "/api/push/subscribe", body); code == http.StatusBadRequest {
		t.Error("a public endpoint was refused")
	}
}
