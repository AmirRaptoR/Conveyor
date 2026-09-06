package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every status this repository actually saw or could see, classified against
// a real httptest server — no network, no live board.
func TestProbeOnceClassifiesEachStatus(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		location   string
		wantPass   bool
		wantDetail string
	}{
		{"200 is healthy", http.StatusOK, "", true, ""},
		{"401 is healthy: auth is on", http.StatusUnauthorized, "", true, ""},
		{"403 is the host being rejected", http.StatusForbidden, "", false, "reject"},
		{"other 4xx fails with the status", http.StatusNotFound, "", false, "404"},
		{"5xx fails with the status", http.StatusBadGateway, "", false, "502"},
		{"3xx fails naming the Location", http.StatusFound, "https://elsewhere.example/", false, "https://elsewhere.example/"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.location != "" {
					w.Header().Set("Location", tc.location)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			p := New()
			v := p.ProbeOnce(context.Background(), srv.URL)
			if v.Pass != tc.wantPass {
				t.Fatalf("Pass = %v, want %v (verdict: %+v)", v.Pass, tc.wantPass, v)
			}
			if v.Status != tc.status {
				t.Fatalf("Status = %d, want %d", v.Status, tc.status)
			}
			if tc.wantDetail != "" && !strings.Contains(v.Detail, tc.wantDetail) {
				t.Fatalf("Detail = %q, want it to contain %q", v.Detail, tc.wantDetail)
			}
		})
	}
}

// A DNS/transport/TLS failure — here, a closed connection — fails and names
// the error rather than a status, since there is no response to read one
// from.
func TestProbeOnceReportsTransportError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening there any more

	p := New()
	v := p.ProbeOnce(context.Background(), url)
	if v.Pass {
		t.Fatalf("Pass = true against a closed connection, want false")
	}
	if v.Status != 0 {
		t.Fatalf("Status = %d, want 0 (no response was ever received)", v.Status)
	}
	if v.Detail == "" {
		t.Fatal("Detail is empty; want it to name the error")
	}
}

// A request is a plain GET with no credentials and no Origin header — the
// probe cannot authenticate and must not try, or a stray cookie/header could
// make a 403 look like a 401.
func TestProbeOnceSendsNoCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if _, ok := r.Header["Authorization"]; ok {
			t.Error("request carried an Authorization header")
		}
		if _, ok := r.Header["Origin"]; ok {
			t.Error("request carried an Origin header")
		}
		if r.URL.Path != "/" {
			t.Errorf("path = %s, want /", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New()
	v := p.ProbeOnce(context.Background(), srv.URL)
	if !v.Pass {
		t.Fatalf("verdict: %+v", v)
	}
}

// Wait retries a failing entry at a fixed interval and returns the moment
// every entry passes — it must not wait out the rest of the deadline once
// there is nothing left to retry.
func TestWaitRetriesUntilEveryEntryPasses(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	p := New()
	p.Interval = 10 * time.Millisecond
	start := time.Now()
	verdicts := p.Wait(context.Background(), []string{srv.URL}, 5*time.Second)
	elapsed := time.Since(start)

	if len(verdicts) != 1 || !verdicts[0].Pass {
		t.Fatalf("verdicts = %+v, want one passing entry", verdicts)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3 (502, 502, then 401)", calls)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("Wait took %s; it should return as soon as the entry passed, not wait out the deadline", elapsed)
	}
}

// A stub that never recovers reports the last verdict once the deadline
// elapses, rather than hanging or panicking.
func TestWaitGivesUpAtDeadlineReportingLastVerdict(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	p := New()
	p.Interval = 10 * time.Millisecond
	verdicts := p.Wait(context.Background(), []string{srv.URL}, 50*time.Millisecond)

	if len(verdicts) != 1 {
		t.Fatalf("verdicts = %+v, want one entry", verdicts)
	}
	if verdicts[0].Pass {
		t.Fatal("Pass = true; the stub never answered anything but 502")
	}
	if verdicts[0].Status != http.StatusBadGateway {
		t.Fatalf("Status = %d, want %d (the last verdict)", verdicts[0].Status, http.StatusBadGateway)
	}
}

// Wait does not follow a redirect to somewhere the probe never asked about;
// http.Client would otherwise chase it and report the final destination's
// status instead of the redirect the origin itself answered with.
func TestProbeOnceDoesNotFollowRedirects(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the redirect target must never be reached")
	}))
	defer final.Close()
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusFound)
	}))
	defer redirecting.Close()

	p := New()
	v := p.ProbeOnce(context.Background(), redirecting.URL)
	if v.Pass {
		t.Fatal("Pass = true; a 3xx must fail")
	}
}
