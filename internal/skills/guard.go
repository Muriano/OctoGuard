package skills

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Guard reports whether a shell command runs a gh command that octoguard
// replaces, and if so what to run instead. gh commands octoguard has no
// equivalent for (releases, repo settings, auth, listings, ...) are allowed.
func Guard(command string) (deny bool, reason string) {
	for _, args := range ghInvocations(command) {
		if use := replacement(args); use != "" {
			return true, "Do not use `gh " + strings.Join(args, " ") + "`: octoguard replaces it with a shared " +
				"cache, ETags and waits that spend no quota. Use " + use + ". Unsure? `octoguard guide quickstart`."
		}
	}
	return false, ""
}

// heredoc matches a here-document operator (not a <<< here-string) and
// captures its delimiter word.
var heredoc = regexp.MustCompile(`(?:^|[^<])<<-?\s*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?`)

// shellWords are keywords and wrappers that can precede a command.
var shellWords = map[string]bool{
	"until": true, "while": true, "do": true, "then": true, "else": true, "if": true,
	"!": true, "time": true, "command": true, "exec": true, "{": true, "nohup": true,
}

// ghInvocations returns the arguments of every gh command in a shell command
// line. It splits on shell separators (see segments) and skips keywords and
// VAR=value prefixes; it is a heuristic, not a shell parser.
func ghInvocations(command string) [][]string {
	var out [][]string
	for _, seg := range segments(command) {
		words := shellFields(strings.TrimLeft(strings.TrimSpace(seg), "({"))
		for len(words) > 0 && (shellWords[words[0]] || isAssignment(words[0])) {
			words = words[1:]
		}
		if len(words) > 0 && filepath.Base(words[0]) == "gh" {
			out = append(out, words[1:])
		}
	}
	return out
}

// shellFields splits s on blanks outside quotes. A quoted span stays in one
// word with its quotes, so a value like "adds --enforce" is never a flag.
func shellFields(s string) []string {
	var words []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\\' && i+1 < len(s):
			cur.WriteByte(c)
			i++
			c = s[i]
		case c == '\'' || c == '"':
			quote = c
		case c == ' ' || c == '\t' || c == '\n':
			if cur.Len() > 0 {
				words = append(words, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		words = append(words, cur.String())
	}
	return words
}

// unquote drops the quotes of a shell word.
func unquote(w string) string { return strings.NewReplacer(`'`, "", `"`, "").Replace(w) }

// segments splits a command line where a new command can start: ; | & and
// newlines outside quotes, and the start of a $( or backtick substitution
// (which runs even inside double quotes). Single-quoted text and heredoc
// bodies are data, never commands, so they are kept whole.
func segments(command string) []string {
	var segs []string
	var cur strings.Builder
	cut := func() {
		segs = append(segs, cur.String())
		cur.Reset()
	}
	s := dropHeredocBodies(command)
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '\\' && i+1 < len(s):
			cur.WriteByte(c)
			i++
			c = s[i]
		case c == '`':
			cut()
			continue
		case c == '$' && i+1 < len(s) && s[i+1] == '(':
			i++
			cut()
			continue
		case quote == '"':
			if c == '"' {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ';' || c == '|' || c == '&' || c == '\n':
			cut()
			continue
		}
		cur.WriteByte(c)
	}
	cut()
	return segs
}

// dropHeredocBodies removes the body lines of every here-document: they are
// input data, not commands.
func dropHeredocBodies(command string) string {
	var out, ends []string
	for _, line := range strings.Split(command, "\n") {
		if len(ends) > 0 {
			if strings.TrimSpace(line) == ends[0] {
				ends = ends[1:]
			}
			continue
		}
		out = append(out, line)
		for _, m := range heredoc.FindAllStringSubmatch(line, -1) {
			ends = append(ends, m[1])
		}
	}
	return strings.Join(out, "\n")
}

func isAssignment(w string) bool {
	i := strings.IndexByte(w, '=')
	return i > 0 && !strings.ContainsAny(w[:i], "-/.'\"")
}

// replacement returns the octoguard command to use for gh args (shell words,
// quotes kept), or "". It names a replacement only when octoguard gives the
// same data or does the same write: any flag, endpoint or field it lacks lets
// gh run.
func replacement(words []string) string {
	if len(words) < 2 {
		return ""
	}
	args := make([]string, len(words))
	for i, w := range words {
		args[i] = unquote(w)
	}
	sub := args[0] + " " + args[1]
	if allowed, ok := coveredFlags[sub]; ok && !onlyFlags(words[2:], allowed) {
		return "" // a flag octoguard has no equivalent for: let gh do it
	}
	if fields, ok := jsonFields[sub]; ok && !jsonCovered(args[2:], fields) {
		return "" // --json asks for a field octoguard does not return
	}
	switch sub {
	case "pr view":
		return "`octoguard get pr <ref>` (also reviews, comments, checks)"
	case "pr checks":
		return "`octoguard wait pr <ref> --until checks-green` to wait, `octoguard get checks <ref>` to read"
	case "pr merge":
		return "`octoguard pr merge <ref> [--squash]`"
	case "pr comment", "pr create", "pr edit":
		return "`octoguard " + args[0] + " " + args[1] + "`"
	case "issue view":
		return "`octoguard get issue <ref>` (also comments)"
	case "issue comment", "issue create":
		return "`octoguard " + args[0] + " " + args[1] + "`"
	case "run watch":
		return "`octoguard wait run <run URL>`"
	case "run view":
		return "`octoguard get runs <run URL>` to read the run, `octoguard wait run <run URL>` to wait. " +
			"For job logs `gh run view <id> --log-failed` (or `--log`, `--job`) is allowed"
	case "api graphql":
		if graphQLCovered(words[2:]) {
			return "`octoguard get pr|issue|reviews|comments|checks <ref>` or `octoguard quota`"
		}
		return ""
	}
	if args[0] == "api" {
		return apiReplacement(args[1:])
	}
	return ""
}

// coveredFlags lists, for the gh commands octoguard replaces, the flags it has
// an equivalent for. A call with any other flag (--remove-label, --draft,
// --auto, --log-failed, --web, ...) is let through.
var coveredFlags = map[string]map[string]bool{
	"pr create": set("--title", "-t", "--body", "-b", "--body-file", "-F", "--base", "-B", "--head", "-H",
		"--label", "-l", "--reviewer", "-r", "--assignee", "-a", "--repo", "-R"),
	"pr edit": set("--title", "-t", "--body", "-b", "--body-file", "-F", "--base", "-B",
		"--add-label", "--add-reviewer", "--add-assignee", "--repo", "-R"),
	"pr merge":      set("--merge", "-m", "--squash", "-s", "--rebase", "-r", "--repo", "-R"),
	"pr comment":    set("--body", "-b", "--body-file", "-F", "--repo", "-R"),
	"issue comment": set("--body", "-b", "--body-file", "-F", "--repo", "-R"),
	"issue create":  set("--title", "-t", "--body", "-b", "--body-file", "-F", "--label", "-l", "--assignee", "-a", "--repo", "-R"),
	"pr view":       set("--comments", "-c", "--json", "--jq", "-q", "--template", "-t", "--repo", "-R"),
	"issue view":    set("--comments", "-c", "--json", "--jq", "-q", "--template", "-t", "--repo", "-R"),
	"pr checks":     set("--watch", "--fail-fast", "--interval", "-i", "--required", "--repo", "-R"),
	"run view":      set("--repo", "-R"),
	"run watch":     set("--exit-status", "--interval", "-i", "--compact", "--repo", "-R"),
}

// jsonFields lists the `--json` fields octoguard returns for gh views.
var jsonFields = map[string]map[string]bool{
	"pr view": set("number", "title", "body", "state", "isDraft", "url", "createdAt", "updatedAt", "closedAt",
		"mergedAt", "author", "labels", "mergeable", "mergeStateStatus", "headRefName", "headRefOid",
		"baseRefName", "reviews", "statusCheckRollup", "comments"),
	"issue view": set("number", "title", "body", "state", "url", "createdAt", "updatedAt", "closedAt",
		"author", "labels", "comments"),
}

// jsonCovered reports whether every field a --json flag in args asks for is
// in covered. No --json flag means nothing extra is asked for.
func jsonCovered(args []string, covered map[string]bool) bool {
	for i, a := range args {
		var list string
		switch {
		case a == "--json" && i+1 < len(args):
			list = args[i+1]
		case strings.HasPrefix(a, "--json="):
			list = strings.TrimPrefix(a, "--json=")
		default:
			continue
		}
		for _, f := range strings.Split(list, ",") {
			// A $( or backtick substitution can leave its closer on the last field.
			if !covered[strings.TrimRight(strings.TrimSpace(f), ")`")] {
				return false
			}
		}
	}
	return true
}

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

// onlyFlags reports whether every flag in words (--flag, --flag=value, -f) is
// in allowed. Other words (refs, values, anything quoted) are ignored.
func onlyFlags(words []string, allowed map[string]bool) bool {
	for _, a := range words {
		if !strings.HasPrefix(a, "-") || a == "-" {
			continue
		}
		name, _, _ := strings.Cut(a, "=")
		if !allowed[name] {
			return false
		}
	}
	return true
}

// graphQLFields are the GraphQL names octoguard's own reads select (see
// internal/gh/resources.go), plus query syntax. A query that only uses these
// asks for nothing octoguard does not return.
var graphQLFields = set("query", "repository", "owner", "repo", "name", "number", "pullRequest", "issue",
	"issueOrPullRequest", "title", "body", "state", "isDraft", "merged", "mergeable", "mergeStateStatus", "url",
	"createdAt", "updatedAt", "closedAt", "mergedAt", "headRefName", "headRefOid", "baseRefName", "author",
	"login", "labels", "nodes", "edges", "node", "reviews", "databaseId", "submittedAt", "comments", "commits",
	"commit", "oid", "statusCheckRollup", "contexts", "__typename", "on", "CheckRun", "StatusContext", "status",
	"conclusion", "context", "rateLimit", "cost", "remaining", "limit", "resetAt", "used", "first", "last",
	"String", "Int")

var (
	gqlString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)
	gqlName   = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
)

// graphQLCovered reports whether the query passed to `gh api graphql` (as
// shell words) is a read of PR, issue or quota data octoguard serves. Mutations,
// queries read from a file and any unknown field are not covered.
func graphQLCovered(words []string) bool {
	var query string
	for _, w := range words {
		if v, ok := strings.CutPrefix(w, "query="); ok {
			query = v
		}
	}
	query = strings.Trim(query, `'"`)
	if query == "" || strings.HasPrefix(query, "@") {
		return false
	}
	names := gqlName.FindAllString(gqlString.ReplaceAllString(query, ""), -1)
	if len(names) == 0 {
		return false
	}
	for _, n := range names {
		if !graphQLFields[n] {
			return false
		}
	}
	return true
}

// apiValueFlags are the `gh api` flags that take a value.
var apiValueFlags = set("-X", "--method", "-q", "--jq", "-t", "--template", "-H", "--header",
	"-f", "--raw-field", "-F", "--field", "--input", "--cache", "-p", "--preview", "--hostname")

// apiPath matches the endpoints octoguard serves, after repos/{owner}/{repo}/.
var (
	apiRepo     = regexp.MustCompile(`^repos/[^/]+/[^/]+/(.+)$`)
	apiPR       = regexp.MustCompile(`^pulls/\d+(/reviews)?$`)
	apiIssue    = regexp.MustCompile(`^issues/\d+(/comments)?$`)
	apiComment  = regexp.MustCompile(`^issues/\d+/comments$`)
	apiMerge    = regexp.MustCompile(`^pulls/\d+/merge$`)
	apiRun      = regexp.MustCompile(`^actions/runs/\d+$`)
	apiChecks   = regexp.MustCompile(`^commits/[^/]+/(check-runs|status)$`)
	apiHeadRuns = regexp.MustCompile(`^head_sha=[^&]+$`)
)

// apiReplacement maps a `gh api` call onto octoguard by its method and exact
// endpoint. Endpoints octoguard does not serve (logs, jobs, files, inline
// review comments, listings, ...) are let through.
func apiReplacement(args []string) string {
	method, path, fields := "", "", false
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, _, hasValue := strings.Cut(a, "=")
		switch {
		case apiValueFlags[name] && strings.HasPrefix(a, "-"):
			v := ""
			if hasValue {
				v = a[len(name)+1:]
			} else if i+1 < len(args) {
				v = args[i+1]
				i++
			}
			switch name {
			case "-X", "--method":
				method = strings.ToUpper(v)
			case "-f", "--raw-field", "-F", "--field", "--input":
				fields = true
			}
		case strings.HasPrefix(a, "-"):
		case path == "":
			path = strings.TrimPrefix(a, "/")
		}
	}
	if method == "" {
		method = "GET"
		if fields { // gh api sends POST when it has fields
			method = "POST"
		}
	}
	path, query, _ := strings.Cut(path, "?")
	if path == "rate_limit" || path == "user" {
		if method == "GET" {
			return "`octoguard quota`"
		}
		return ""
	}
	m := apiRepo.FindStringSubmatch(path)
	if m == nil {
		return ""
	}
	rest := m[1]
	switch {
	case method == "PUT" && apiMerge.MatchString(rest):
		return "`octoguard pr merge <ref>`"
	case method == "POST" && apiComment.MatchString(rest):
		return "`octoguard pr comment <ref>` or `octoguard issue comment <ref>`"
	case method != "GET":
		return ""
	case apiChecks.MatchString(rest):
		return "`octoguard get checks <ref>`, or `octoguard wait pr <ref> --until checks-green`"
	case apiRun.MatchString(rest), rest == "actions/runs" && apiHeadRuns.MatchString(query):
		return "`octoguard get runs <ref or run URL>`, or `octoguard wait run <run URL>`"
	case apiPR.MatchString(rest) || apiIssue.MatchString(rest):
		return "`octoguard get pr|issue|reviews|comments <ref>`, or `octoguard wait`"
	}
	return ""
}
