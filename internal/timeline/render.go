package timeline

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/SheaHawkins/sheepdog/internal/herdr"
	"github.com/SheaHawkins/sheepdog/internal/run"
)

// View is what to draw besides the run itself.
type View struct {
	Width    int
	Now      time.Time
	Selected int                     // index into Run.Calls; -1 for none
	Live     map[string]herdr.Status // agent status by label, for running calls
	Color    bool
}

// Render draws the run as lines no wider than v.Width, and returns the line
// each call's first row is on, so a caller can scroll the selection into view.
func Render(r *Run, v View) (lines []string, rowOf map[int]int) {
	p := &painter{v: v, rowOf: map[int]int{}}
	p.header(r)
	p.add("")
	if len(r.Calls) == 0 {
		p.add("  " + p.dim("no agent calls yet"))
	}
	// One name column for the whole run, so kinds and durations line up.
	for _, c := range r.Calls {
		p.nameWidth = max(p.nameWidth, utf8.RuneCountInString(displayName(c)))
	}
	// Each log() line goes after the last group that started before it.
	logsAfter := map[int][]string{}
	for _, l := range r.Logs {
		at := -1
		for gi, g := range r.Groups {
			// The script logs before it makes its next call, so a group
			// starting at the same instant comes after the line.
			if s := r.Calls[g[0]].Started; !s.IsZero() && s.Before(l.Time) {
				at = gi
			}
		}
		logsAfter[at] = append(logsAfter[at], l.Text)
	}
	for _, t := range logsAfter[-1] {
		p.add(clipVisible("  "+p.dim("· "+oneLine(t)), v.Width))
	}
	prevSide := false
	for gi, g := range r.Groups {
		side := len(g) > 1 && p.fitsSideBySide(r, g)
		// A side-by-side group draws its own edges, which join the line.
		if gi > 0 && !side && !prevSide {
			p.add("  " + p.dim("│"))
		}
		if side {
			p.sideBySide(r, g)
		} else {
			p.stacked(r, g)
		}
		prevSide = side
		for _, t := range logsAfter[gi] {
			p.add(clipVisible("  "+p.dim("· "+oneLine(t)), v.Width))
			prevSide = false
		}
	}
	if r.Record.Error != "" {
		p.add("")
		p.add("  " + p.paint(red, "error: ") + clip(oneLine(r.Record.Error), v.Width-9))
	}
	return p.lines, p.rowOf
}

type painter struct {
	v         View
	nameWidth int
	lines     []string
	rowOf     map[int]int
}

func (p *painter) add(s string) { p.lines = append(p.lines, s) }

func (p *painter) header(r *Run) {
	name := r.Record.Graph
	if name == "" {
		name = baseName(r.Record.Script)
	}
	status := r.Record.Status
	icon, color := "●", yellow
	switch status {
	case run.Succeeded:
		icon, color = "✓", green
	case run.Failed:
		icon, color = "✗", red
	case run.Stopped:
		icon, color = "■", dimc
	case run.Blocked:
		icon, color = "!", red
	}
	elapsed := ""
	if !r.Record.Started.IsZero() {
		end := p.v.Now
		if status != run.Running && status != run.Blocked {
			end = r.Record.Updated
		}
		elapsed = "  " + dur(end.Sub(r.Record.Started))
	}
	p.add(clipVisible(" "+p.bold(name)+"  "+p.dim("run "+shortID(r.ID))+"  "+p.paint(color, icon+" "+status)+elapsed, p.v.Width))
}

// line is one call drawn on one row: icon, name, then details.
func (p *painter) callLine(r *Run, i int, nameWidth int) string {
	c := r.Calls[i]
	icon, details := p.status(c)
	name := displayName(c)
	pad := strings.Repeat(" ", max(0, nameWidth-utf8.RuneCountInString(name)))
	s := icon + " " + name + pad + "  " + p.dim(fmt.Sprintf("%-6s", c.Kind)) + "  " + details
	return s
}

// status returns a call's icon and what to say after its kind.
func (p *painter) status(c Call) (icon, details string) {
	switch c.State {
	case Done:
		d := ""
		if !c.Started.IsZero() && !c.Ended.IsZero() {
			d = fmt.Sprintf("%6s", dur(c.Ended.Sub(c.Started)))
		}
		return p.paint(green, "✓"), d + "  " + summary(c)
	case Failed:
		return p.paint(red, "✗"), p.paint(red, oneLine(c.Error))
	}
	since := ""
	if !c.Started.IsZero() {
		since = " " + dur(p.v.Now.Sub(c.Started))
	}
	switch p.v.Live[c.Label] {
	case herdr.Blocked:
		return p.paint(red, "!"), p.paint(red, "waiting on you"+since)
	case herdr.Unknown:
		// herdr cannot read an agent's screen while it starts up; only
		// later does unknown mean something needs a person.
		if !c.Started.IsZero() && p.v.Now.Sub(c.Started) < 30*time.Second {
			return p.paint(yellow, "◐"), p.paint(yellow, "starting"+since)
		}
		return p.paint(red, "?"), p.paint(red, "needs a look"+since)
	}
	return p.paint(yellow, "◐"), p.paint(yellow, "working"+since)
}

func (p *painter) stacked(r *Run, g []int) {
	w := p.nameWidth
	if len(g) > 1 {
		w -= 4 // the parallel bracket takes four columns
	}
	for k, i := range g {
		prefix := "  "
		if len(g) > 1 {
			switch {
			case k == 0:
				p.add("  " + p.dim(fmt.Sprintf("├─┬─ parallel ×%d", len(g))))
				prefix = "  " + p.dim("│ ├ ")
			case k == len(g)-1:
				prefix = "  " + p.dim("│ └ ")
			default:
				prefix = "  " + p.dim("│ ├ ")
			}
		}
		p.rowOf[i] = len(p.lines)
		p.add(p.select_(i, clipVisible(prefix+p.callLine(r, i, w), p.v.Width)))
	}
}

const colGap = 3

func (p *painter) colWidth(r *Run, g []int) int {
	w := 0
	for _, i := range g {
		c := r.Calls[i]
		w = max(w, 2+utf8.RuneCountInString(displayName(c)), 2+visibleLen(p.cellDetails(c)))
	}
	return min(w, 28)
}

// cellDetails is the second line of a side-by-side cell: the result summary
// once done (or the duration, without one), else the live status.
func (p *painter) cellDetails(c Call) string {
	_, details := p.status(c)
	if c.State == Done {
		details = summary(c)
		if details == "" && !c.Started.IsZero() && !c.Ended.IsZero() {
			details = dur(c.Ended.Sub(c.Started))
		}
	}
	return details
}

func (p *painter) fitsSideBySide(r *Run, g []int) bool {
	return 2+len(g)*(p.colWidth(r, g)+colGap) <= p.v.Width
}

// sideBySide draws a parallel group as columns:
//
//	├──────────────┐
//	✓ validate-F1  ✓ validate-F2
//	│ real         │ not real
//	├──────────────┘
func (p *painter) sideBySide(r *Run, g []int) {
	w := p.colWidth(r, g) + colGap
	edge := func(first, mid, last string) string {
		var b strings.Builder
		for k := range g {
			switch {
			case k == 0:
				b.WriteString(first)
			case k == len(g)-1:
				b.WriteString(last)
			default:
				b.WriteString(mid)
			}
			if k < len(g)-1 {
				b.WriteString(strings.Repeat("─", w-1))
			}
		}
		return "  " + p.dim(b.String())
	}
	p.add(edge("├", "┬", "┐"))
	var top, bottom strings.Builder
	top.WriteString("  ")
	bottom.WriteString("  ")
	row := len(p.lines)
	for _, i := range g {
		c := r.Calls[i]
		icon, _ := p.status(c)
		details := p.cellDetails(c)
		cell := icon + " " + clip(displayName(c), w-colGap-2)
		if i == p.v.Selected {
			cell = p.reverse(cell)
		}
		top.WriteString(padVisible(cell, w))
		bottom.WriteString(padVisible(p.dim("│ ")+clipVisible(details, w-colGap-2), w))
		p.rowOf[i] = row
	}
	p.add(strings.TrimRight(top.String(), " "))
	p.add(strings.TrimRight(bottom.String(), " "))
	p.add(edge("├", "┴", "┘"))
}

func (p *painter) select_(i int, s string) string {
	if i == p.v.Selected {
		return p.reverse(s)
	}
	return s
}

// displayName is the label, with the call's number from the second call on.
func displayName(c Call) string {
	if c.Nth > 1 {
		return fmt.Sprintf("%s #%d", c.Label, c.Nth)
	}
	return c.Label
}

// summary describes a call's result in a few words: the first line of text,
// or an object's scalar fields and array lengths. Fields that only repeat the
// label, such as id F1 for validate-F1, are left out.
func summary(c Call) string {
	switch x := c.Result.(type) {
	case nil:
		return ""
	case string:
		return clip(oneLine(x), 48)
	case bool:
		return fmt.Sprint(x)
	case float64:
		return fmt.Sprint(x)
	case []any:
		return fmt.Sprintf("%d items", len(x))
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var parts []string
		for _, k := range keys {
			switch y := x[k].(type) {
			case []any:
				parts = append(parts, fmt.Sprintf("%d %s", len(y), k))
			case map[string]any:
			case string:
				if y != "" && strings.Contains(strings.ToLower(c.Label), strings.ToLower(y)) {
					continue
				}
				parts = append(parts, k+": "+clip(oneLine(y), 24))
			default:
				parts = append(parts, k+": "+clip(oneLine(fmt.Sprint(y)), 24))
			}
		}
		return clip(strings.Join(parts, " · "), 48)
	}
	return ""
}

func dur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func baseName(path string) string {
	i := strings.LastIndexAny(path, `/\`)
	return strings.TrimSuffix(path[i+1:], ".ts")
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return s
}

func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// ANSI styling. With Color off every helper returns its text unchanged.

const (
	red    = "31"
	green  = "32"
	yellow = "33"
	dimc   = "2"
)

func (p *painter) paint(code, s string) string {
	if !p.v.Color || s == "" {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
func (p *painter) dim(s string) string  { return p.paint(dimc, s) }
func (p *painter) bold(s string) string { return p.paint("1", s) }
func (p *painter) reverse(s string) string {
	if !p.v.Color {
		return "› " + strings.TrimLeft(s, " ")
	}
	// Re-apply reverse after every reset inside s, so colored parts stay
	// highlighted.
	return "\x1b[7m" + strings.ReplaceAll(s, "\x1b[0m", "\x1b[0m\x1b[7m") + "\x1b[0m"
}

// visibleLen counts runes outside ANSI escape sequences.
func visibleLen(s string) int {
	n, esc := 0, false
	for _, r := range s {
		switch {
		case esc:
			if r == 'm' {
				esc = false
			}
		case r == '\x1b':
			esc = true
		default:
			n++
		}
	}
	return n
}

func padVisible(s string, w int) string {
	return s + strings.Repeat(" ", max(0, w-visibleLen(s)))
}

// clipVisible cuts s to n visible runes, keeping escape sequences intact.
func clipVisible(s string, n int) string {
	if visibleLen(s) <= n {
		return s
	}
	var b strings.Builder
	seen, esc := 0, false
loop:
	for _, r := range s {
		switch {
		case esc:
			if r == 'm' {
				esc = false
			}
		case r == '\x1b':
			esc = true
		case seen == n-1:
			b.WriteString("…")
			break loop
		default:
			seen++
		}
		b.WriteRune(r)
	}
	if strings.ContainsRune(s, '\x1b') {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// Clip cuts a line, which may contain ANSI colors, to n visible columns.
func Clip(s string, n int) string { return clipVisible(s, n) }
