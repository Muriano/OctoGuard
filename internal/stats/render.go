package stats

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	minWidth = 30
	indent   = "  "
	labelW   = 9 // width of the row labels ("graphql", "etag 304"...)
)

// ANSI styles, used only when color is on.
const (
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
	cyan   = "\x1b[36m"
	reset  = "\x1b[0m"
)

var sparkLevels = []rune("▁▂▃▄▅▆▇█")

// Render draws r as a terminal panel at most width columns wide: quota bars,
// savings bars by mechanism, hourly sparklines and the rate limit line. With
// color off it writes no escape codes.
func Render(w io.Writer, r Report, width int, color bool) {
	if width < minWidth {
		width = minWidth
	}
	p := &panel{w: w, width: width, color: color}

	p.line(p.paint(bold, fit("OctoGuard stats · last "+sinceLabel(r.Since), width)))
	p.blank()

	p.line(p.paint(bold, "Quota"))
	for _, name := range quotaOrder(r) {
		p.quotaRow(name, r)
	}
	p.blank()

	s := r.Saved
	p.line(p.paint(bold, fit(fmt.Sprintf("Saved  %d of %d calls (%d%%)", s.RESTSaved, s.TotalCalls, pct(s.RESTSaved, s.TotalCalls)), width)))
	numW := len(fmt.Sprint(maxOf(s.Cache, s.ETag304, s.Dedup, s.WaitVsPoll)))
	for _, row := range []struct {
		label string
		n     int64
	}{{"cache", s.Cache}, {"etag 304", s.ETag304}, {"dedup", s.Dedup}, {"wait", s.WaitVsPoll}} {
		suffix := fmt.Sprintf(" %*d", numW, row.n)
		bw := width - len(indent) - labelW - 1 - len(suffix)
		p.line(indent + pad(row.label) + " " + p.bar(row.n, s.RESTSaved, bw, cyan) + suffix)
	}
	p.line(fit(fmt.Sprintf("%sREST calls %d · GraphQL points %d", indent, s.RESTSaved, s.GraphQLPointsSaved), width))
	p.blank()

	realCalls, avoided := series(r.Hourly)
	peak := maxOf(append(append([]int64{}, realCalls...), avoided...)...)
	p.line(p.paint(bold, fit(fmt.Sprintf("Hourly  oldest → now, peak %d/h", peak), width)))
	sw := width - len(indent) - labelW - 1
	p.line(indent + pad("real") + " " + p.paint(yellow, spark(realCalls, peak, sw)))
	p.line(indent + pad("avoided") + " " + p.paint(cyan, spark(avoided, peak, sw)))
	p.blank()

	rl := r.RateLimits
	suffered := p.paint(green, fmt.Sprint(rl.Suffered))
	if rl.Suffered > 0 {
		suffered = p.paint(red, fmt.Sprint(rl.Suffered))
	}
	// The visible text is short enough for minWidth.
	p.line(fmt.Sprintf("%s  avoided %d · suffered %s", p.paint(bold, "Rate limits"), rl.Avoided, suffered))
	p.blank()

	for _, l := range wrap(footer(r.Baseline), width) {
		p.line(p.paint(dim, l))
	}
}

type panel struct {
	w     io.Writer
	width int
	color bool
}

func (p *panel) line(s string) { _, _ = fmt.Fprintln(p.w, s) }
func (p *panel) blank()        { _, _ = fmt.Fprintln(p.w) }

func (p *panel) paint(style, s string) string {
	if !p.color || s == "" {
		return s
	}
	return style + s + reset
}

// bar draws n/total as a bar of width cells (at least 1).
func (p *panel) bar(n, total int64, width int, style string) string {
	width = max(width, 1)
	full := 0
	if total > 0 {
		full = int(int64(width) * min(n, total) / total)
	}
	if n > 0 && full == 0 {
		full = 1
	}
	return p.paint(style, strings.Repeat("█", full)) + strings.Repeat("░", width-full)
}

func (p *panel) quotaRow(name string, r Report) {
	b, ok := r.Quota[name]
	if !ok || !b.Known || b.Limit <= 0 {
		p.line(fit(indent+pad(name)+" unknown", p.width))
		return
	}
	used := int64(b.Limit - b.Remaining)
	pc := pct(used, int64(b.Limit))
	short := fmt.Sprintf(" %3d%%", pc)
	suffix := fmt.Sprintf("%s %11s reset %5s", short, fmt.Sprintf("%d/%d", used, b.Limit), until(b.Reset, r.At))
	bw := p.width - len(indent) - labelW - 1 - utf8.RuneCountInString(suffix)
	if bw < 10 {
		suffix = short
		bw = p.width - len(indent) - labelW - 1 - len(suffix)
	}
	style := green
	switch {
	case pc >= 95:
		style = red
	case pc >= 80:
		style = yellow
	}
	p.line(indent + pad(name) + " " + p.bar(used, int64(b.Limit), bw, style) + suffix)
}

// quotaOrder lists core, graphql and search first, then any other known
// resource alphabetically.
func quotaOrder(r Report) []string {
	order := []string{"core", "graphql", "search"}
	var extra []string
	for name := range r.Quota {
		if name != "core" && name != "graphql" && name != "search" {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	return append(order, extra...)
}

func series(h []HourPoint) (realCalls, avoided []int64) {
	for _, p := range h {
		realCalls = append(realCalls, p.Real)
		avoided = append(avoided, p.Avoided)
	}
	return realCalls, avoided
}

// spark draws vs in at most width cells, summing consecutive values when
// there are more values than cells. Zero is "·"; anything else gets a level
// relative to peak, so the two series share a scale.
func spark(vs []int64, peak int64, width int) string {
	if len(vs) == 0 || width < 1 {
		return ""
	}
	group := (len(vs) + width - 1) / width
	if group > 1 {
		peak *= int64(group) // sums of group hours
	}
	var b strings.Builder
	for i := 0; i < len(vs); i += group {
		var sum int64
		for _, v := range vs[i:min(i+group, len(vs))] {
			sum += v
		}
		switch {
		case sum <= 0 || peak <= 0:
			b.WriteRune('·')
		default:
			lvl := int(sum * int64(len(sparkLevels)-1) / peak)
			b.WriteRune(sparkLevels[min(lvl, len(sparkLevels)-1)])
		}
	}
	return b.String()
}

func footer(baseline time.Duration) string {
	return fmt.Sprintf("Saved = cache hits + 304 revalidations + reads joined in flight + "+
		"(polls of a %s sleep loop during waits − polls made). Each counts as 1 REST call; "+
		"real = REST calls that spent quota. Set the baseline with %s.", baseline, BaselineEnv)
}

func sinceLabel(d time.Duration) string {
	if d >= 48*time.Hour && d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	}
	return fmt.Sprintf("%dh", (d+time.Hour-1)/time.Hour)
}

func until(t, at time.Time) string {
	d := t.Sub(at).Round(time.Minute)
	switch {
	case d <= 0:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return fmt.Sprintf("%dh%02dm", d/time.Hour, (d%time.Hour)/time.Minute)
	}
}

func pct(n, total int64) int64 {
	if total <= 0 {
		return 0
	}
	return n * 100 / total
}

func maxOf(vs ...int64) int64 {
	var m int64
	for _, v := range vs {
		m = max(m, v)
	}
	return m
}

func pad(s string) string { return fmt.Sprintf("%-*s", labelW, s) }

// fit truncates plain text to width runes.
func fit(s string, width int) string {
	if utf8.RuneCountInString(s) <= width {
		return s
	}
	return string([]rune(s)[:width-1]) + "…"
}

// wrap splits plain text into lines of at most width runes.
func wrap(s string, width int) []string {
	var lines []string
	cur := ""
	for _, word := range strings.Fields(s) {
		switch {
		case cur == "":
			cur = fit(word, width)
		case utf8.RuneCountInString(cur)+1+utf8.RuneCountInString(word) <= width:
			cur += " " + word
		default:
			lines = append(lines, cur)
			cur = fit(word, width)
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}
