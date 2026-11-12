package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// runAgentContext runs `octoguard agent-context` with a fixed version.
func runAgentContext(t *testing.T) []byte {
	t.Helper()
	old := Version
	Version = "v0.0.0-test"
	t.Cleanup(func() { Version = old })
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetArgs([]string{"agent-context"})
	if err := root.Execute(); err != nil {
		t.Fatalf("agent-context: %v", err)
	}
	return out.Bytes()
}

func TestAgentContext_CoversAllCommands(t *testing.T) {
	var ac agentContext
	if err := json.Unmarshal(runAgentContext(t), &ac); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ac.SchemaVersion != 1 || ac.Version != "v0.0.0-test" {
		t.Fatalf("schema_version=%d version=%q", ac.SchemaVersion, ac.Version)
	}
	listed := map[string]commandInfo{}
	for _, c := range ac.Commands {
		listed[c.Path] = c
	}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if !describable(sub) {
				continue
			}
			path := commandPath(sub)
			got, ok := listed[path]
			if !ok {
				t.Errorf("command %q missing from agent-context", path)
			} else if sub.Runnable() && got.OutputSchema == nil {
				t.Errorf("command %q has no output_schema: add it to outputSchemas", path)
			}
			delete(listed, path)
			walk(sub)
		}
	}
	walk(NewRoot())
	for path := range listed {
		t.Errorf("agent-context lists %q, which is not a visible command", path)
	}
	if len(ac.Commands) == 0 {
		t.Fatal("no commands listed")
	}
}

func TestAgentContext_Stable(t *testing.T) {
	var got bytes.Buffer
	if err := json.Indent(&got, runAgentContext(t), "", "  "); err != nil {
		t.Fatalf("indent: %v", err)
	}
	golden := filepath.Join("testdata", "agent-context.golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, got.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("agent-context changed; if intended, run `go test ./internal/cli -run TestAgentContext_Stable -update`\ngot:\n%s", got.Bytes())
	}
}

func TestAgentContext_HidesInternalFlags(t *testing.T) {
	var ac agentContext
	if err := json.Unmarshal(runAgentContext(t), &ac); err != nil {
		t.Fatal(err)
	}
	for _, c := range ac.Commands {
		for _, f := range c.Flags {
			if f.Name == "api-url" || f.Name == "help" {
				t.Errorf("%s lists flag --%s", c.Path, f.Name)
			}
		}
	}
}
