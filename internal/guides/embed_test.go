package guides

import (
	"slices"
	"strings"
	"testing"
)

// maxLines keeps every rendered guide within one terminal screen.
const maxLines = 25

func TestGuide_AllTopicsEmbeddedAndVersioned(t *testing.T) {
	want := []string{"anti-patterns", "quickstart", "quota", "reading", "stats", "waiting", "writes"}
	if got := Topics(); !slices.Equal(got, want) {
		t.Fatalf("Topics() = %v, want %v", got, want)
	}
	for _, topic := range want {
		out, err := Render(topic, "v1.2.3")
		if err != nil {
			t.Fatalf("Render(%q): %v", topic, err)
		}
		if !strings.HasPrefix(out, "<!-- octoguard v1.2.3 -->\n") {
			t.Errorf("%s: missing version header, starts with %q", topic, strings.SplitN(out, "\n", 2)[0])
		}
		if n := strings.Count(strings.TrimRight(out, "\n"), "\n") + 1; n > maxLines {
			t.Errorf("%s: %d lines, want at most %d", topic, n, maxLines)
		}
	}
}

func TestGuide_AntiPatternsNamesEachBannedCall(t *testing.T) {
	out, err := Render("anti-patterns", "v0")
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"sleep N && gh", "gh pr checks --watch", "gh api rate_limit", "gh api user"} {
		if !strings.Contains(out, banned) {
			t.Errorf("anti-patterns does not mention %q", banned)
		}
	}
}

func TestGuide_UnknownTopic(t *testing.T) {
	if _, err := Render("nope", "v0"); err == nil {
		t.Fatal("expected an error for an unknown topic")
	}
}
