package cli

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoot_Version(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "octoguard")
	build := exec.Command("go", "build",
		"-ldflags", "-X github.com/Muriano/OctoGuard/internal/cli.Version=v9.9.9-test",
		"-o", bin, "github.com/Muriano/OctoGuard/cmd/octoguard")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("octoguard --version: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "v9.9.9-test") {
		t.Fatalf("version output %q does not contain injected version", out)
	}
}

func TestRoot_InvalidFormat(t *testing.T) {
	root := NewRoot()
	root.SetArgs([]string{"--format", "yaml"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for --format yaml")
	}
}

func TestRoot_APIURLHidden(t *testing.T) {
	f := NewRoot().PersistentFlags().Lookup("api-url")
	if f == nil || !f.Hidden {
		t.Fatal("--api-url must exist and be hidden")
	}
}
