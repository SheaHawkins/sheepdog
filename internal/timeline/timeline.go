// Package timeline turns a run's files into the run timeline that sheepdog's
// herdr pane draws: every agent() call in order, parallel() calls side by
// side, each loop round below the last.
//
// It only reads the run directory (run.json, journal.jsonl, events.jsonl), so
// it is safe to call while a supervisor is writing.
package timeline

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/SheaHawkins/sheepdog/internal/run"
)

// State is a call's state as the timeline shows it.
type State string

const (
	Running State = "running" // sent, not finished
	Done    State = "done"
	Failed  State = "failed"
)

// Call is one agent() call.
type Call struct {
	Seq     int
	Label   string
	Kind    string
	ExecID  string
	Nth     int // 1 for the label's first call, 2 for its second, ...
	State   State
	Started time.Time // zero if unknown
	Ended   time.Time // zero while running, or if unknown
	Result  any       // from the journal, once done
	Error   string    // the last failure, while not done
}

// Run is one run as the timeline shows it.
type Run struct {
	ID     string
	Dir    string
	Record run.Record
	Calls  []Call // in call order
	Groups [][]int
	Logs   []Log // the script's log() lines, in order
}

// Log is one log() line from the script.
type Log struct {
	Time time.Time
	Text string
}

// Load reads the run in dir.
func Load(dir string) (*Run, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return nil, err
	}
	r := &Run{ID: filepath.Base(dir), Dir: dir}
	if err := json.Unmarshal(raw, &r.Record); err != nil {
		return nil, err
	}
	calls := map[int]*Call{}
	get := func(seq int) *Call {
		c := calls[seq]
		if c == nil {
			c = &Call{Seq: seq}
			calls[seq] = c
		}
		return c
	}
	eachLine(filepath.Join(dir, "events.jsonl"), func(b []byte) {
		var e run.Event
		if json.Unmarshal(b, &e) != nil {
			return
		}
		if e.Type == run.Log {
			r.Logs = append(r.Logs, Log{Time: e.Time, Text: strings.TrimPrefix(e.Message, "log: ")})
			return
		}
		if e.Seq <= 0 {
			return
		}
		c := get(e.Seq)
		switch e.Type {
		case run.CallStarted:
			// A resume can start a call again; the latest attempt counts.
			c.Label, c.ExecID, c.State, c.Started, c.Ended, c.Error = e.Label, e.ExecID, Running, e.Time, time.Time{}, ""
			if e.Kind != "" {
				c.Kind = e.Kind
			}
		case run.CallDone:
			c.State, c.Ended = Done, e.Time
		case run.CallFailed:
			c.State, c.Ended, c.Error = Failed, e.Time, e.Error
		}
	})
	eachLine(filepath.Join(dir, "journal.jsonl"), func(b []byte) {
		var e run.Entry
		if json.Unmarshal(b, &e) != nil || e.Seq <= 0 {
			return
		}
		c := get(e.Seq)
		c.Label, c.ExecID, c.State, c.Error = e.Label, e.ExecID, Done, ""
		if e.Kind != "" {
			c.Kind = e.Kind
		}
		json.Unmarshal(e.Result, &c.Result)
	})

	seqs := make([]int, 0, len(calls))
	for s := range calls {
		seqs = append(seqs, s)
	}
	sort.Ints(seqs)
	nth, kinds := map[string]int{}, map[string]string{}
	for _, s := range seqs {
		c := calls[s]
		if c.Label == "" {
			continue // a line torn mid-write
		}
		nth[c.Label]++
		c.Nth = nth[c.Label]
		if c.Kind == "" {
			c.Kind = kinds[c.Label] // only a label's first call names its kind
		}
		kinds[c.Label] = c.Kind
		// A call still marked running in a run whose supervisor has
		// finished was cut off; show it as failed, not spinning forever.
		if c.State == Running && r.Record.Status != run.Running && r.Record.Status != run.Blocked {
			c.State = Failed
			if c.Error == "" {
				c.Error = "interrupted"
			}
		}
		r.Calls = append(r.Calls, *c)
	}
	r.Groups = group(r.Calls)
	return r, nil
}

// group splits calls into rows. Calls whose runs overlapped in time were
// made by one parallel(); anything else stands alone. Positions are in call
// order, so a group is always a consecutive range.
func group(calls []Call) [][]int {
	var groups [][]int
	var end time.Time // latest end in the current group; zero means still running
	open := false
	for i, c := range calls {
		overlaps := len(groups) > 0 && !c.Started.IsZero() && (open || c.Started.Before(end))
		if overlaps {
			groups[len(groups)-1] = append(groups[len(groups)-1], i)
		} else {
			groups = append(groups, []int{i})
			end, open = time.Time{}, false
		}
		switch {
		case c.State == Running || c.Ended.IsZero() && !c.Started.IsZero():
			open = true
		case c.Ended.After(end):
			end = c.Ended
		}
	}
	return groups
}

// Runs lists the runs in a project's .sheepdog/runs, newest first. Run IDs
// start with the start time, so name order is start order.
func Runs(project string) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(project, ".sheepdog", "runs"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range ents {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	return ids, nil
}

func eachLine(path string, fn func([]byte)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		fn(sc.Bytes())
	}
}
