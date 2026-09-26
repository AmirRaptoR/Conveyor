package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	gatecheck "github.com/AmirRaptoR/Conveyor/internal/gate"
	"github.com/AmirRaptoR/Conveyor/internal/server"
)

func unixHTTPClient(socket string) *http.Client {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	return &http.Client{Transport: transport}
}

func fetchServerState(ctx context.Context, client *http.Client) (server.State, bool, error) {
	request := func(path string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+path, nil)
		if err != nil {
			return nil, err
		}
		return client.Do(req)
	}
	res, err := request("/api/state")
	if err != nil {
		return server.State{}, false, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return server.State{}, false, fmt.Errorf("GET /api/state: %s", res.Status)
	}
	var state server.State
	if err := json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&state); err != nil {
		return server.State{}, false, err
	}
	ui, err := request("/")
	if err != nil {
		return state, false, err
	}
	defer ui.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(ui.Body, 1<<20))
	uiOK := ui.StatusCode == http.StatusOK && strings.Contains(strings.ToLower(string(body)), "<!doctype html")
	return state, uiOK, nil
}

func cmdGate(args []string) error {
	c := newFlags("gate")
	timeout := c.fs.Duration("timeout", time.Minute, "maximum wait for a fresh healthy server snapshot")
	socket := c.fs.String("socket", "", "server Unix socket (default: <data>/api.sock)")
	expected := c.fs.String("expected-revision", "", "immutable revision that must be serving")
	cfg, _, parent, stop, err := c.load(args)
	if err != nil {
		return err
	}
	defer stop()
	if *socket == "" {
		*socket = cfg.DataDir() + "/api.sock"
	}
	if *expected == "" {
		*expected = c.release.Revision
	}
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	client := unixHTTPClient(*socket)
	var last error
	var result gatecheck.Result
	for {
		state, uiOK, fetchErr := fetchServerState(ctx, client)
		if fetchErr == nil {
			result = gatecheck.Evaluate(cfg, state, *expected, uiOK, time.Now())
			if result.Pass {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}
			last = errors.New("post-deploy checks did not pass")
		} else {
			last = fetchErr
		}
		select {
		case <-ctx.Done():
			if len(result.Checks) > 0 {
				b, _ := json.Marshal(result)
				return fmt.Errorf("gate timed out: %w: %s", last, b)
			}
			return fmt.Errorf("gate timed out: %w", last)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func cmdSoakReport(args []string) error {
	c := newFlags("soak-report")
	socket := c.fs.String("socket", "", "server Unix socket (default: <data>/api.sock)")
	format := c.fs.String("format", "json", "json | markdown")
	cfg, _, ctx, stop, err := c.load(args)
	if err != nil {
		return err
	}
	defer stop()
	if *socket == "" {
		*socket = cfg.DataDir() + "/api.sock"
	}
	state, _, err := fetchServerState(ctx, unixHTTPClient(*socket))
	if err != nil {
		return err
	}
	pass := !state.Watchdog.Incident && state.Metrics.HumanInterventions == 0 && state.Storage.Level != "critical"
	for _, finding := range state.Watchdog.Findings {
		if finding.Kind == "source-stale" || finding.Kind == "revision-coherence" || finding.Kind == "repeated-blocker" {
			pass = false
		}
	}
	report := struct {
		Schema      int                 `json:"schema"`
		Pass        bool                `json:"pass"`
		GeneratedAt time.Time           `json:"generatedAt"`
		Window      string              `json:"window"`
		Metrics     server.Metrics      `json:"metrics"`
		Watchdog    server.WatchdogView `json:"watchdog"`
	}{1, pass, time.Now().UTC(), "7d", state.Metrics, state.Watchdog}
	if *format == "markdown" {
		fmt.Printf("# Conveyor Seven-Day Soak Report\n\n- Result: **%s**\n- Window: `%s` to `%s`\n- Success rate: `%.2f`\n- Retries per completion: `%.2f`\n- Wasted model runs: `%d`\n- Human interventions: `%d`\n- Blocked-to-recovered mean: `%s`\n", map[bool]string{true: "PASS", false: "FAIL"}[pass], report.Metrics.WindowStart.Format(time.RFC3339), report.Metrics.WindowEnd.Format(time.RFC3339), report.Metrics.SuccessRate, report.Metrics.RetriesPerCompletion, report.Metrics.WastedModelRuns, report.Metrics.HumanInterventions, report.Metrics.BlockedToRecovered)
		return nil
	}
	if *format != "json" {
		return errors.New("-format must be json or markdown")
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
