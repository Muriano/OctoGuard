package skills

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// frontmatter returns the YAML frontmatter lines of a SKILL.md and the body
// after it, failing when the file does not start with one.
func frontmatter(t *testing.T, b []byte) (fields map[string]string, body string) {
	t.Helper()
	text := string(b)
	if !strings.HasPrefix(text, "---\n") {
		t.Fatal("SKILL.md must start with a --- frontmatter line")
	}
	head, body, ok := strings.Cut(text[len("---\n"):], "\n---\n")
	if !ok {
		t.Fatal("SKILL.md frontmatter is not closed by ---")
	}
	fields = map[string]string{}
	for _, line := range strings.Split(head, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("frontmatter line %q is not key: value", line)
		}
		fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return fields, body
}

func TestSkill_FrontmatterValid(t *testing.T) {
	fields, body := frontmatter(t, Skill)
	if fields["name"] != "octoguard" {
		t.Errorf("name = %q, want octoguard", fields["name"])
	}
	desc := strings.ToLower(fields["description"])
	for _, word := range []string{"github", "pr", "issue", "checks", "runs", "poll", "rate limit"} {
		if !strings.Contains(desc, word) {
			t.Errorf("description does not mention %q", word)
		}
	}
	if len(fields["description"]) > 1024 {
		t.Errorf("description is %d chars, the Agent Skills limit is 1024", len(fields["description"]))
	}
	if !strings.HasPrefix(body, Marker+"\n") {
		t.Errorf("the body must start with %s", Marker)
	}
	if !strings.Contains(body, "octoguard guide quickstart") {
		t.Error("the body must point to `octoguard guide quickstart`")
	}
}

func TestSkill_UnderLineBudget(t *testing.T) {
	_, body := frontmatter(t, Skill)
	if n := strings.Count(strings.TrimRight(body, "\n"), "\n") + 1; n >= 40 {
		t.Fatalf("body has %d lines, budget is under 40", n)
	}
}

func TestSkill_PublishedCopyInSync(t *testing.T) {
	published, err := os.ReadFile(filepath.Join("..", "..", "skills", "octoguard", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(published, Skill) {
		t.Fatal("skills/octoguard/SKILL.md differs from internal/skills/SKILL.md: copy it over")
	}
}

func TestInstall_AllAgentsInTempHome(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	want := map[string]map[string]string{
		ScopeUser: {
			"claude": ".claude/skills/octoguard", "codex": ".agents/skills/octoguard",
			"gemini": ".gemini/skills/octoguard", "copilot": ".copilot/skills/octoguard",
		},
		ScopeProject: {
			"claude": ".claude/skills/octoguard", "codex": ".agents/skills/octoguard",
			"gemini": ".gemini/skills/octoguard", "copilot": ".github/skills/octoguard",
		},
	}
	for scope, dirs := range want {
		base := home
		if scope == ScopeProject {
			base = root
		}
		results, err := Install(Options{Agents: Agents, Scope: scope, Home: home, Root: root})
		if err != nil {
			t.Fatalf("%s: %v", scope, err)
		}
		if len(results) != len(Agents) {
			t.Fatalf("%s: %d results, want %d", scope, len(results), len(Agents))
		}
		for _, r := range results {
			path := filepath.Join(base, dirs[r.Agent], "SKILL.md")
			if r.Path != path || r.Action != ActionInstalled || r.Scope != scope {
				t.Errorf("%s: got %+v, want path %s installed", scope, r, path)
			}
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, Skill) {
				t.Errorf("%s: %s not written: %v", scope, path, err)
			}
		}
		// A second install over managed files updates them in place.
		again, err := Install(Options{Agents: Agents, Scope: scope, Home: home, Root: root})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range again {
			if r.Action != ActionUpdated {
				t.Errorf("%s: reinstall %s = %s, want updated", scope, r.Agent, r.Action)
			}
		}
	}
}

func TestUpdate_DoesNotClobberUnmanaged(t *testing.T) {
	home := t.TempDir()
	opts := func(agents ...string) Options {
		return Options{Agents: agents, Scope: ScopeUser, Home: home}
	}
	if _, err := Install(opts("claude", "gemini")); err != nil {
		t.Fatal(err)
	}
	// The user rewrote the claude skill: no marker any more.
	own := []byte("---\nname: octoguard\ndescription: mine\n---\nmy own text\n")
	claude := filepath.Join(home, ".claude/skills/octoguard/SKILL.md")
	if err := os.WriteFile(claude, own, 0o644); err != nil {
		t.Fatal(err)
	}
	// An old managed gemini skill must be refreshed.
	gemini := filepath.Join(home, ".gemini/skills/octoguard/SKILL.md")
	if err := os.WriteFile(gemini, []byte("---\nname: octoguard\n---\n"+Marker+"\nold\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The codex skill is a symlink to a file of the user.
	target := filepath.Join(home, "elsewhere.md")
	if err := os.WriteFile(target, []byte(Marker+"\nlinked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(home, ".agents/skills/octoguard/SKILL.md")
	if err := os.MkdirAll(filepath.Dir(codex), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, codex); err != nil {
		t.Fatal(err)
	}

	results, err := Update(opts(Agents...))
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, r := range results {
		actions[r.Agent] = r.Action
	}
	wantActions := map[string]string{
		"claude": ActionSkippedUnmanaged, "codex": ActionSkippedUnmanaged,
		"gemini": ActionUpdated, "copilot": ActionNotInstalled,
	}
	for agent, want := range wantActions {
		if actions[agent] != want {
			t.Errorf("%s: %s, want %s", agent, actions[agent], want)
		}
	}
	if got, _ := os.ReadFile(claude); !bytes.Equal(got, own) {
		t.Error("update clobbered an unmanaged SKILL.md")
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, []byte(Marker+"\nlinked\n")) {
		t.Error("update wrote through a symlink")
	}
	if got, _ := os.ReadFile(gemini); !bytes.Equal(got, Skill) {
		t.Error("update did not refresh a managed SKILL.md")
	}
	if _, err := os.Stat(filepath.Join(home, ".copilot")); !os.IsNotExist(err) {
		t.Error("update created a skill that was not installed")
	}
	// install must not clobber the unmanaged file either.
	if r, err := Install(opts("claude")); err != nil || r[0].Action != ActionSkippedUnmanaged {
		t.Errorf("install over unmanaged: %+v, %v", r, err)
	}
}

func TestProjectRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectRoot(sub); got != root {
		t.Errorf("ProjectRoot(%s) = %s, want %s", sub, got, root)
	}
}
