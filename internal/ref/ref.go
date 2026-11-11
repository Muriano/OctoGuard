// Package ref parses the <ref> argument that identifies a GitHub PR, issue or
// workflow run.
package ref

import (
	"errors"
	"fmt"
	"net/url"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Kind is the type of GitHub resource a ref points to.
type Kind string

// Supported kinds.
const (
	PR    Kind = "pr"
	Issue Kind = "issue"
	Run   Kind = "run"
)

// Host is the only GitHub host supported in v1.
const Host = "github.com"

// ErrUsage marks errors caused by an invalid or ambiguous ref. Callers map it
// to exit code 2.
var ErrUsage = errors.New("usage error")

// Ref identifies a PR, issue or workflow run.
type Ref struct {
	Host   string
	Owner  string
	Repo   string
	Number int
	Kind   Kind
}

// Key returns the cache key of the ref, e.g. "github.com/owner/repo/pr/123".
// Owner and repo are lowercased because GitHub treats them case-insensitively.
func (r Ref) Key() string {
	return fmt.Sprintf("%s/%s/%s/%s/%d", r.Host, strings.ToLower(r.Owner), strings.ToLower(r.Repo), r.Kind, r.Number)
}

var (
	ownerRe = regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	repoRe  = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	shortRe = regexp.MustCompile(`^([^/#]+)/([^/#]+)#(.+)$`)
)

// urlKind maps the GitHub web path segment(s) after owner/repo to a Kind.
var urlKind = map[string]Kind{"pull": PR, "issues": Issue, "actions/runs": Run}

// gitRemote returns the URL of the origin remote of the repository at cwd.
func gitRemote(cwd string) (string, error) {
	out, err := exec.Command("git", "-C", cwd, "remote", "get-url", "origin").Output()
	return strings.TrimSpace(string(out)), err
}

func usagef(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrUsage}, args...)...)
}

// Parse parses s as a ref of the given kind. It accepts "owner/repo#N", a
// github.com URL of a PR, issue or workflow run, or a bare "N" resolved
// against the origin remote of the git repository at cwd.
func Parse(s string, kind Kind, cwd string) (Ref, error) {
	if kind != PR && kind != Issue && kind != Run {
		return Ref{}, usagef("invalid ref kind %q", kind)
	}
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return Ref{}, usagef("empty ref: use owner/repo#N, a GitHub URL or N inside a git repository")
	case strings.Contains(s, "://"):
		return parseURL(s, kind)
	case shortRe.MatchString(s):
		m := shortRe.FindStringSubmatch(s)
		return build(m[1], m[2], m[3], kind)
	case !strings.ContainsAny(s, "/#"):
		remote, err := gitRemote(cwd)
		if err != nil || remote == "" {
			return Ref{}, usagef("cannot resolve %q: %s is not a git repository with an origin remote; use owner/repo#N", s, cwd)
		}
		owner, repo, err := parseRemote(remote)
		if err != nil {
			return Ref{}, err
		}
		return build(owner, repo, s, kind)
	default:
		return Ref{}, usagef("unrecognized ref %q: use owner/repo#N, a GitHub URL or N inside a git repository", s)
	}
}

func parseURL(s string, kind Kind) (Ref, error) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return Ref{}, usagef("invalid GitHub URL %q", s)
	}
	if err := checkHost(u.Hostname()); err != nil {
		return Ref{}, err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 4 {
		return Ref{}, usagef("URL %q does not point to a PR, issue or workflow run", s)
	}
	section, number := parts[2], parts[3]
	if section == "actions" && len(parts) >= 5 && parts[3] == "runs" {
		section, number = "actions/runs", parts[4]
	}
	urlK, ok := urlKind[section]
	if !ok {
		return Ref{}, usagef("URL %q does not point to a PR, issue or workflow run", s)
	}
	if urlK != kind {
		return Ref{}, usagef("URL %q points to a %s, not a %s", s, urlK, kind)
	}
	return build(parts[0], parts[1], number, kind)
}

// parseRemote extracts owner and repo from an https, ssh:// or scp-like
// (git@host:owner/repo.git) remote URL.
func parseRemote(remote string) (owner, repo string, err error) {
	var host, path string
	if strings.Contains(remote, "://") {
		u, perr := url.Parse(remote)
		if perr != nil {
			return "", "", usagef("cannot parse origin remote %q", remote)
		}
		host, path = u.Hostname(), u.Path
	} else if at, rest, ok := strings.Cut(remote, ":"); ok {
		_, host, _ = strings.Cut(at, "@")
		if host == "" {
			host = at
		}
		path = rest
	} else {
		return "", "", usagef("cannot parse origin remote %q", remote)
	}
	if err := checkHost(host); err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	if len(parts) != 2 {
		return "", "", usagef("origin remote %q is not a github.com owner/repo", remote)
	}
	return parts[0], parts[1], nil
}

func checkHost(host string) error {
	if !strings.EqualFold(host, Host) {
		return usagef("host %q is not supported: octoguard v1 only supports github.com", host)
	}
	return nil
}

func build(owner, repo, number string, kind Kind) (Ref, error) {
	if !ownerRe.MatchString(owner) {
		return Ref{}, usagef("invalid owner %q", owner)
	}
	if !repoRe.MatchString(repo) || repo == "." || repo == ".." {
		return Ref{}, usagef("invalid repository %q", repo)
	}
	n, err := strconv.Atoi(number)
	if err != nil || n <= 0 {
		return Ref{}, usagef("invalid %s number %q: must be a positive integer", kind, number)
	}
	return Ref{Host: Host, Owner: owner, Repo: repo, Number: n, Kind: kind}, nil
}
