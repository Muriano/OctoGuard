package cli

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// agentContext is the document printed by `octoguard agent-context`.
type agentContext struct {
	SchemaVersion int           `json:"schema_version"`
	Version       string        `json:"version"`
	Commands      []commandInfo `json:"commands"`
}

type commandInfo struct {
	Path         string            `json:"path"`
	Short        string            `json:"short"`
	Args         string            `json:"args"`
	Flags        []flagInfo        `json:"flags"`
	ExitCodes    map[string]string `json:"exit_codes"`
	OutputSchema *outputSchema     `json:"output_schema"`
}

type flagInfo struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	Type      string `json:"type"`
	Default   string `json:"default"`
	Usage     string `json:"usage"`
}

// outputSchema describes what a command prints on stdout with the default
// --format json: the top-level fields of its JSON object.
type outputSchema struct {
	Format string   `json:"format"`
	Fields []string `json:"fields,omitempty"`
	Note   string   `json:"note,omitempty"`
}

var (
	daemonSchema = &outputSchema{Format: "json", Fields: []string{"schema_version", "running", "pid", "version", "started_at", "connections", "pollers", "waiters"}, Note: "pid and the rest only when running"}
	skillsSchema = &outputSchema{Format: "json", Fields: []string{"schema_version", "results"}, Note: "each result: agent, scope, path, action (installed|updated|skipped_unmanaged|not_installed)"}
	writeSchema  = &outputSchema{Format: "json", Fields: []string{"schema_version", "op", "status", "data", "reason", "hint"}, Note: "status ok carries data; status rejected carries reason and hint"}
)

// outputSchemas maps the path of every runnable command to its output. A test
// fails when a runnable command is missing here.
var outputSchemas = map[string]*outputSchema{
	"agent-context":     {Format: "json", Fields: []string{"schema_version", "version", "commands"}},
	"daemon run":        {Format: "none", Note: "runs the daemon in the foreground until stopped"},
	"daemon status":     daemonSchema,
	"daemon stop":       daemonSchema,
	"get":               {Format: "json", Fields: []string{"schema_version", "ref", "data", "meta"}, Note: "meta: from_cache, not_modified, via, fetched_at"},
	"guide":             {Format: "json", Fields: []string{"schema_version", "topics"}, Note: "with a topic: the Markdown guide"},
	"issue comment":     writeSchema,
	"issue create":      writeSchema,
	"pr comment":        writeSchema,
	"pr create":         writeSchema,
	"pr edit":           writeSchema,
	"pr merge":          writeSchema,
	"project item-add":  writeSchema,
	"project item-edit": writeSchema,
	"quota":             {Format: "json", Fields: []string{"schema_version", "core", "graphql", "search", "meta"}, Note: "each bucket: remaining, limit, reset, known; meta: probed"},
	"skills install":    skillsSchema,
	"skills update":     skillsSchema,
	"stats":             {Format: "text", Fields: []string{"schema_version", "since", "baseline", "at", "quota", "saved", "hourly", "rate_limits"}, Note: "a panel by default; the fields are for an explicit --format json"},
	"wait":              {Format: "json", Fields: []string{"schema_version", "ref", "status", "condition", "changes", "state"}, Note: "status: met|failed|pending"},
}

// extraExitCodes are the exit codes of a command beyond 0, 1 and 2.
var extraExitCodes = map[string]map[string]string{
	"wait":     {"3": "pending: the timeout expired first; run the same command again"},
	"pr merge": {"4": "merge rejected: checks are not green"},
}

func newAgentContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "agent-context",
		Short: "Print the machine-readable schema of every command (JSON, for agents)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(buildAgentContext(cmd.Root()))
		},
	}
}

func buildAgentContext(root *cobra.Command) agentContext {
	ac := agentContext{SchemaVersion: 1, Version: Version, Commands: []commandInfo{}}
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			if !describable(sub) {
				continue
			}
			ac.Commands = append(ac.Commands, describe(sub))
			walk(sub)
		}
	}
	walk(root)
	sort.Slice(ac.Commands, func(i, j int) bool { return ac.Commands[i].Path < ac.Commands[j].Path })
	return ac
}

// describable reports whether c is part of octoguard's interface: visible and
// not one of the help and completion commands cobra adds on its own.
func describable(c *cobra.Command) bool {
	return c.IsAvailableCommand() && c.Name() != "help" && c.Name() != "completion"
}

// commandPath is the path of c without the binary name, e.g. "wait" or "pr merge".
func commandPath(c *cobra.Command) string {
	return strings.TrimPrefix(c.CommandPath(), c.Root().Name()+" ")
}

func describe(c *cobra.Command) commandInfo {
	path := commandPath(c)
	info := commandInfo{
		Path:      path,
		Short:     c.Short,
		Flags:     []flagInfo{},
		ExitCodes: map[string]string{"0": "ok", "1": "error", "2": "usage error"},
	}
	if _, args, ok := strings.Cut(c.Use, " "); ok {
		info.Args = args
	}
	add := func(f *pflag.Flag) {
		// help and version are added by cobra, and not on every command.
		if f.Hidden || f.Name == "help" || f.Name == "version" {
			return
		}
		info.Flags = append(info.Flags, flagInfo{
			Name: f.Name, Shorthand: f.Shorthand, Type: f.Value.Type(), Default: f.DefValue, Usage: f.Usage,
		})
	}
	c.LocalFlags().VisitAll(add)
	c.InheritedFlags().VisitAll(add)
	sort.Slice(info.Flags, func(i, j int) bool { return info.Flags[i].Name < info.Flags[j].Name })
	for code, meaning := range extraExitCodes[path] {
		info.ExitCodes[code] = meaning
	}
	if c.Runnable() {
		info.OutputSchema = outputSchemas[path]
	}
	return info
}
