// Package probe answers one question a deploy's own build-test-install
// pipeline never asks: is the board still serving after the restart it just
// scheduled? A build that is green in CI can still leave the running box
// unreachable — a worktree marker that predates every existing worktree, an
// allowlist with no entry for the live config, a PATH systemd never sources —
// because none of those are things a test suite has state for.
//
// The check has to run after the fact, on its own: the restart a deploy
// schedules is deliberately detached (`systemd-run --on-active=30`) so it
// outlives the stage that triggered it, and by the time it lands the process
// that asked for it is long gone.
package probe

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Verdict is the outcome of probing one origin.
type Verdict struct {
	URL    string
	Pass   bool
	Status int    // the HTTP status received, or 0 on a transport error
	Detail string // empty on a pass; otherwise what went wrong, for a person reading a log
}

func (v Verdict) String() string {
	if v.Pass {
		return fmt.Sprintf("%s: ok (%d)", v.URL, v.Status)
	}
	if v.Status != 0 {
		return fmt.Sprintf("%s: FAIL (%d) %s", v.URL, v.Status, v.Detail)
	}
	return fmt.Sprintf("%s: FAIL %s", v.URL, v.Detail)
}

// Prober probes origins over plain GET requests, retrying a fixed interval
// until a deadline. The client is exported so a caller can point it at
// something other than the network default, and RequestTimeout/Interval are
// plain fields rather than constructor arguments so a test can shrink them.
type Prober struct {
	Client *http.Client
	// RequestTimeout bounds one request. It is the "-request-timeout" flag,
	// distinct from the config's stage timeout: that word already means
	// something else to a reader of conveyor.yaml.
	RequestTimeout time.Duration
	// Interval is how long Wait sleeps between rounds of retrying whatever
	// has not yet passed.
	Interval time.Duration
}

// New is a Prober with reasonable defaults: TLS verification on (the client
// never disables it), no cookies, no redirect chasing.
func New() *Prober {
	return &Prober{
		Client: &http.Client{
			// A 3xx must be classified and reported as a failure naming the
			// Location, not chased somewhere the probe never asked about.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		RequestTimeout: 10 * time.Second,
		Interval:       5 * time.Second,
	}
}

// probeOnce requests <url>/ with no credentials and no Origin header, and
// classifies the response.
func (p *Prober) ProbeOnce(ctx context.Context, url string) Verdict {
	ctx, cancel := context.WithTimeout(ctx, p.RequestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/", nil)
	if err != nil {
		return Verdict{URL: url, Detail: err.Error()}
	}
	resp, err := p.Client.Do(req)
	if err != nil {
		return Verdict{URL: url, Detail: err.Error()}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	pass, detail := classify(resp)
	return Verdict{URL: url, Pass: pass, Status: resp.StatusCode, Detail: detail}
}

// classify turns one response into pass/fail, per the table this package
// exists to enforce:
//
//	200        -> pass
//	401        -> pass (the probe carries no credentials; a challenge means
//	              auth itself is answering, which is the healthy shape)
//	403        -> fail, the host was rejected (Server.hostCheck's answer to an
//	              origin the config does not recognise — see #54)
//	other 4xx,
//	5xx        -> fail, naming the status
//	3xx        -> fail, naming the Location it was pointed at
func classify(resp *http.Response) (pass bool, detail string) {
	switch {
	case resp.StatusCode == http.StatusOK, resp.StatusCode == http.StatusUnauthorized:
		return true, ""
	case resp.StatusCode == http.StatusForbidden:
		return false, "host rejected (403) — add it to auth.origins"
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		return false, fmt.Sprintf("redirected (%d) to %s", resp.StatusCode, resp.Header.Get("Location"))
	default:
		return false, fmt.Sprintf("status %d", resp.StatusCode)
	}
}

// Wait probes every url, retrying whichever have not yet passed at a fixed
// interval, until every one passes or deadline elapses — whichever comes
// first. It returns exactly one verdict per url, in the order given: the
// passing one if it eventually passed, the last failure otherwise.
//
// It returns the moment every entry passes rather than waiting out the rest
// of the deadline, because the caller (a detached, scheduled run with nobody
// watching) wants an answer as soon as there is one.
func (p *Prober) Wait(ctx context.Context, urls []string, deadline time.Duration) []Verdict {
	giveUpAt := time.Now().Add(deadline)
	last := make(map[string]Verdict, len(urls))
	for {
		allPass := true
		for _, u := range urls {
			if v, ok := last[u]; ok && v.Pass {
				continue
			}
			v := p.ProbeOnce(ctx, u)
			last[u] = v
			if !v.Pass {
				allPass = false
			}
		}
		if allPass {
			break
		}
		// Sleep no longer than what is left of the deadline: a plain
		// time.After(p.Interval) would overshoot -wait by up to a whole
		// interval on every round, which for a short -wait against a long
		// default interval means "give up" arrives far later than asked.
		remaining := time.Until(giveUpAt)
		if remaining <= 0 || ctx.Err() != nil {
			break
		}
		sleep := p.Interval
		if remaining < sleep {
			sleep = remaining
		}
		select {
		case <-ctx.Done():
		case <-time.After(sleep):
		}
	}
	out := make([]Verdict, len(urls))
	for i, u := range urls {
		out[i] = last[u]
	}
	return out
}
