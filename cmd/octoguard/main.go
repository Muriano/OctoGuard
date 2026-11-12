// Command octoguard is a GitHub API quota guardian for AI agents.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/Muriano/OctoGuard/internal/cli"
	"github.com/Muriano/OctoGuard/internal/ref"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "octoguard: %v\n", err)
		os.Exit(exitCode(err))
	}
}

// exitCode maps an error to the process exit code: 2 for usage errors, 1 for
// the rest.
func exitCode(err error) int {
	if errors.Is(err, ref.ErrUsage) {
		return 2
	}
	return 1
}
