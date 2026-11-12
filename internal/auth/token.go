// Package auth resolves the GitHub token used by octoguard.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"strings"
)

// Token is a resolved GitHub token. Its value must never be printed, logged or
// persisted; String and GoString redact it.
type Token struct {
	Value  string
	Source string // "GH_TOKEN", "GITHUB_TOKEN" or "gh"
	ID     string // hex(sha256(Value))[:16], safe to use in cache keys
}

// String redacts the token so it never leaks through fmt or logs.
func (t Token) String() string { return "***" }

// GoString redacts the token for %#v as well.
func (t Token) GoString() string { return "***" }

// ErrNoToken is returned when no token source yields a token.
var ErrNoToken = errors.New("no GitHub token found: set GH_TOKEN or GITHUB_TOKEN, or run `gh auth login`")

// Test hooks.
var (
	getenv      = os.Getenv
	execCommand = exec.CommandContext
)

// Resolve returns the token following the precedence
// GH_TOKEN -> GITHUB_TOKEN -> `gh auth token`.
func Resolve(ctx context.Context) (Token, error) {
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return newToken(v, name), nil
		}
	}
	// stderr is discarded on purpose: it is not included in the error.
	out, err := execCommand(ctx, "gh", "auth", "token").Output()
	if err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			return newToken(v, "gh"), nil
		}
	}
	return Token{}, ErrNoToken
}

// FromValue builds the Token of a raw value received from a client, such as
// the daemon getting it over its socket.
func FromValue(value string) Token { return newToken(value, "client") }

func newToken(value, source string) Token {
	sum := sha256.Sum256([]byte(value))
	return Token{Value: value, Source: source, ID: hex.EncodeToString(sum[:])[:16]}
}
