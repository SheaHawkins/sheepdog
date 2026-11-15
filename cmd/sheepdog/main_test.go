package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// TestSmoke builds the real binary and a fake herdr, puts the fake first on
// the PATH, and runs a graph, a resume of the finished run, and a resume of a
// run whose last journal entries were lost. It runs on every OS in CI, so it
// covers process spawning, argument passing, paths in prompts, file renames
// and the run lock on each.
func TestSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("builds binaries")
	}
	bin := t.TempDir()
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	build(t, filepath.Join(bin, "sheepdog"+exe), ".")
	build(t, filepath.Join(bin, "herdr"+exe), "../../internal/herdr/fakeherdr")

	work := t.TempDir()
	state := filepath.Join(t.TempDir(), "herdr.json")
	graph, _ := filepath.Abs("testdata/smoke.ts")
	sheepdog := func(args ...string) (stdout, stderr string) {
		t.Helper()
		cmd := exec.Command(filepath.Join(bin, "sheepdog"+exe), args...)
		cmd.Dir = work
		cmd.Env = append(os.Environ(),
			"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"FAKEHERDR_STATE="+state)
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &e
		if err := cmd.Run(); err != nil {
			t.Fatalf("sheepdog %s: %v\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), err, o.String(), e.String())
		}
		return o.String(), e.String()
	}
	want := map[string]any{
		"plan":    "echo: plan the thing",
		"checked": []any{"check a", "check b"},
		"who":     "me",
	}
	check := func(stdout string) {
		t.Helper()
		var got map[string]any
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("result is not JSON: %v\n%s", err, stdout)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("result = %v, want %v", got, want)
		}
	}

	out, _ := sheepdog("version")
	if !strings.HasPrefix(out, "sheepdog ") {
		t.Errorf("version printed %q", out)
	}

	out, errOut := sheepdog("--session", "smoke", "graph", "run", graph, "the thing", "-i", "who=me")
	check(out)
	id := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(errOut), "run "))
	dir := filepath.Join(work, ".sheepdog", "runs", id)
	if rec := record(t, dir); rec["status"] != "succeeded" {
		t.Fatalf("run.json: %v", rec)
	}
	if _, err := os.Stat(filepath.Join(work, ".sheepdog", ".gitignore")); err != nil {
		t.Error("no .sheepdog/.gitignore")
	}
	fake := readFake(t, state)
	if len(fake.Prompts) != 3 || len(fake.Agents) != 3 {
		t.Fatalf("fake herdr saw %d prompts and %d agents, want 3 and 3", len(fake.Prompts), len(fake.Agents))
	}
	for _, c := range fake.Calls {
		if len(c) < 2 || c[0] != "--session" || c[1] != "smoke" {
			t.Fatalf("herdr called without --session smoke: %q", c)
		}
	}

	// A finished run replays from its journal without prompting anyone.
	out, _ = sheepdog("run", "resume", id)
	check(out)
	if n := len(readFake(t, state).Prompts); n != 3 {
		t.Errorf("resume of a finished run sent %d new prompts", n-3)
	}

	// Lose every journal entry but the first, as if the supervisor died
	// before journaling the parallel calls. Their result files are on disk,
	// so the resume adopts them, again without prompting.
	keepFirstEntry(t, filepath.Join(dir, "journal.jsonl"))
	out, _ = sheepdog("run", "resume", id)
	check(out)
	if n := len(readFake(t, state).Prompts); n != 3 {
		t.Errorf("resume sent %d new prompts; the results were on disk", n-3)
	}
	if rec := record(t, dir); rec["attempts"] != 3.0 || rec["status"] != "succeeded" {
		t.Errorf("run.json after two resumes: %v", rec)
	}
	if _, err := os.Stat(filepath.Join(dir, "supervisor.lock")); err == nil {
		t.Error("supervisor.lock left behind")
	}
}

func build(t *testing.T, out, pkg string) {
	t.Helper()
	cmd := exec.Command("go", "build", "-o", out, pkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build %s: %v\n%s", pkg, err, b)
	}
}

func record(t *testing.T, dir string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

type fakeState struct {
	Agents  map[string]any `json:"agents"`
	Prompts []string       `json:"prompts"`
	Calls   [][]string     `json:"calls"`
}

func readFake(t *testing.T, path string) fakeState {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s fakeState
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func keepFirstEntry(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var keep []byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e struct {
			Seq int `json:"seq"`
		}
		json.Unmarshal(sc.Bytes(), &e)
		if e.Seq == 1 {
			keep = append(sc.Bytes(), '\n')
		}
	}
	f.Close()
	if keep == nil {
		t.Fatal("no entry for call 1 in the journal")
	}
	if err := os.WriteFile(path, keep, 0o644); err != nil {
		t.Fatal(err)
	}
}
