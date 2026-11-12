package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/Muriano/OctoGuard/internal/cli"
	"github.com/Muriano/OctoGuard/internal/ref"
)

func TestExitCode(t *testing.T) {
	_, parseErr := ref.Parse("https://ghe.example.com/o/r/pull/1", ref.PR, t.TempDir())
	if parseErr == nil {
		t.Fatal("expected a parse error for another host")
	}
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"wrapped usage", fmt.Errorf("%w: bad ref", ref.ErrUsage), 2},
		{"ref.Parse usage", parseErr, 2},
		{"generic", errors.New("github: 500"), 1},
		{"wait pending", cli.ErrPending, 3},
	} {
		if got := exitCode(tc.err); got != tc.want {
			t.Errorf("%s: exitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}
