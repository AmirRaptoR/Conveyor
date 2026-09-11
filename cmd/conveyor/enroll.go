package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/AmirRaptoR/Conveyor/internal/config"
	"github.com/AmirRaptoR/Conveyor/internal/enroll"
	"github.com/AmirRaptoR/Conveyor/internal/preflight"
	"github.com/AmirRaptoR/Conveyor/internal/runner"
	"gopkg.in/yaml.v3"
)

// answerFlags implements flag.Value for a repeatable -answer NAME=VALUE. A
// name given twice, or one with no "=", is an error — Set returns it and the
// standard flag package reports it before enrollFlow ever asks a question.
type answerFlags struct {
	m map[string]string
}

func (a *answerFlags) String() string { return "" }

func (a *answerFlags) Set(s string) error {
	i := strings.IndexByte(s, '=')
	if i < 0 {
		return fmt.Errorf("-answer wants NAME=VALUE, got %q", s)
	}
	name, val := s[:i], s[i+1:]
	if a.m == nil {
		a.m = map[string]string{}
	}
	if _, dup := a.m[name]; dup {
		return fmt.Errorf("-answer %s given twice", name)
	}
	a.m[name] = val
	return nil
}

// cmdEnroll is a guided flow that drafts a sources: block and prints it on
// stdout — nothing else on stdout, ever, so the output is directly
// pasteable. It edits no file: the operator pastes the block into their own
// config, exactly as `conveyor passwd` prints an auth.users line rather than
// writing one. It starts no stage, lists no items and touches no repository;
// the one thing it does beyond asking questions is run the same readiness
// checks `conveyor preflight` runs, against the drafted source alone, and
// print them to stderr so a failing check never suppresses the draft.
func cmdEnroll(args []string) error {
	c := newFlags("enroll")
	answers := &answerFlags{}
	c.fs.Var(answers, "answer", "NAME=VALUE, repeatable; pre-answers a prompt without asking it")
	cfg, r, ctx, stop, err := c.load(args)
	if err != nil {
		return err
	}
	defer stop()

	asker := &enroll.Asker{Answers: answers.m, In: stdin, Out: os.Stderr}
	name, block, err := enrollFlow(cfg, asker)
	if err != nil {
		return err
	}
	if leftover := asker.Leftover(); len(leftover) > 0 {
		return fmt.Errorf("-answer given for unknown prompt(s): %s", strings.Join(leftover, ", "))
	}

	// The draft is what makes this command worth running; print it before
	// anything that could still fail, so a check that cannot even be
	// attempted never costs the operator the YAML they just built.
	fmt.Fprintln(os.Stderr, "\nno file was written — paste the block below under this config's sources:")
	fmt.Fprintln(os.Stdout, block)

	checks, cerr := checkDraft(ctx, cfg, r, *c.cfgPath, *c.providers, name, block)
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "\ncould not check the draft: %v\n", cerr)
		return nil
	}
	fmt.Fprintln(os.Stderr, "\nchecklist:")
	preflight.Render(os.Stderr, checks)
	counts := preflight.Counts(checks)
	fmt.Fprintf(os.Stderr, "  %d pass, %d warn, %d skip, %d fail, %d unknown\n",
		counts[preflight.StatusPass], counts[preflight.StatusWarn], counts[preflight.StatusSkip],
		counts[preflight.StatusFail], counts[preflight.StatusUnknown])
	return nil
}

// providerOption is one directory under the provider root, and whether
// conveyor enroll can offer it: it must resolve both list and move (the same
// resolution a source's own provider: gets) and ship source.template.yaml.
// One that resolves the scripts but ships no template is still named, so the
// operator learns why it is not offered rather than seeing it silently
// vanish from the list.
type providerOption struct {
	Name      string
	Available bool
	Reason    string
}

// listProviders inspects every directory under the provider root. A
// directory that does not even resolve list and move is not a provider
// conveyor enroll has any business discussing — it is skipped rather than
// reported, the same as any other stray directory would be.
func listProviders(cfg *config.Config) ([]providerOption, error) {
	root := cfg.ProvidersDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("provider root %s: %w", root, err)
	}
	var out []providerOption
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, _, err := cfg.ProviderVerbs(name); err != nil {
			continue
		}
		tmplPath := filepath.Join(root, name, "source.template.yaml")
		if _, err := os.Stat(tmplPath); err != nil {
			out = append(out, providerOption{Name: name, Reason: "no source.template.yaml"})
			continue
		}
		out = append(out, providerOption{Name: name, Available: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// listAgentNames lists the directories under the agents root, for the
// prompt that asks which agent: or script: provides one distinct script
// name — offered as a hint, since the answer itself is free-form
// (agent:NAME or script:PATH).
func listAgentNames(cfg *config.Config) []string {
	entries, err := os.ReadDir(cfg.AgentsDir())
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// distinctScriptNames is every stage's script: name, deduplicated in the
// order stages declare them — once per distinct name, not once per stage,
// since two stages may share one.
func distinctScriptNames(cfg *config.Config) []string {
	seen := map[string]bool{}
	var out []string
	for _, st := range cfg.Stages {
		if st.Script == "" || seen[st.Script] {
			continue
		}
		seen[st.Script] = true
		out = append(out, st.Script)
	}
	return out
}

// askScriptChoice asks which agent: or script: provides one script name,
// naming the agents available under the agents root as a hint.
func askScriptChoice(asker *enroll.Asker, name string, agents []string) (enroll.ScriptChoice, error) {
	prompt := fmt.Sprintf("which agent: or script: provides %q (agent:NAME or script:PATH)", name)
	if len(agents) > 0 {
		prompt += fmt.Sprintf("; agents available: %s", strings.Join(agents, ", "))
	}
	answer, err := asker.Ask(name, prompt, "agent:mock", "")
	if err != nil {
		return enroll.ScriptChoice{}, err
	}
	return enroll.ParseScriptChoice(name, answer)
}

// enrollFlow drives the whole guided question-and-answer sequence and
// returns the source's name and the drafted sources: block — one list
// entry's worth of raw YAML text, ready to paste under the operator's own
// sources:. It touches no repository and starts no stage: every side effect
// is a question printed to asker.Out and an answer read from asker.Answers
// or asker.In.
func enrollFlow(cfg *config.Config, asker *enroll.Asker) (name, block string, err error) {
	existing := make([]string, len(cfg.Sources))
	for i, s := range cfg.Sources {
		existing[i] = s.Name
	}

	name, err = asker.Ask("SOURCE", "source name", "midgame", "")
	if err != nil {
		return "", "", err
	}
	if err := enroll.ValidateSourceName(name, existing); err != nil {
		return "", "", err
	}

	workdir, err := asker.Ask("WORKDIR", "workdir (a checkout of the repository this source names)", "~/codes/midgame", "")
	if err != nil {
		return "", "", err
	}

	providers, err := listProviders(cfg)
	if err != nil {
		return "", "", err
	}
	var offered []string
	for _, p := range providers {
		if p.Available {
			offered = append(offered, p.Name)
		} else {
			fmt.Fprintf(asker.Out, "  %s: unavailable (%s)\n", p.Name, p.Reason)
		}
	}
	if len(offered) == 0 {
		return "", "", errors.New("no provider under the provider root ships both list/move and source.template.yaml")
	}
	providerName, err := asker.Ask("PROVIDER", "provider", strings.Join(offered, "/"), "")
	if err != nil {
		return "", "", err
	}
	var chosen *providerOption
	for i := range providers {
		if providers[i].Name == providerName {
			chosen = &providers[i]
			break
		}
	}
	if chosen == nil || !chosen.Available {
		return "", "", fmt.Errorf("provider %q is not available to enroll (available: %s)", providerName, strings.Join(offered, ", "))
	}

	tmplPath := filepath.Join(cfg.ProvidersDir(), providerName, "source.template.yaml")
	tmpl, err := enroll.LoadTemplate(tmplPath)
	if err != nil {
		return "", "", err
	}

	promptAnswers := map[string]string{}
	for _, p := range tmpl.Prompts {
		v, err := asker.Ask(p.Name, p.Prompt, p.Example, p.Default)
		if err != nil {
			return "", "", err
		}
		promptAnswers[p.Name] = v
	}

	agentNames := listAgentNames(cfg)
	var choices []enroll.ScriptChoice
	for _, sn := range distinctScriptNames(cfg) {
		c, err := askScriptChoice(asker, sn, agentNames)
		if err != nil {
			return "", "", err
		}
		choices = append(choices, c)
	}

	wantDoctor, err := asker.AskBool("DOCTOR_ENABLED",
		"add an optional doctor: script too? (used by the board's Diagnose sweep)", false)
	if err != nil {
		return "", "", err
	}
	if wantDoctor {
		c, err := askScriptChoice(asker, "doctor", agentNames)
		if err != nil {
			return "", "", err
		}
		choices = append(choices, c)
	}

	scripts := enroll.BuildScripts(choices)
	block, err = enroll.Fill(tmpl, name, workdir, scripts, promptAnswers)
	if err != nil {
		return "", "", err
	}
	return name, block, nil
}

// checkDraft appends the drafted source to a temporary copy of the config
// file, written in the config's own directory — paths inside a config
// resolve against that directory, so a temp file anywhere else would
// silently break providers: and every relative script: — loads it, and
// returns the same readiness checks conveyor preflight would report, for
// just the drafted source. The temp file is removed before this returns.
func checkDraft(ctx context.Context, cfg *config.Config, r *runner.Runner, cfgPath, providersFlag, name, block string) ([]preflight.Check, error) {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s: not a YAML mapping document", cfgPath)
	}
	root := doc.Content[0]

	var sourcesNode *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "sources" {
			sourcesNode = root.Content[i+1]
			break
		}
	}
	if sourcesNode == nil {
		key := &yaml.Node{Kind: yaml.ScalarNode, Value: "sources"}
		sourcesNode = &yaml.Node{Kind: yaml.SequenceNode}
		root.Content = append(root.Content, key, sourcesNode)
	}
	if sourcesNode.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s: sources: is not a list", cfgPath)
	}

	var draftDoc yaml.Node
	if err := yaml.Unmarshal([]byte(block), &draftDoc); err != nil {
		return nil, fmt.Errorf("drafted block did not parse as YAML: %w", err)
	}
	if len(draftDoc.Content) == 0 || draftDoc.Content[0].Kind != yaml.SequenceNode || len(draftDoc.Content[0].Content) != 1 {
		return nil, errors.New("drafted block is not exactly one sources: entry")
	}
	sourcesNode.Content = append(sourcesNode.Content, draftDoc.Content[0].Content[0])

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, err
	}

	tmp, err := os.CreateTemp(cfg.Dir, ".conveyor-enroll-*.yaml")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	tmpCfg, err := config.LoadFrom(tmpPath, providersFlag)
	if err != nil {
		return nil, fmt.Errorf("drafted config does not load: %w", err)
	}
	for _, s := range tmpCfg.Sources {
		if s.Name == name {
			return checksForSource(ctx, tmpCfg, r, s), nil
		}
	}
	return nil, fmt.Errorf("drafted source %q not found after loading temp config", name)
}
