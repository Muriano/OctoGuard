// Command octoguard is a GitHub API quota guardian for AI agents.
package main

import (
	"fmt"
	"os"

	"github.com/Muriano/OctoGuard/internal/cli"
)

func main() {
	if err := cli.NewRoot().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "octoguard: %v\n", err)
		os.Exit(1)
	}
}
