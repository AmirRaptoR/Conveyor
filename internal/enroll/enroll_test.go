package enroll

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadTemplateHappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.template.yaml")
	writeFile(t, path, `
prompts:
  - name: GREETING
    prompt: "a greeting"
    example: "hi"
    default: "hi"
template: |
  - name: {{SOURCE}}
    provider: mock
    workdir: {{WORKDIR}}
    env:
      GREETING: {{GREETING}}
    scripts:
      {{SCRIPTS}}
`)
	tmpl, err := LoadTemplate(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(tmpl.Prompts) != 1 || tmpl.Prompts[0].Name != "GREETING" {
		t.Errorf("prompts = %+v", tmpl.Prompts)
	}
}

func TestLoadTemplateErrors(t *testing.T) {
	base := `
prompts:
  - name: %s
    prompt: "x"
template: %s
`
	cases := []struct {
		name     string
		content  string
		wantErrs []string
	}{
		{
			"missing template",
			"prompts: []\n",
			[]string{"missing template"},
		},
		{
			"unresolved placeholder never named by a prompt",
			"template: |\n  - name: {{SOURCE}}\n    foo: {{UNKNOWN}}\n",
			nil, // covered by Fill, not LoadTemplate; this case is here as documentation
		},
		{
			"prompt name never appears in template",
			fmt.Sprintf(base, "UNUSED", "|\n  - name: {{SOURCE}}\n"),
			[]string{"never appears"},
		},
		{
			"reserved name",
			fmt.Sprintf(base, "SOURCE", "|\n  - name: {{SOURCE}}\n"),
			[]string{"reserved"},
		},
		{
			"bad name shape",
			fmt.Sprintf(base, "lower", "|\n  - name: {{lower}}\n"),
			[]string{"must match"},
		},
		{
			"secret-shaped name",
			fmt.Sprintf(base, "API_TOKEN", "|\n  - name: {{API_TOKEN}}\n"),
			[]string{"secret"},
		},
	}
	for _, tc := range cases {
		if tc.wantErrs == nil {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "source.template.yaml")
			writeFile(t, path, tc.content)
			_, err := LoadTemplate(path)
			if err == nil {
				t.Fatal("expected an error")
			}
			for _, want := range tc.wantErrs {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q should name the file", err)
			}
		})
	}
}

func TestLoadTemplateDuplicatePromptName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.template.yaml")
	writeFile(t, path, `
prompts:
  - name: FOO
    prompt: "a"
  - name: FOO
    prompt: "b"
template: |
  - name: {{SOURCE}}
    a: {{FOO}}
`)
	_, err := LoadTemplate(path)
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("err = %v, want it to say declared twice", err)
	}
}

func TestLoadTemplateMissingFile(t *testing.T) {
	_, err := LoadTemplate("/nonexistent/source.template.yaml")
	if err == nil {
		t.Fatal("expected an error")
	}
}

func TestFillSubstitutesSourceWorkdirAndAnswers(t *testing.T) {
	tmpl := &Template{
		Prompts: []Prompt{{Name: "GREETING", Prompt: "g"}},
		Template: "- name: {{SOURCE}}\n" +
			"  workdir: {{WORKDIR}}\n" +
			"  env:\n" +
			"    GREETING: {{GREETING}}\n",
	}
	out, err := Fill(tmpl, "myrepo", "/path/to/repo", "", map[string]string{"GREETING": "hi there"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: \"myrepo\"") {
		t.Errorf("missing substituted source name: %s", out)
	}
	if !strings.Contains(out, "workdir: \"/path/to/repo\"") {
		t.Errorf("missing substituted workdir: %s", out)
	}
	if !strings.Contains(out, "GREETING: \"hi there\"") {
		t.Errorf("missing substituted answer: %s", out)
	}
}

// {{SCRIPTS}} on its own line is indented to whatever column the placeholder
// itself sits at — proven here two levels deep.
func TestFillIndentsScriptsToPlaceholderColumn(t *testing.T) {
	tmpl := &Template{
		Template: "- name: {{SOURCE}}\n" +
			"  scripts:\n" +
			"    {{SCRIPTS}}\n",
	}
	scripts := "implement:\n  agent: mock"
	out, err := Fill(tmpl, "s", "w", scripts, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "  scripts:\n    implement:\n      agent: mock\n"
	if !strings.Contains(out, want) {
		t.Errorf("out = %q, want it to contain %q", out, want)
	}
}

// Substitution is single-pass: a value containing literal {{X}} text must
// not be substituted a second time.
func TestFillIsSinglePassNotRecursive(t *testing.T) {
	tmpl := &Template{
		Prompts:  []Prompt{{Name: "FOO", Prompt: "f"}},
		Template: "- name: {{SOURCE}}\n  a: {{FOO}}\n",
	}
	out, err := Fill(tmpl, "s", "w", "", map[string]string{"FOO": "{{SOURCE}}"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `a: "{{SOURCE}}"`) {
		t.Errorf("out = %q, want the literal placeholder text preserved", out)
	}
}

func TestFillUnresolvedPlaceholderIsAnError(t *testing.T) {
	tmpl := &Template{Template: "- name: {{SOURCE}}\n  a: {{MISSING}}\n"}
	_, err := Fill(tmpl, "s", "w", "", nil)
	if err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("err = %v, want it to name MISSING", err)
	}
}

// Every substituted value is a YAML-safe scalar that round-trips to exactly
// the value typed, even one containing ": ", "#", a double quote, a newline
// and "{{" — all of which would otherwise break the surrounding YAML.
func TestFillEncodesValuesYAMLSafely(t *testing.T) {
	tricky := "value: with # a \"quote\"\nand a newline and {{X}}"
	tmpl := &Template{
		Prompts:  []Prompt{{Name: "FOO", Prompt: "f"}},
		Template: "- name: {{SOURCE}}\n  a: {{FOO}}\n",
	}
	out, err := Fill(tmpl, "s", "w", "", map[string]string{"FOO": tricky})
	if err != nil {
		t.Fatal(err)
	}
	var doc []map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output did not parse as YAML: %v\n%s", err, out)
	}
	if len(doc) != 1 {
		t.Fatalf("want one entry, got %d", len(doc))
	}
	got, _ := doc[0]["a"].(string)
	if got != tricky {
		t.Errorf("round-trip = %q, want %q", got, tricky)
	}
}

func TestBuildScriptsAndParseScriptChoice(t *testing.T) {
	choices := []ScriptChoice{
		{Name: "implement", Agent: "mock"},
		{Name: "review", Script: "./scripts/review.sh"},
	}
	got := BuildScripts(choices)
	want := "implement:\n  agent: mock\nreview:\n  script: ./scripts/review.sh"
	if got != want {
		t.Errorf("BuildScripts = %q, want %q", got, want)
	}

	c, err := ParseScriptChoice("implement", "agent:mock")
	if err != nil || c.Agent != "mock" || c.Script != "" {
		t.Errorf("ParseScriptChoice(agent:mock) = %+v, %v", c, err)
	}
	c, err = ParseScriptChoice("review", "script:./x.sh")
	if err != nil || c.Script != "./x.sh" || c.Agent != "" {
		t.Errorf("ParseScriptChoice(script:./x.sh) = %+v, %v", c, err)
	}
	if _, err := ParseScriptChoice("x", "bogus"); err == nil {
		t.Error("expected an error for an unrecognised form")
	}
	if _, err := ParseScriptChoice("x", "agent:"); err == nil {
		t.Error("expected an error for an empty agent name")
	}
}

func TestReservedNames(t *testing.T) {
	got := ReservedNames()
	want := map[string]bool{"SOURCE": true, "WORKDIR": true, "SCRIPTS": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected reserved name %q", n)
		}
	}
}
