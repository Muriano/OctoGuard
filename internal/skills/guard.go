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
		words := strings.Fields(strings.TrimLeft(strings.TrimSpace(seg), "({"))
		for len(words) > 0 && (shellWords[words[0]] || isAssignment(words[0])) {
			words = words[1:]
		}
		if len(words) > 0 && filepath.Base(words[0]) == "gh" {
			args := make([]string, 0, len(words)-1)
			for _, w := range words[1:] {
				args = append(args, strings.Trim(w, `'"`))
			}
			out = append(out, args)
		}
	}
	return out
}

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

// replacement returns the octoguard command to use for gh args, or "".
func replacement(args []string) string {
	if len(args) < 2 {
		return ""
	}
	switch args[0] + " " + args[1] {
	case "pr view":
		return "`octoguard get pr <ref>` (also reviews, comments, checks)"
	case "pr checks":
		return "`octoguard wait pr <ref> --until checks-green` to wait, `octoguard get checks <ref>` to read"
	case "pr status":
		return "`octoguard get pr <ref>`"
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
		return "`octoguard get runs <run URL>` to read, `octoguard wait run <run URL>` to wait"
	case "api graphql":
		q := strings.Join(args[2:], " ")
		if strings.Contains(q, "pullRequest") || strings.Contains(q, "issue(") || strings.Contains(q, "rateLimit") {
			return "`octoguard get pr|issue|reviews|comments|checks <ref>` or `octoguard quota`"
		}
		return ""
	}
	if args[0] == "api" {
		return apiReplacement(args[1:])
	}
	return ""
}

// apiReplacement maps a `gh api` call onto octoguard by its endpoint.
func apiReplacement(args []string) string {
	method, path := "GET", ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-X" || a == "--method":
			if i+1 < len(args) {
				method = strings.ToUpper(args[i+1])
				i++
			}
		case strings.HasPrefix(a, "-"):
		case path == "":
			path = strings.TrimPrefix(a, "/")
		}
	}
	path, _, _ = strings.Cut(path, "?")
	switch {
	case path == "rate_limit" || path == "user":
		return "`octoguard quota`"
	case strings.HasSuffix(path, "/merge") && method == "PUT":
		return "`octoguard pr merge <ref>`"
	case strings.HasSuffix(path, "/comments") && method == "POST":
		return "`octoguard pr comment <ref>` or `octoguard issue comment <ref>`"
	case strings.Contains(path, "/check-runs") || strings.Contains(path, "/check-suites") || strings.HasSuffix(path, "/status"):
		return "`octoguard get checks <ref>`, or `octoguard wait pr <ref> --until checks-green`"
	case strings.Contains(path, "/actions/runs"):
		return "`octoguard get runs <ref or run URL>`, or `octoguard wait run <run URL>`"
	case strings.Contains(path, "/pulls") || strings.Contains(path, "/issues"):
		return "`octoguard get pr|issue|reviews|comments <ref>`, or `octoguard wait`"
	}
	return ""
}
