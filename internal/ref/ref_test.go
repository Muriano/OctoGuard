package ref

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

// gitRepo creates a temporary git repository whose origin remote is url.
func gitRepo(t *testing.T, url string) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", url},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func TestParse_Valid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		kind Kind
		want Ref
	}{
		{"short", "owner/repo#12", PR, Ref{"github.com", "owner", "repo", 12, PR}},
		{"short issue", "owner/repo#12", Issue, Ref{"github.com", "owner", "repo", 12, Issue}},
		{"short run", "owner/repo#99", Run, Ref{"github.com", "owner", "repo", 99, Run}},
		{"pull url", "https://github.com/o/r/pull/12", PR, Ref{"github.com", "o", "r", 12, PR}},
		{"pull url files", "https://github.com/o/r/pull/12/files", PR, Ref{"github.com", "o", "r", 12, PR}},
		{"issue url", "https://github.com/o/r/issues/12", Issue, Ref{"github.com", "o", "r", 12, Issue}},
		{"issue url fragment", "https://github.com/o/r/issues/12#issuecomment-1", Issue, Ref{"github.com", "o", "r", 12, Issue}},
		{"run url", "https://github.com/o/r/actions/runs/99", Run, Ref{"github.com", "o", "r", 99, Run}},
		{"run url job", "https://github.com/o/r/actions/runs/99/job/5?pr=1", Run, Ref{"github.com", "o", "r", 99, Run}},
		{"dotted repo", "my-org/my.repo_x#3", PR, Ref{"github.com", "my-org", "my.repo_x", 3, PR}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.in, tt.kind, t.TempDir())
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.in, err)
			}
			if got != tt.want {
				t.Fatalf("Parse(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParse_BareNumberInRepo(t *testing.T) {
	remotes := map[string]string{
		"https":          "https://github.com/owner/repo.git",
		"https no .git":  "https://github.com/owner/repo",
		"ssh scp":        "git@github.com:owner/repo.git",
		"ssh url":        "ssh://git@github.com/owner/repo.git",
		"https trailing": "https://github.com/owner/repo/",
	}
	for name, remote := range remotes {
		t.Run(name, func(t *testing.T) {
			got, err := Parse("12", PR, gitRepo(t, remote))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			want := Ref{"github.com", "owner", "repo", 12, PR}
			if got != want {
				t.Fatalf("got %+v, want %+v", got, want)
			}
		})
	}
}

func TestParseRef_BareNumberOutsideRepo(t *testing.T) {
	_, err := Parse("12", PR, t.TempDir())
	if !errors.Is(err, ErrUsage) {
		t.Fatalf("err = %v, want ErrUsage", err)
	}
	if !strings.Contains(err.Error(), "owner/repo#N") {
		t.Fatalf("message should suggest owner/repo#N: %v", err)
	}
}

func TestParseRef_OtherHostRejected(t *testing.T) {
	cases := map[string]func(t *testing.T) error{
		"url": func(t *testing.T) error {
			_, err := Parse("https://ghe.example.com/o/r/pull/1", PR, t.TempDir())
			return err
		},
		"remote": func(t *testing.T) error {
			_, err := Parse("1", PR, gitRepo(t, "git@ghe.example.com:o/r.git"))
			return err
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			err := run(t)
			if !errors.Is(err, ErrUsage) {
				t.Fatalf("err = %v, want ErrUsage", err)
			}
			if !strings.Contains(err.Error(), "only supports github.com") {
				t.Fatalf("message should say v1 only supports github.com: %v", err)
			}
		})
	}
}

func TestParse_Invalid(t *testing.T) {
	tests := []struct {
		name string
		in   string
		kind Kind
	}{
		{"empty", "", PR},
		{"kind mismatch issue url as pr", "https://github.com/o/r/issues/5", PR},
		{"kind mismatch pull url as run", "https://github.com/o/r/pull/5", Run},
		{"zero", "o/r#0", PR},
		{"negative", "o/r#-1", PR},
		{"not a number", "o/r#abc", PR},
		{"bare zero", "0", PR},
		{"missing repo", "o#1", PR},
		{"dotdot repo", "o/..#1", PR},
		{"dot repo", "o/.#1", PR},
		{"slash in owner", "a/b/c#1", PR},
		{"bad owner chars", "o_x/r#1", PR},
		{"url dotdot", "https://github.com/o/../pull/1", PR},
		{"url no number", "https://github.com/o/r/pull/", PR},
		{"url repo root", "https://github.com/o/r", PR},
		{"url bad scheme", "ftp://github.com/o/r/pull/1", PR},
		{"url userinfo", "https://x@github.com/o/r/pull/1", PR},
		{"invalid kind", "o/r#1", Kind("commit")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.in, tt.kind, t.TempDir())
			if !errors.Is(err, ErrUsage) {
				t.Fatalf("Parse(%q) = %+v, %v; want ErrUsage", tt.in, got, err)
			}
		})
	}
}

func TestRef_Key(t *testing.T) {
	r := Ref{Host: "github.com", Owner: "owner", Repo: "repo", Number: 123, Kind: PR}
	if got := r.Key(); got != "github.com/owner/repo/pr/123" {
		t.Fatalf("Key() = %q", got)
	}
	mixed := Ref{Host: "github.com", Owner: "Muriano", Repo: "OctoGuard", Number: 3, Kind: Issue}
	if got := mixed.Key(); got != "github.com/muriano/octoguard/issue/3" {
		t.Fatalf("Key() = %q, want lowercase owner/repo", got)
	}
}
