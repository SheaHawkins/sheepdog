package timeline

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SheaHawkins/sheepdog/internal/herdr"
	"github.com/SheaHawkins/sheepdog/internal/run"
)

var update = flag.Bool("update", false, "rewrite golden files")

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func at(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

// fixture writes a run directory: secure-implement from SPEC.md, in round 2,
// with its second security review still working.
func fixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "mwhbcbewdjcp5s")
	os.MkdirAll(dir, 0o755)
	write := func(name string, v any) {
		b, _ := json.MarshalIndent(v, "", "  ")
		os.WriteFile(filepath.Join(dir, name), b, 0o644)
	}
	write("run.json", run.Record{
		ID: "mwhbcbewdjcp5s", Graph: "secure-implement", Status: run.Running,
		Script: "/p/secure-implement.ts", Started: at(0), Updated: at(200), Attempts: 1,
	})
	var ev, jr []string
	line := func(dst *[]string, v any) {
		b, _ := json.Marshal(v)
		*dst = append(*dst, string(b))
	}
	call := func(seq int, label, kind string, start, end int, result any) {
		id := "e" + string(rune('0'+seq))
		line(&ev, run.Event{Time: at(start), Type: run.CallStarted, Seq: seq, Label: label, Kind: kind, ExecID: id})
		if end < 0 {
			return
		}
		line(&ev, run.Event{Time: at(end), Type: run.CallDone, Seq: seq, Label: label, ExecID: id})
		raw, _ := json.Marshal(result)
		line(&jr, run.Entry{Seq: seq, ExecID: id, Label: label, Kind: kind, Result: raw})
	}
	call(1, "implement", "claude", 0, 100, "Added rate limiting to /login.\nDetails follow.")
	call(2, "security", "codex", 100, 152, map[string]any{"findings": []any{map[string]any{"id": "F1"}, map[string]any{"id": "F2"}}})
	call(3, "validate-F1", "claude", 152, 172, map[string]any{"id": "F1", "isReal": true})
	call(4, "validate-F2", "claude", 152, 177, map[string]any{"id": "F2", "isReal": false})
	line(&ev, run.Event{Time: at(177), Type: run.Log, Message: "log: round 1: 1 confirmed"})
	call(5, "implement", "", 177, 242, "Fixed F1.")
	call(6, "security", "", 242, -1, nil)
	os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(strings.Join(ev, "\n")+"\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "journal.jsonl"), []byte(strings.Join(jr, "\n")+"\n"), 0o644)
	return dir
}

func TestLoadGroupsParallelCalls(t *testing.T) {
	r, err := Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	want := [][]int{{0}, {1}, {2, 3}, {4}, {5}}
	if got := r.Groups; len(got) != len(want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
	for i := range want {
		if len(r.Groups[i]) != len(want[i]) || r.Groups[i][0] != want[i][0] {
			t.Fatalf("groups = %v, want %v", r.Groups, want)
		}
	}
	if c := r.Calls[4]; c.Nth != 2 || c.Kind != "claude" {
		t.Errorf("second implement call: nth %d kind %q", c.Nth, c.Kind)
	}
	if c := r.Calls[5]; c.State != Running {
		t.Errorf("last call state %s", c.State)
	}
}

func TestRenderGolden(t *testing.T) {
	r, err := Load(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		width int
		live  herdr.Status
		now   int
	}{
		{"wide", 80, herdr.Working, 273},
		{"narrow", 40, herdr.Working, 273},
		{"blocked", 80, herdr.Blocked, 273},
		{"starting", 60, herdr.Unknown, 250},
		{"unknown", 60, herdr.Unknown, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, _ := Render(r, View{
				Width: tc.width, Now: at(tc.now), Selected: 5,
				Live: map[string]herdr.Status{"security": tc.live},
			})
			got := strings.Join(lines, "\n") + "\n"
			path := filepath.Join("testdata", tc.name+".txt")
			if *update {
				os.MkdirAll("testdata", 0o755)
				os.WriteFile(path, []byte(got), 0o644)
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			// Windows checkouts may turn the golden files' LF into CRLF.
			if got != strings.ReplaceAll(string(want), "\r\n", "\n") {
				t.Errorf("render differs from %s:\n%s", path, got)
			}
			for i, l := range lines {
				if n := visibleLen(l); n > tc.width {
					t.Errorf("line %d is %d wide, over %d: %q", i, n, tc.width, l)
				}
			}
		})
	}
}

func TestInterruptedCallShowsFailed(t *testing.T) {
	dir := fixture(t)
	b, _ := os.ReadFile(filepath.Join(dir, "run.json"))
	var rec run.Record
	json.Unmarshal(b, &rec)
	rec.Status = run.Failed
	b, _ = json.Marshal(rec)
	os.WriteFile(filepath.Join(dir, "run.json"), b, 0o644)
	r, _ := Load(dir)
	if c := r.Calls[5]; c.State != Failed || c.Error != "interrupted" {
		t.Errorf("call left running by a finished run: %s %q", c.State, c.Error)
	}
}
