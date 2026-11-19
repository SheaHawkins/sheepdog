package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/SheaHawkins/sheepdog/internal/herdr"
	"github.com/SheaHawkins/sheepdog/internal/node"
	"github.com/SheaHawkins/sheepdog/internal/run"
	"github.com/SheaHawkins/sheepdog/internal/timeline"
)

// watcher is the run timeline pane: `sheepdog plugin watch` in herdr, and
// `sheepdog run watch` in any terminal.
type watcher struct {
	project string
	h       herdr.Client
	runs    []string // newest first
	cur     int      // index into runs
	pinned  bool     // the user picked a run; don't jump to newer ones
	sel     int      // selected call; -1 follows the latest
	scroll  int
	status  string // one-line message under the timeline
}

func watch(ctx context.Context, project, runID string, h herdr.Client, in *os.File, out io.Writer) error {
	w := &watcher{project: project, h: h, sel: -1}
	if err := w.refreshRuns(); err != nil {
		return err
	}
	if runID != "" {
		w.cur, w.pinned = -1, true
		for i, id := range w.runs {
			if id == runID {
				w.cur = i
			}
		}
		if w.cur < 0 {
			return fmt.Errorf("no run %s in %s", runID, project)
		}
	}

	f, isFile := out.(*os.File)
	if !isFile || !term.IsTerminal(int(f.Fd())) || !term.IsTerminal(int(in.Fd())) {
		// Not interactive: draw once, plain.
		lines := w.frame(ctx, 100, 0, false)
		_, err := fmt.Fprintln(out, strings.Join(lines, "\n"))
		return err
	}

	old, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(in.Fd()), old)
	fmt.Fprint(out, "\x1b[?1049h\x1b[?25l") // alternate screen, hide cursor
	defer fmt.Fprint(out, "\x1b[?25h\x1b[?1049l")

	keys := make(chan string, 16)
	go readKeys(in, keys)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		width, height, err := term.GetSize(int(f.Fd()))
		if err != nil || width < 20 || height < 5 {
			width, height = 80, 24
		}
		lines := w.frame(ctx, width, height, true)
		var b strings.Builder
		b.WriteString("\x1b[H")
		for i, l := range lines {
			// A line wider than the pane would wrap and scroll the screen.
			b.WriteString(timeline.Clip(l, width))
			b.WriteString("\x1b[K")
			if i < len(lines)-1 {
				b.WriteString("\r\n")
			}
		}
		b.WriteString("\x1b[J")
		io.WriteString(out, b.String())

		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		case k, ok := <-keys:
			if !ok || w.key(ctx, k) {
				return nil
			}
		}
	}
}

// frame draws the whole screen: the timeline, scrolled so the selection is
// visible, and a key line at the bottom.
func (w *watcher) frame(ctx context.Context, width, height int, color bool) []string {
	// New runs appear while the pane is open; the pane moves to them.
	w.refreshRuns()
	if len(w.runs) == 0 {
		return []string{
			" sheepdog",
			"",
			"  No runs in " + w.project + " yet.",
			"  Start one with: sheepdog graph run FILE.ts \"instruction\"",
			"",
			"  This pane updates when one starts.  q quit",
		}
	}
	r, err := timeline.Load(filepath.Join(w.project, ".sheepdog", "runs", w.runs[w.cur]))
	if err != nil {
		return []string{" sheepdog", "", "  " + err.Error()}
	}
	sel := w.sel
	if sel < 0 || sel >= len(r.Calls) {
		sel = len(r.Calls) - 1
	}
	lines, rowOf := timeline.Render(r, timeline.View{
		Width: width, Now: time.Now(), Selected: sel, Live: w.live(ctx, r), Color: color,
	})
	if height == 0 {
		return lines
	}

	footer := w.footer(r, width, color)
	body := height - len(footer)
	// Keep the header (first two lines) fixed and scroll the rest.
	if len(lines) > body && body > 2 {
		head, rest := lines[:2], lines[2:]
		room := body - 2
		if row, ok := rowOf[sel]; ok {
			row -= 2
			if row < w.scroll {
				w.scroll = row
			}
			if row+2 > w.scroll+room { // keep a parallel cell's second line too
				w.scroll = row + 2 - room
			}
		}
		w.scroll = max(0, min(w.scroll, len(rest)-room))
		lines = append(append([]string{}, head...), rest[w.scroll:w.scroll+room]...)
	}
	for len(lines) < body {
		lines = append(lines, "")
	}
	return append(lines, footer...)
}

func (w *watcher) footer(r *timeline.Run, width int, color bool) []string {
	keys := "↑↓ select · enter go to agent · ←→ runs · q quit"
	pos := fmt.Sprintf("run %d of %d", len(w.runs)-w.cur, len(w.runs))
	line := " " + keys
	if pad := width - len([]rune(line)) - len(pos) - 1; pad > 1 {
		line += strings.Repeat(" ", pad) + pos
	}
	msg := " " + w.status
	if color {
		line = "\x1b[2m" + line + "\x1b[0m"
	}
	return []string{msg, line}
}

// live asks herdr for the status of each agent with a call in flight.
func (w *watcher) live(ctx context.Context, r *timeline.Run) map[string]herdr.Status {
	m := map[string]herdr.Status{}
	if r.Record.Status != run.Running && r.Record.Status != run.Blocked {
		return m
	}
	for _, c := range r.Calls {
		if c.State != timeline.Running {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		a, err := w.h.GetAgent(cctx, node.AgentName(r.ID, c.Label))
		cancel()
		if err == nil {
			m[c.Label] = a.Status
		}
	}
	return m
}

// key handles one key press and reports whether to quit.
func (w *watcher) key(ctx context.Context, k string) bool {
	w.status = ""
	r, _ := w.current()
	n := 0
	if r != nil {
		n = len(r.Calls)
	}
	sel := w.sel
	if sel < 0 {
		sel = n - 1
	}
	switch k {
	case "q", "\x03", "\x1b":
		return true
	case "up", "k":
		w.sel = max(0, sel-1)
	case "down", "j":
		if sel+1 >= n-1 {
			w.sel = -1 // back at the bottom: follow new calls again
		} else {
			w.sel = sel + 1
		}
	case "left", "[":
		if w.cur+1 < len(w.runs) {
			w.cur, w.sel, w.scroll, w.pinned = w.cur+1, -1, 0, true
		}
	case "right", "]":
		if w.cur > 0 {
			w.cur, w.sel, w.scroll = w.cur-1, -1, 0
			w.pinned = w.cur != 0
		}
	case "enter":
		if r == nil || sel < 0 || sel >= n {
			break
		}
		name := node.AgentName(r.ID, r.Calls[sel].Label)
		cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if err := focusAgent(cctx, w.h, name); err != nil {
			w.status = "could not focus " + r.Calls[sel].Label + ": " + err.Error()
		}
	}
	return false
}

func (w *watcher) current() (*timeline.Run, error) {
	if len(w.runs) == 0 {
		return nil, nil
	}
	return timeline.Load(filepath.Join(w.project, ".sheepdog", "runs", w.runs[w.cur]))
}

// refreshRuns reloads the run list. Unless the user picked a run, the pane
// moves to the newest one, so starting a run brings it up.
func (w *watcher) refreshRuns() error {
	ids, err := timeline.Runs(w.project)
	if err != nil {
		return err
	}
	if len(w.runs) > 0 && w.pinned {
		id := w.runs[w.cur]
		for i, x := range ids {
			if x == id {
				w.cur = i
			}
		}
	} else {
		if len(ids) > 0 && (len(w.runs) == 0 || ids[0] != w.runs[0]) {
			w.sel, w.scroll = -1, 0
		}
		w.cur = 0
	}
	w.runs = ids
	return nil
}

func focusAgent(ctx context.Context, h herdr.Client, name string) error {
	c, ok := h.(*herdr.CLI)
	if !ok {
		return fmt.Errorf("not supported")
	}
	return c.Focus(ctx, name)
}

// readKeys turns raw terminal input into key names.
func readKeys(in io.Reader, keys chan<- string) {
	defer close(keys)
	buf := make([]byte, 64)
	for {
		n, err := in.Read(buf)
		if err != nil {
			return
		}
		s := string(buf[:n])
		for len(s) > 0 {
			switch {
			case strings.HasPrefix(s, "\x1b[A"), strings.HasPrefix(s, "\x1bOA"):
				keys <- "up"
				s = s[3:]
			case strings.HasPrefix(s, "\x1b[B"), strings.HasPrefix(s, "\x1bOB"):
				keys <- "down"
				s = s[3:]
			case strings.HasPrefix(s, "\x1b[C"), strings.HasPrefix(s, "\x1bOC"):
				keys <- "right"
				s = s[3:]
			case strings.HasPrefix(s, "\x1b[D"), strings.HasPrefix(s, "\x1bOD"):
				keys <- "left"
				s = s[3:]
			case strings.HasPrefix(s, "\x1b["):
				s = "" // another sequence; ignore the rest of this read
			case s[0] == '\r' || s[0] == '\n':
				keys <- "enter"
				s = s[1:]
			default:
				keys <- s[:1]
				s = s[1:]
			}
		}
	}
}
