package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/AmirRaptoR/Conveyor/internal/push"
)

// --- push notifications ---------------------------------------------------

// notify sends one notification to every subscribed device, in the
// background: a push service that is slow must not hold the scheduler. A
// subscription the service says is gone is forgotten on the spot.
func (s *Server) notify(title, body, itemID string) {
	if s.pushKeys == nil || s.pushSubs.Len() == 0 {
		return
	}
	if len(body) > 160 {
		body = body[:157] + "…"
	}
	payload, _ := json.Marshal(map[string]string{
		"title": title, "body": body, "tag": itemID,
		"url": "/#item=" + url.PathEscape(itemID),
	})
	for _, sub := range s.pushSubs.All() {
		sub := sub
		s.spawn(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err := s.pushKeys.Send(ctx, sub, payload, "https://github.com/AmirRaptoR/Conveyor")
			switch {
			case errors.Is(err, push.Gone):
				_ = s.pushSubs.Remove(sub.Endpoint)
			case err != nil:
				fmt.Fprintf(os.Stderr, "conveyor: push to %s: %v\n", sub.Endpoint, err)
			}
		})
	}
}

func (s *Server) handlePushKey(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if s.pushKeys == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"key": "", "subscribed": 0})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"key": s.pushKeys.Public, "subscribed": s.pushSubs.Len()})
}

func (s *Server) handlePushSubscribe(w http.ResponseWriter, r *http.Request) {
	var sub push.Subscription
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&sub); err != nil ||
		!strings.HasPrefix(sub.Endpoint, "https://") || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		http.Error(w, "not a push subscription", http.StatusBadRequest)
		return
	}
	// Every real web-push endpoint is a public service (Google, Mozilla,
	// Apple); an endpoint whose host is already a literal loopback, private,
	// link-local or unique-local address can only be aimed at this machine or
	// its own network by whoever is calling this route, never a real push
	// service. Send's own dial-time check (internal/push) still guards a
	// hostname that resolves somewhere non-public later.
	if u, err := url.Parse(sub.Endpoint); err == nil {
		if ip := net.ParseIP(strings.Trim(u.Hostname(), "[]")); ip != nil && push.NonPublicIP(ip) {
			http.Error(w, "push endpoint host is not a public address", http.StatusBadRequest)
			return
		}
	}
	fresh, err := s.pushSubs.Add(sub)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// A new device hears back at once, so turning notifications on is its
	// own proof; the page re-posts on every load and those stay silent.
	if fresh && s.pushKeys != nil {
		s.spawn(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			payload, _ := json.Marshal(map[string]string{"title": "Conveyor",
				"body": "Notifications are on. You will hear when something needs you or ships.", "url": "/"})
			if err := s.pushKeys.Send(ctx, sub, payload, "https://github.com/AmirRaptoR/Conveyor"); err != nil {
				fmt.Fprintf(os.Stderr, "conveyor: hello push: %v\n", err)
			}
		})
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Endpoint string `json:"endpoint"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 16<<10)).Decode(&v)
	if v.Endpoint == "" {
		http.Error(w, "no endpoint", http.StatusBadRequest)
		return
	}
	_ = s.pushSubs.Remove(v.Endpoint)
	w.WriteHeader(http.StatusNoContent)
}

// A test push, so a person can see the whole path work before waiting for a
// real decision to come up.
func (s *Server) handlePushTest(w http.ResponseWriter, r *http.Request) {
	if s.pushKeys == nil {
		http.Error(w, "push notifications are off (no key)", http.StatusServiceUnavailable)
		return
	}
	if s.pushSubs.Len() == 0 {
		http.Error(w, "no device is subscribed", http.StatusConflict)
		return
	}
	s.notify("Conveyor", "Notifications are on. You will hear when something needs you or ships.", "")
	w.WriteHeader(http.StatusNoContent)
}
