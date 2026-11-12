package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Muriano/OctoGuard/internal/ref"
)

func runGuide(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := NewRoot()
	root.SetOut(&out)
	root.SetArgs(append([]string{"guide"}, args...))
	err := root.Execute()
	return out.String(), err
}

func TestGuide_ListsTopics(t *testing.T) {
	out, err := runGuide(t)
	if err != nil {
		t.Fatal(err)
	}
	var v struct {
		SchemaVersion int      `json:"schema_version"`
		Topics        []string `json:"topics"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if v.SchemaVersion != 1 || len(v.Topics) != 7 {
		t.Fatalf("got %+v", v)
	}

	out, err = runGuide(t, "--format", "text")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "quickstart\n") {
		t.Fatalf("text list %q lacks quickstart", out)
	}
}

func TestGuide_Topic(t *testing.T) {
	out, err := runGuide(t, "anti-patterns")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "<!-- octoguard "+Version+" -->\n") || !strings.Contains(out, "gh pr checks --watch") {
		t.Fatalf("unexpected guide:\n%s", out)
	}
}

func TestGuide_UnknownTopicIsUsageError(t *testing.T) {
	_, err := runGuide(t, "nope")
	if !errors.Is(err, ref.ErrUsage) {
		t.Fatalf("err = %v, want ErrUsage", err)
	}
}
