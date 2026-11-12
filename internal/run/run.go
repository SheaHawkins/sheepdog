// Package run is the supervisor: it executes one run's graph script, journals
// every completed agent() call, and resumes a run by replaying its script
// against the journal.
package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SheaHawkins/sheepdog/internal/node"
	"github.com/SheaHawkins/sheepdog/internal/script"
)

// Statuses a run can have.
const (
	Running   = "running"
	Blocked   = "blocked"
	Succeeded = "succeeded"
	Failed    = "failed"
	Stopped   = "stopped"
)

// Record is run.json.
type Record struct {
	ID       string         `json:"id"`
	Status   string         `json:"status"`
	Script   string         `json:"script"`
	Args     map[string]any `json:"args"`
	Result   any            `json:"result,omitempty"`
	Error    string         `json:"error,omitempty"`
	PID      int            `json:"supervisor_pid,omitempty"`
	Attempts int            `json:"attempts"` // supervisors that have driven this run
	Updated  time.Time      `json:"updated"`
}

// Executor runs live calls. *node.Executor implements it.
type Executor interface {
	Do(ctx context.Context, c node.Call) (any, error)
	Learn(label, kind string)
	KindOf(label string) string
}

// Config describes one run. The same Config, with the same Dir, starts a run
// and resumes it.
type Config struct {
	Dir     string // .sheepdog/runs/<run-id>
	ID      string
	Program *script.Program
	Args    map[string]any
	// Executor runs live calls. Its logs should go to Logf below.
	Executor Executor
}

// MismatchError means the script made a different call from the one the
// journal recorded at that position, so the run cannot be resumed safely.
type MismatchError struct {
	Seq  int
	Want Entry
	Got  string
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("resume: call %d does not match the journal (recorded %q, exec %s; the script now makes %s). The graph or its inputs changed since the run started",
		e.Seq, e.Want.Label, e.Want.ExecID, e.Got)
}

// Execute runs, or resumes, the run in cfg.Dir and returns what the script
// returns. Calls already in the journal return their recorded results at once.
func Execute(ctx context.Context, cfg Config) (any, error) {
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		return nil, err
	}
	l, err := acquire(filepath.Join(cfg.Dir, "supervisor.lock"))
	if err != nil {
		return nil, err
	}
	defer l.release()

	j, err := OpenJournal(filepath.Join(cfg.Dir, "journal.jsonl"))
	if err != nil {
		return nil, err
	}
	defer j.Close()
	ev, err := openEvents(filepath.Join(cfg.Dir, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	defer ev.close()

	rec := Record{ID: cfg.ID, Script: cfg.Program.Path, Args: cfg.Args}
	if raw, err := os.ReadFile(filepath.Join(cfg.Dir, "run.json")); err == nil {
		if err := json.Unmarshal(raw, &rec); err != nil {
			return nil, fmt.Errorf("run.json: %w", err)
		}
	}
	rec.Attempts++
	rec.Status, rec.PID, rec.Error, rec.Result = Running, os.Getpid(), "", nil
	if err := writeRecord(cfg.Dir, &rec); err != nil {
		return nil, err
	}
	if rec.Attempts > 1 {
		ev.log("resuming (supervisor %d); %d call(s) in the journal", rec.Attempts, j.Len())
	} else {
		ev.log("started")
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	h := &host{ctx: ctx, cancel: cancel, cfg: cfg, journal: j, ev: ev}
	result, err := script.Run(ctx, cfg.Program, cfg.Args, h)

	var mismatch *MismatchError
	switch cause := context.Cause(ctx); {
	case errors.As(cause, &mismatch):
		err = cause
		rec.Status = Failed
	case err == nil:
		rec.Status, rec.Result = Succeeded, result
	case ctx.Err() != nil:
		rec.Status = Stopped
	default:
		rec.Status = Failed
	}
	if err != nil {
		rec.Error = err.Error()
	}
	rec.PID = 0
	ev.log("%s", rec.Status)
	if werr := writeRecord(cfg.Dir, &rec); werr != nil && err == nil {
		err = werr
	}
	return result, err
}

// host is the script's view of the supervisor.
type host struct {
	ctx     context.Context
	cancel  context.CancelCauseFunc
	cfg     Config
	journal *Journal
	ev      *events
}

func (h *host) Log(msg string) { h.ev.log("log: %s", msg) }

func (h *host) Agent(ctx context.Context, prompt string, opts script.AgentOptions) (any, error) {
	sha := promptSHA(prompt)
	if e, ok := h.journal.Get(opts.Seq); ok {
		if e.Label != opts.Label || e.PromptSHA != sha {
			got := fmt.Sprintf("%q with a different prompt", opts.Label)
			if e.Label != opts.Label {
				got = fmt.Sprintf("%q", opts.Label)
			}
			err := &MismatchError{Seq: opts.Seq, Want: e, Got: got}
			h.cancel(err) // fatal even if the script catches it
			return nil, err
		}
		h.cfg.Executor.Learn(e.Label, e.Kind)
		var v any
		if err := json.Unmarshal(e.Result, &v); err != nil {
			return nil, err
		}
		return v, nil
	}

	execID := fmt.Sprintf("e%d", opts.Seq)
	var timeout time.Duration
	if opts.Timeout != "" {
		d, err := time.ParseDuration(opts.Timeout)
		if err != nil {
			return nil, fmt.Errorf("agent(): bad timeout %q", opts.Timeout)
		}
		timeout = d
	}
	h.ev.log("%s: call %d (%s)", opts.Label, opts.Seq, execID)
	v, err := h.cfg.Executor.Do(ctx, node.Call{
		ExecID: execID, Label: opts.Label, Kind: opts.Kind,
		Prompt: prompt, Schema: opts.Schema, Timeout: timeout,
	})
	if err != nil {
		// Failures are not journaled: a resume tries the call again.
		h.ev.log("%s: call %d failed: %v", opts.Label, opts.Seq, err)
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if err := h.journal.Append(Entry{
		Seq: opts.Seq, ExecID: execID, Label: opts.Label,
		Kind: h.cfg.Executor.KindOf(opts.Label), PromptSHA: sha, Result: raw,
	}); err != nil {
		// The script must not move on from a result that is not on disk.
		h.cancel(err)
		return nil, err
	}
	h.ev.log("%s: call %d done", opts.Label, opts.Seq)
	return v, nil
}

func promptSHA(p string) string {
	s := sha256.Sum256([]byte(p))
	return hex.EncodeToString(s[:])
}

// writeRecord writes run.json through a temp file and rename, so a crash
// never leaves it half-written.
func writeRecord(dir string, rec *Record) error {
	rec.Updated = time.Now().UTC()
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "run.json"), append(b, '\n'))
}

func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// events is events.jsonl, what `run watch` tails.
type events struct {
	mu sync.Mutex
	f  *os.File
}

func openEvents(path string) (*events, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &events{f: f}, nil
}

func (e *events) log(format string, args ...any) {
	b, _ := json.Marshal(struct {
		Time    time.Time `json:"time"`
		Message string    `json:"msg"`
	}{time.Now().UTC(), fmt.Sprintf(format, args...)})
	e.mu.Lock()
	defer e.mu.Unlock()
	e.f.Write(append(b, '\n'))
}

// Logf returns a function that writes to the run's event log, for the
// executor's progress lines. It opens the file per call; executors log rarely.
func Logf(dir string) func(string, ...any) {
	return func(format string, args ...any) {
		ev, err := openEvents(filepath.Join(dir, "events.jsonl"))
		if err != nil {
			return
		}
		defer ev.close()
		ev.log(format, args...)
	}
}

func (e *events) close() { e.f.Close() }
