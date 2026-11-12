package run

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// Entry is one completed agent() call.
type Entry struct {
	Seq       int             `json:"seq"`
	ExecID    string          `json:"exec_id"`
	Label     string          `json:"label"`
	Kind      string          `json:"kind"`
	PromptSHA string          `json:"prompt_sha256"`
	Result    json.RawMessage `json:"result"`
}

// Journal is the append-only record of a run's completed calls. Each entry
// is one line, synced to disk before the script sees the result.
type Journal struct {
	mu      sync.Mutex
	f       *os.File
	entries map[int]Entry
}

// OpenJournal opens or creates a journal. A last line cut short by a crash
// mid-write is dropped and truncated away; damage anywhere else is an error.
func OpenJournal(path string) (*Journal, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	j := &Journal{f: f, entries: map[int]Entry{}}
	r := bufio.NewReader(f)
	var good int64
	for line := 1; ; line++ {
		b, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			if len(bytes.TrimSpace(b)) > 0 {
				// No newline: the write that would have finished it never
				// happened, so the entry was never acknowledged.
				if err := f.Truncate(good); err != nil {
					f.Close()
					return nil, err
				}
			}
			break
		}
		if err != nil {
			f.Close()
			return nil, err
		}
		var e Entry
		if err := json.Unmarshal(b, &e); err != nil || e.Seq <= 0 {
			f.Close()
			return nil, fmt.Errorf("%s:%d: damaged journal entry", path, line)
		}
		if _, dup := j.entries[e.Seq]; dup {
			f.Close()
			return nil, fmt.Errorf("%s:%d: call %d recorded twice", path, line, e.Seq)
		}
		j.entries[e.Seq] = e
		good += int64(len(b))
	}
	if _, err := f.Seek(good, io.SeekStart); err != nil {
		f.Close()
		return nil, err
	}
	return j, nil
}

// Get returns the entry for a call, if it completed.
func (j *Journal) Get(seq int) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[seq]
	return e, ok
}

// Len is the number of completed calls.
func (j *Journal) Len() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.entries)
}

// Append records a completed call and syncs it to disk.
func (j *Journal) Append(e Entry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, dup := j.entries[e.Seq]; dup {
		return fmt.Errorf("call %d is already in the journal", e.Seq)
	}
	if _, err := j.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := j.f.Sync(); err != nil {
		return err
	}
	j.entries[e.Seq] = e
	return nil
}

func (j *Journal) Close() error { return j.f.Close() }
