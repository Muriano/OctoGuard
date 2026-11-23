package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Muriano/OctoGuard/internal/ref"
)

func runSkills(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetArgs(append([]string{"skills"}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestSkillsCmd_InvalidAgentIsUsageError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, args := range [][]string{
		{"install", "--agent", "cursor"},
		{"update", "--scope", "system"},
	} {
		if _, err := runSkills(t, args...); !errors.Is(err, ref.ErrUsage) {
			t.Errorf("%v: err = %v, want a usage error", args, err)
		}
	}
}

func TestSkillsCmd_InstallJSON(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	out, err := runSkills(t, "install", "--agent", "claude")
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		SchemaVersion int `json:"schema_version"`
		Results       []struct {
			Agent, Scope, Path, Action string
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	want := filepath.Join(home, ".claude", "skills", "octoguard", "SKILL.md")
	if v.SchemaVersion != 1 || len(v.Results) != 1 || v.Results[0].Path != want || v.Results[0].Action != "installed" {
		t.Fatalf("got %+v", v)
	}
}

func TestSkillsCmd_EnforceAnyAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	out, err := runSkills(t, "install", "--agent", "codex", "--enforce", "--format", "text")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".agents/skills/octoguard/SKILL.md", ".codex/AGENTS.md", ".codex/hooks.json"} {
		if !strings.Contains(out, filepath.Join(home, p)) {
			t.Errorf("output lacks %s:\n%s", p, out)
		}
	}
}

func TestHookCmd(t *testing.T) {
	run := func(agent, in string) (string, error) {
		var out bytes.Buffer
		root := NewRoot()
		root.SetOut(&out)
		root.SetIn(strings.NewReader(in))
		root.SetArgs([]string{"hook", agent})
		err := root.Execute()
		return out.String(), err
	}
	if _, err := run("cursor", "{}"); !errors.Is(err, ref.ErrUsage) {
		t.Fatalf("hook cursor: err = %v, want a usage error", err)
	}
	out, err := run("gemini", `{"tool_name":"run_shell_command","tool_input":{"command":"gh pr checks 1"}}`)
	if err != nil || !strings.Contains(out, `"decision":"deny"`) {
		t.Fatalf("hook gemini = %q, %v", out, err)
	}
}
