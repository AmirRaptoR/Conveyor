package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/enroll"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
)

// enrollTestCfg lays down a provider ("fake") that resolves list, move and
// source.template.yaml — offered to enroll — an agent ("fakeagent") with a
// status script, and a two-stage pipeline needing one distinct script name
// ("work"). Mirrors healthyConfig in preflight_test.go, with the template
// enroll additionally needs.
func enrollTestCfg(t *testing.T) (cfg *config.Config, cfgPath string) {
	t.Helper()
	dir := t.TempDir()
	writeExec(t, filepath.Join(dir, "providers", "fake", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "providers", "fake", "move.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "providers", "fake", "preflight.sh"),
		"#!/bin/sh\ncat > \"$CONVEYOR_RESULT\" <<'EOF'\n{\"checks\":[{\"name\":\"ok\",\"status\":\"pass\"}]}\nEOF\n")
	writeFile(t, filepath.Join(dir, "providers", "fake", "source.template.yaml"), `
prompts:
  - name: GREETING
    prompt: "a greeting"
    example: "hello"
    default: "hello"
template: |
  - name: {{SOURCE}}
    provider: fake
    workdir: {{WORKDIR}}
    env:
      GREETING: {{GREETING}}
    scripts:
      {{SCRIPTS}}
`)
	writeExec(t, filepath.Join(dir, "agents", "fakeagent", "work"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "agents", "fakeagent", "status"),
		"#!/bin/sh\ncat > \"$CONVEYOR_RESULT\" <<'EOF'\n{\"state\":\"ok\"}\nEOF\n")
	if err := os.MkdirAll(filepath.Join(dir, "repo1"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath = filepath.Join(dir, "conveyor.yaml")
	yaml := `version: 1
stages:
  - name: backlog
  - name: working
    script: work
    onSuccess: done
  - name: done
    terminal: true
sources:
  - name: s1
    provider: fake
    workdir: ./repo1
    scripts:
      work:
        agent: fakeagent
`
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, cfgPath
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fullAnswers() map[string]string {
	return map[string]string{
		"SOURCE": "s2", "WORKDIR": "./repo1", "PROVIDER": "fake",
		"GREETING": "hi there", "work": "agent:fakeagent",
		"DOCTOR_ENABLED": "n",
	}
}

func TestEnrollFlowHappyPath(t *testing.T) {
	cfg, _ := enrollTestCfg(t)
	var out bytes.Buffer
	asker := &enroll.Asker{Answers: fullAnswers(), Out: &out}
	name, block, err := enrollFlow(cfg, asker)
	if err != nil {
		t.Fatalf("enrollFlow: %v (stderr so far: %s)", err, out.String())
	}
	if name != "s2" {
		t.Errorf("name = %q, want s2", name)
	}
	if !strings.Contains(block, `name: "s2"`) {
		t.Errorf("block missing source name: %s", block)
	}
	if !strings.Contains(block, `GREETING: "hi there"`) {
		t.Errorf("block missing the greeting answer: %s", block)
	}
	if !strings.Contains(block, "work:\n") || !strings.Contains(block, "agent: fakeagent") {
		t.Errorf("block missing the script choice: %s", block)
	}
	if strings.Contains(block, "doctor") {
		t.Errorf("doctor was declined; block should not mention it: %s", block)
	}
	if leftover := asker.Leftover(); len(leftover) != 0 {
		t.Errorf("leftover answers not consumed: %v", leftover)
	}
}

func TestEnrollFlowWithDoctor(t *testing.T) {
	cfg, _ := enrollTestCfg(t)
	answers := fullAnswers()
	answers["DOCTOR_ENABLED"] = "y"
	answers["doctor"] = "agent:fakeagent"
	asker := &enroll.Asker{Answers: answers, Out: &bytes.Buffer{}}
	_, block, err := enrollFlow(cfg, asker)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "doctor:") {
		t.Errorf("block missing the opted-in doctor script: %s", block)
	}
}

// The source name must be plausible and unique before any other prompt is
// answered — checked here by never supplying the later answers at all: if
// enrollFlow asked for them, it would fail on a missing answer instead of
// on the duplicate name, and the test would fail for the wrong reason.
func TestEnrollFlowDuplicateSourceNameFailsBeforeOtherPrompts(t *testing.T) {
	cfg, _ := enrollTestCfg(t)
	asker := &enroll.Asker{Answers: map[string]string{"SOURCE": "s1"}, Out: &bytes.Buffer{}}
	_, _, err := enrollFlow(cfg, asker)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err = %v, want it to say the name already exists", err)
	}
}

func TestEnrollFlowImplausibleSourceName(t *testing.T) {
	cfg, _ := enrollTestCfg(t)
	asker := &enroll.Asker{Answers: map[string]string{"SOURCE": "has spaces"}, Out: &bytes.Buffer{}}
	_, _, err := enrollFlow(cfg, asker)
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestEnrollFlowUnknownProviderIsAnError(t *testing.T) {
	cfg, _ := enrollTestCfg(t)
	answers := fullAnswers()
	answers["PROVIDER"] = "nonexistent"
	asker := &enroll.Asker{Answers: answers, Out: &bytes.Buffer{}}
	_, _, err := enrollFlow(cfg, asker)
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("err = %v, want it to say the provider is not available", err)
	}
}

// A provider directory that resolves list/move but ships no
// source.template.yaml is named as unavailable, not silently dropped.
func TestEnrollFlowProviderWithoutTemplateIsUnavailable(t *testing.T) {
	cfg, cfgPath := enrollTestCfg(t)
	dir := filepath.Dir(cfgPath)
	writeExec(t, filepath.Join(dir, "providers", "bare", "list.sh"), "#!/bin/sh\nexit 0\n")
	writeExec(t, filepath.Join(dir, "providers", "bare", "move.sh"), "#!/bin/sh\nexit 0\n")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	answers := fullAnswers()
	answers["PROVIDER"] = "bare"
	asker := &enroll.Asker{Answers: answers, Out: &stderr}
	_, _, err = enrollFlow(cfg, asker)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(stderr.String(), "bare") || !strings.Contains(stderr.String(), "no source.template.yaml") {
		t.Errorf("stderr should name %q as unavailable with the reason: %s", "bare", stderr.String())
	}
}

// -answer given for an unknown prompt is an error, surfaced by cmdEnroll
// (which checks Leftover after the flow completes) — this test exercises
// enrollFlow's own consumption plus the Leftover check cmdEnroll performs.
func TestEnrollUnknownAnswerIsLeftover(t *testing.T) {
	cfg, _ := enrollTestCfg(t)
	answers := fullAnswers()
	answers["NOT_A_REAL_PROMPT"] = "x"
	asker := &enroll.Asker{Answers: answers, Out: &bytes.Buffer{}}
	if _, _, err := enrollFlow(cfg, asker); err != nil {
		t.Fatal(err)
	}
	leftover := asker.Leftover()
	if len(leftover) != 1 || leftover[0] != "NOT_A_REAL_PROMPT" {
		t.Errorf("Leftover() = %v, want [NOT_A_REAL_PROMPT]", leftover)
	}
}

// checkDraft runs conveyor preflight's own checks against just the drafted
// source, by splicing it into a temporary copy of the real config file.
func TestCheckDraftRunsPreflightAgainstDraftedSourceOnly(t *testing.T) {
	cfg, cfgPath := enrollTestCfg(t)
	r := runner.New(filepath.Join(t.TempDir(), "runs"))
	asker := &enroll.Asker{Answers: fullAnswers(), Out: &bytes.Buffer{}}
	name, block, err := enrollFlow(cfg, asker)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := checkDraft(context.Background(), cfg, r, cfgPath, "", name, block)
	if err != nil {
		t.Fatalf("checkDraft: %v", err)
	}
	found := false
	for _, c := range checks {
		if c.Name == "ok" && c.Status == "pass" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the fake provider's own preflight check to appear: %+v", checks)
	}

	// checkDraft must not have modified or left behind a temp file in the
	// config's own directory.
	entries, _ := os.ReadDir(filepath.Dir(cfgPath))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".conveyor-enroll-") {
			t.Errorf("temp config %s was not removed", e.Name())
		}
	}
	raw, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(raw), name) {
		t.Errorf("the real config file was modified: %s", raw)
	}
}

// cmdEnroll's stdout carries the drafted sources: block and nothing else —
// every prompt, note and checklist goes to stderr. Driven fully by -answer
// so it completes with stdin closed.
func TestCmdEnrollStdoutIsOnlyTheDraft(t *testing.T) {
	_, cfgPath := enrollTestCfg(t)
	args := []string{"-c", cfgPath, "-providers", filepath.Join(filepath.Dir(cfgPath), "providers")}
	for k, v := range fullAnswers() {
		args = append(args, "-answer", k+"="+v)
	}

	oldStdout, oldStderr, oldStdin := os.Stdout, os.Stderr, stdin
	stdin = bufio.NewReader(strings.NewReader("")) // never read: every prompt is -answered
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	os.Stdout, os.Stderr = wo, we
	err := cmdEnroll(args)
	wo.Close()
	we.Close()
	os.Stdout, os.Stderr, stdin = oldStdout, oldStderr, oldStdin

	var stdout, stderr bytes.Buffer
	stdout.ReadFrom(ro)
	stderr.ReadFrom(re)

	if err != nil {
		t.Fatalf("cmdEnroll: %v; stderr: %s", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), `name: "s2"`) {
		t.Errorf("stdout missing the drafted block: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "checklist") || strings.Contains(stdout.String(), "no file was written") {
		t.Errorf("stdout carries more than the draft: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "checklist") {
		t.Errorf("stderr missing the checklist: %q", stderr.String())
	}
}

// EOF before the last answer prints nothing on stdout and exits non-zero —
// a half-drafted source is worse than none.
func TestCmdEnrollEOFPrintsNothingOnStdout(t *testing.T) {
	_, cfgPath := enrollTestCfg(t)
	args := []string{"-c", cfgPath, "-providers", filepath.Join(filepath.Dir(cfgPath), "providers"),
		"-answer", "SOURCE=s2", "-answer", "WORKDIR=./repo1", "-answer", "PROVIDER=fake"}

	oldStdout, oldStderr, oldStdin := os.Stdout, os.Stderr, stdin
	stdin = bufio.NewReader(strings.NewReader("")) // closed: GREETING is never answered
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	os.Stdout, os.Stderr = wo, we
	err := cmdEnroll(args)
	wo.Close()
	we.Close()
	os.Stdout, os.Stderr, stdin = oldStdout, oldStderr, oldStdin

	var stdout bytes.Buffer
	stdout.ReadFrom(ro)
	var stderr bytes.Buffer
	stderr.ReadFrom(re)

	if err == nil {
		t.Fatal("expected an error")
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty, got %q", stdout.String())
	}
}

// -answer given twice for the same name is an error from flag parsing,
// before enrollFlow ever runs.
func TestCmdEnrollDuplicateAnswerFlag(t *testing.T) {
	_, cfgPath := enrollTestCfg(t)
	a := &answerFlags{}
	if err := a.Set("SOURCE=one"); err != nil {
		t.Fatal(err)
	}
	if err := a.Set("SOURCE=two"); err == nil {
		t.Fatal("expected an error for a duplicate -answer name")
	}
	_ = cfgPath
}

// The acceptance criterion for the real thing: driving enroll with -answer
// against the shipped providers/mock/source.template.yaml, splicing its
// stdout under conveyor.example.yaml's own sources:, must produce a config
// that loads and comes back clean from Validate() — the same proof a person
// pasting the block in by hand would get.
func TestEnrollAgainstMockTemplateProducesAValidExampleConfig(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	examplePath := filepath.Join(root, "conveyor.example.yaml")
	providersDir := filepath.Join(root, "providers")
	if _, err := os.Stat(filepath.Join(providersDir, "mock", "source.template.yaml")); err != nil {
		t.Skipf("providers/mock/source.template.yaml not found: %v", err)
	}

	args := []string{
		"-c", examplePath, "-providers", providersDir,
		"-answer", "SOURCE=enrolled-example",
		"-answer", "WORKDIR=.",
		"-answer", "PROVIDER=mock",
		"-answer", "GREETING=hello from the enrolled example",
		"-answer", "refine=agent:mock",
		"-answer", "implement=agent:mock",
		"-answer", "cleanup=agent:git",
		"-answer", "DOCTOR_ENABLED=n",
	}

	oldStdout, oldStderr, oldStdin := os.Stdout, os.Stderr, stdin
	stdin = bufio.NewReader(strings.NewReader(""))
	ro, wo, _ := os.Pipe()
	re, we, _ := os.Pipe()
	os.Stdout, os.Stderr = wo, we
	err = cmdEnroll(args)
	wo.Close()
	we.Close()
	os.Stdout, os.Stderr, stdin = oldStdout, oldStderr, oldStdin

	var stdout, stderr bytes.Buffer
	stdout.ReadFrom(ro)
	stderr.ReadFrom(re)
	if err != nil {
		t.Fatalf("cmdEnroll: %v; stderr: %s", err, stderr.String())
	}

	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	idx := -1
	for i, l := range lines {
		if l == "sources:" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("conveyor.example.yaml has no top-level sources: key")
	}

	// The drafted block is a single "- name: ..." sequence item at column 0;
	// every entry already under sources: sits two spaces in, so splicing it
	// in means indenting it the same way a person pasting it by hand would.
	draft := strings.TrimRight(stdout.String(), "\n")
	var indented []string
	for _, l := range strings.Split(draft, "\n") {
		if l == "" {
			indented = append(indented, l)
			continue
		}
		indented = append(indented, "  "+l)
	}
	var spliced []string
	spliced = append(spliced, lines[:idx+1]...)
	spliced = append(spliced, indented...)
	spliced = append(spliced, lines[idx+1:]...)

	tmp := filepath.Join(t.TempDir(), "conveyor.example.yaml")
	if err := os.WriteFile(tmp, []byte(strings.Join(spliced, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := config.LoadFrom(tmp, providersDir)
	if err != nil {
		t.Fatalf("spliced config did not load: %v", err)
	}
	if errs := cfg.Validate(); len(errs) != 0 {
		t.Fatalf("spliced config failed Validate(): %v", errs)
	}
	found := false
	for _, s := range cfg.Sources {
		if s.Name == "enrolled-example" {
			found = true
		}
	}
	if !found {
		t.Error("the enrolled source did not end up in the spliced config")
	}
}
