// Package guides embeds the Markdown guides served by `octoguard guide`.
package guides

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed *.md
var files embed.FS

// Topics returns the names of the embedded guides, sorted.
func Topics() []string {
	entries, err := fs.Glob(files, "*.md")
	if err != nil {
		panic(err) // the pattern is constant and valid
	}
	topics := make([]string, len(entries))
	for i, e := range entries {
		topics[i] = strings.TrimSuffix(e, ".md")
	}
	sort.Strings(topics)
	return topics
}

// Render returns the guide for topic, headed by the binary version so that a
// reader can tell which octoguard it describes.
func Render(topic, version string) (string, error) {
	for _, t := range Topics() {
		if t == topic {
			body, err := files.ReadFile(t + ".md")
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("<!-- octoguard %s -->\n%s", version, body), nil
		}
	}
	return "", fmt.Errorf("unknown guide %q: available topics are %s", topic, strings.Join(Topics(), ", "))
}
