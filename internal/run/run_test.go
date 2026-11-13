package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/SheaHawkins/sheepdog/internal/node"
	"github.com/SheaHawkins/sheepdog/internal/script"
)

// fakeExec answers each prompt with "r(<prompt>)". block, when set, makes
// the call with that prompt prefix hang until its context ends, as a call in
// flight when the supervisor dies does.
type fakeExec struct {
	mu      sync.Mutex
	kinds   map[string]string
	live    []string
	block   string
	started chan struct{}
}

func (f *fakeExec) Do(ctx context.Context, c node.Call) (any, error) {
	f.mu.Lock()
	if f.kinds == nil {
		f.kinds = map[string]string{}
	}
	if _, ok := f.kinds[c.Label]; !ok {
		if c.Kind == "" {
			f.mu.Unlock()
			return nil, fmt.Errorf("label %q appears for the first time without a kind", c.Label)
		}
		f.kinds[c.Label] = c.Kind
	}
	f.live = append(f.live, c.Prompt)
	f.mu.Unlock()
	if f.block != "" && strings.HasPrefix(c.Prompt, f.block) {
		close(f.started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return "r(" + c.Prompt + ")", nil
}

func (f *fakeExec) Learn(label, kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.kinds == nil {
		f.kinds = map[string]string{}
	}
	f.kinds[label] = kind
}

func (f *fakeExec) KindOf(label string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kinds[label]
}

func compileFile(path string) (*script.Program, error) { return script.Compile(path) }

func compile(t *testing.T, name string) *script.Program {
	t.Helper()
	p, err := compileFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// crash runs the graph until the call starting with block is in flight, then
// kills the supervisor's context, as a crash or `run stop` would.
func crash(t *testing.T, dir string, p *script.Program, block string) *fakeExec {
	t.Helper()
	f := &fakeExec{block: block, started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := Execute(ctx, Config{Dir: dir, ID: "r1", Program: p, Executor: f})
		done <- err
	}()
	<-f.started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("crashed run returned %v", err)
	}
	return f
}

func TestResumeReplaysJournal(t *testing.T) {
	dir := t.TempDir()
	p := compile(t, "three.ts")
	crash(t, dir, p, "two")

	f := &fakeExec{}
	got, err := Execute(context.Background(), Config{Dir: dir, ID: "r1", Program: p, Executor: f})
	if err != nil {
		t.Fatal(err)
	}
	want := []any{"r(one)", "r(two after r(one))", "r(three after r(two after r(one)))"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	// Call 1 came from the journal; calls 2 and 3 ran live. Call 2 has no
	// kind, so the executor must have learned it from the journal.
	if !reflect.DeepEqual(f.live, []string{"two after r(one)", "three after r(two after r(one))"}) {
		t.Errorf("live calls: %q", f.live)
	}
	rec := readRecord(t, dir)
	if rec.Status != Succeeded || rec.Attempts != 2 || rec.PID != 0 {
		t.Errorf("run.json: %+v", rec)
	}
}

func TestResumeParallel(t *testing.T) {
	dir := t.TempDir()
	p := compile(t, "fan.ts")
	// check-b hangs; check-a and check-c complete and are journaled, in
	// whatever order they finish.
	crash(t, dir, p, "check b")
	j, _ := OpenJournal(filepath.Join(dir, "journal.jsonl"))
	n := j.Len()
	j.Close()

	f := &fakeExec{}
	got, err := Execute(context.Background(), Config{Dir: dir, ID: "r1", Program: p, Executor: f})
	if err != nil {
		t.Fatal(err)
	}
	if got != "r(sum r(check a),r(check b),r(check c))" {
		t.Errorf("got %v", got)
	}
	if len(f.live) != 4-n || f.live[len(f.live)-1] != "sum r(check a),r(check b),r(check c)" {
		t.Errorf("%d journaled before the crash; live calls on resume: %q", n, f.live)
	}
}

func TestResumeMismatchFails(t *testing.T) {
	dir := t.TempDir()
	crash(t, dir, compile(t, "three.ts"), "two")
	// The graph changed: call 1's prompt differs. The script catches the
	// error, but the run must fail anyway.
	_, err := Execute(context.Background(), Config{Dir: dir, ID: "r1", Program: compile(t, "three-changed.ts"), Executor: &fakeExec{}})
	var m *MismatchError
	if !errors.As(err, &m) || m.Seq != 1 {
		t.Fatalf("err = %v", err)
	}
	if rec := readRecord(t, dir); rec.Status != Failed {
		t.Errorf("status %s", rec.Status)
	}
}

func TestJournalTornLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	j, _ := OpenJournal(path)
	j.Append(Entry{Seq: 1, Label: "a", Result: []byte(`"x"`)})
	j.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"seq":2,"label":"b","res`) // crash mid-write
	f.Close()

	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := j.Get(2); ok || j.Len() != 1 {
		t.Fatal("torn entry was kept")
	}
	if err := j.Append(Entry{Seq: 2, Label: "b", Result: []byte(`"y"`)}); err != nil {
		t.Fatal(err)
	}
	j.Close()
	if j, err = OpenJournal(path); err != nil || j.Len() != 2 {
		t.Fatalf("after repair: %v, %d entries", err, j.Len())
	}
	j.Close()
}

func TestJournalDamageInTheMiddle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	os.WriteFile(path, []byte("{\"seq\":1,\"result\":1}\nnot json\n{\"seq\":3,\"result\":1}\n"), 0o644)
	if _, err := OpenJournal(path); err == nil || !strings.Contains(err.Error(), ":2:") {
		t.Fatalf("err = %v", err)
	}
}

func TestLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supervisor.lock")

	// A live supervisor: our parent process stands in for one.
	os.WriteFile(path, []byte(fmt.Sprint(os.Getppid())), 0o644)
	if _, err := acquire(path); err == nil || !strings.Contains(err.Error(), "already supervised") {
		t.Fatalf("took a live lock: %v", err)
	}

	// A crashed one: a process that has exited.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skip("no `true` command")
	}
	os.WriteFile(path, []byte(fmt.Sprint(cmd.Process.Pid)), 0o644)
	l, err := acquire(path)
	if err != nil {
		t.Fatalf("did not take over a stale lock: %v", err)
	}
	l.release()
}

func readRecord(t *testing.T, dir string) Record {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r Record
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}
