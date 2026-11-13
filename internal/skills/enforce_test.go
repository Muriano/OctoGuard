package skills

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnforce_UserScopeKeepsExistingContent(t *testing.T) {
	home := t.TempDir()
	md := filepath.Join(home, ".claude", "CLAUDE.md")
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(md), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(md, []byte("# Mine\n\nKeep me."), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := `{"model":"opus","hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"a && b < c"}]}]}}`
	if err := os.WriteFile(settings, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Enforce(Options{Scope: ScopeUser, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Action != ActionInstalled || res[1].Action != ActionInstalled {
		t.Fatalf("results = %+v", res)
	}
	got, _ := os.ReadFile(md)
	if !strings.HasPrefix(string(got), "# Mine\n\nKeep me.\n\n"+blockBegin) || !strings.HasSuffix(string(got), blockEnd+"\n") {
		t.Fatalf("CLAUDE.md = %q", got)
	}
	raw, _ := os.ReadFile(settings)
	if strings.Contains(string(raw), "\\u0026") || !strings.Contains(string(raw), "a && b < c") {
		t.Fatalf("settings.json escaped & : %s", raw)
	}
	var s struct {
		Model string `json:"model"`
		Hooks struct {
			PreToolUse []struct {
				Matcher string `json:"matcher"`
				Hooks   []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"PreToolUse"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	pre := s.Hooks.PreToolUse
	if s.Model != "opus" || len(pre) != 2 || pre[0].Hooks[0].Command != "a && b < c" ||
		pre[1].Matcher != "Bash" || pre[1].Hooks[0].Command != HookCommand {
		t.Fatalf("settings = %s", raw)
	}
	if bak, _ := os.ReadFile(settings + ".octoguard.bak"); string(bak) != orig {
		t.Fatalf("backup = %q", bak)
	}

	// Idempotent.
	res, err = Enforce(Options{Scope: ScopeUser, Home: home})
	if err != nil || res[0].Action != ActionUnchanged || res[1].Action != ActionUnchanged {
		t.Fatalf("second run = %+v, %v", res, err)
	}
	if again, _ := os.ReadFile(md); string(again) != string(got) {
		t.Fatalf("CLAUDE.md changed on second run: %q", again)
	}
}

func TestEnforce_UpdatesOldBlock(t *testing.T) {
	root := t.TempDir()
	md := filepath.Join(root, "CLAUDE.md")
	old := "before\n" + blockBegin + "\nold text\n" + blockEnd + "\nafter\n"
	if err := os.WriteFile(md, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Enforce(Options{Scope: ScopeProject, Root: root})
	if err != nil || res[0].Action != ActionUpdated {
		t.Fatalf("results = %+v, %v", res, err)
	}
	if got, _ := os.ReadFile(md); string(got) != "before\n"+Block+"after\n" {
		t.Fatalf("CLAUDE.md = %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "settings.json")); err != nil {
		t.Fatalf("project settings not written: %v", err)
	}
}

func TestHookResponse(t *testing.T) {
	out, err := HookResponse([]byte(`{"tool_name":"Bash","tool_input":{"command":"gh pr checks 3 --watch"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]string
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	h := m["hookSpecificOutput"]
	if h["hookEventName"] != "PreToolUse" || h["permissionDecision"] != "deny" ||
		!strings.Contains(h["permissionDecisionReason"], "octoguard wait pr") {
		t.Fatalf("response = %s", out)
	}
	for _, in := range []string{
		`{"tool_name":"Bash","tool_input":{"command":"gh release create v1"}}`,
		`{"tool_name":"Read","tool_input":{"file_path":"x"}}`,
	} {
		if out, err := HookResponse([]byte(in)); err != nil || out != nil {
			t.Fatalf("HookResponse(%s) = %s, %v; want allow", in, out, err)
		}
	}
}
