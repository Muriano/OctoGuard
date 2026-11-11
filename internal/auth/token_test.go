package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// fakeEnv replaces getenv with a fixed map for the duration of the test.
func fakeEnv(t *testing.T, env map[string]string) {
	t.Helper()
	orig := getenv
	getenv = func(k string) string { return env[k] }
	t.Cleanup(func() { getenv = orig })
}

// fakeGh replaces execCommand with a helper process that prints out to stdout
// and exits with code. It records whether it was called.
func fakeGh(t *testing.T, out string, code int) *bool {
	t.Helper()
	called := false
	orig := execCommand
	execCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		called = true
		if name != "gh" || strings.Join(args, " ") != "auth token" {
			t.Errorf("unexpected command: %s %v", name, args)
		}
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestHelperProcess", "--")
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"HELPER_OUT="+out,
			fmt.Sprintf("HELPER_CODE=%d", code),
		)
		return cmd
	}
	t.Cleanup(func() { execCommand = orig })
	return &called
}

func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	_, _ = fmt.Fprint(os.Stdout, os.Getenv("HELPER_OUT"))
	if os.Getenv("HELPER_CODE") != "0" {
		os.Exit(1)
	}
	os.Exit(0)
}

func wantID(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])[:16]
}

func TestResolve_PrefersGHToken(t *testing.T) {
	fakeEnv(t, map[string]string{"GH_TOKEN": "gh-tok", "GITHUB_TOKEN": "github-tok"})
	called := fakeGh(t, "cli-tok", 0)

	tok, err := Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "gh-tok" || tok.Source != "GH_TOKEN" || tok.ID != wantID("gh-tok") {
		t.Fatalf("got Source=%q ID=%q, value match=%v", tok.Source, tok.ID, tok.Value == "gh-tok")
	}
	if len(tok.ID) != 16 {
		t.Fatalf("ID length = %d, want 16", len(tok.ID))
	}
	if *called {
		t.Fatal("gh must not be called when GH_TOKEN is set")
	}
}

func TestResolve_FallsBackToGitHubToken(t *testing.T) {
	fakeEnv(t, map[string]string{"GITHUB_TOKEN": "github-tok"})
	called := fakeGh(t, "cli-tok", 0)

	tok, err := Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if tok.Value != "github-tok" || tok.Source != "GITHUB_TOKEN" || tok.ID != wantID("github-tok") {
		t.Fatalf("got Source=%q ID=%q", tok.Source, tok.ID)
	}
	if *called {
		t.Fatal("gh must not be called when GITHUB_TOKEN is set")
	}
}

func TestResolve_FallsBackToGhCLI(t *testing.T) {
	fakeEnv(t, map[string]string{"GH_TOKEN": "  "})
	called := fakeGh(t, "cli-tok\n", 0)

	tok, err := Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !*called {
		t.Fatal("expected gh auth token to be called")
	}
	if tok.Value != "cli-tok" || tok.Source != "gh" || tok.ID != wantID("cli-tok") {
		t.Fatalf("got Source=%q ID=%q", tok.Source, tok.ID)
	}
}

func TestResolve_NoTokenError(t *testing.T) {
	fakeEnv(t, map[string]string{})
	fakeGh(t, "", 1)

	_, err := Resolve(context.Background())
	if !errors.Is(err, ErrNoToken) {
		t.Fatalf("err = %v, want ErrNoToken", err)
	}
	if !strings.Contains(err.Error(), "gh auth login") {
		t.Fatalf("error %q should suggest `gh auth login`", err)
	}
}

func TestToken_StringRedacted(t *testing.T) {
	tok := newToken("super-secret", "GH_TOKEN")
	for _, verb := range []string{"%v", "%+v", "%#v", "%s"} {
		if s := fmt.Sprintf(verb, tok); strings.Contains(s, "super-secret") {
			t.Fatalf("%s leaks token: %q", verb, s)
		}
	}
	if tok.String() != "***" {
		t.Fatalf("String() = %q, want ***", tok.String())
	}
}
