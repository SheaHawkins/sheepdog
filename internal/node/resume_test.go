package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SheaHawkins/sheepdog/internal/herdr"
)

// crashed sets up what a supervisor that died during call e1 leaves behind:
// a live agent for the label and prompt-1.md in the node directory. It
// returns a fresh executor for the same run, as `run resume` would build.
func crashed(t *testing.T, status herdr.Status, withSchema bool) (*Executor, *fake, string) {
	t.Helper()
	f := &fake{respond: func(f *fake, n int, p string) (herdr.Status, error) { return herdr.Idle, nil }}
	e := newExec(t, f)
	name := AgentName(e.RunID, "x")
	f.agents = map[string]*herdr.Agent{name: {Name: name, Kind: "claude", PaneID: "w1:p2"}}
	f.bump(f.agents[name], status)

	dir := filepath.Join(e.RunDir, "nodes", "e1")
	os.MkdirAll(dir, 0o755)
	result := filepath.Join(dir, "result.txt")
	var s map[string]any
	if withSchema {
		result = filepath.Join(dir, "result.json")
		s = schema
	}
	os.WriteFile(filepath.Join(dir, "prompt-1.md"), []byte("p"+resultInstruction(result, s)), 0o644)
	return e, f, result
}

func call(schemaOn bool) Call {
	c := Call{ExecID: "e1", Label: "x", Kind: "claude", Prompt: "p"}
	if schemaOn {
		c.Schema = schema
	}
	return c
}

func TestResumeAdoptsFinishedResult(t *testing.T) {
	// The agent finished and wrote its result; the supervisor died before
	// journaling it.
	e, f, result := crashed(t, herdr.Idle, true)
	os.WriteFile(result, []byte(`{"file":"a.py","lines":2}`), 0o644)
	got, err := e.Do(context.Background(), call(true))
	if err != nil {
		t.Fatal(err)
	}
	if got.(map[string]any)["file"] != "a.py" {
		t.Errorf("got %v", got)
	}
	if len(f.prompts) != 0 || f.starts != 0 {
		t.Errorf("sent %d prompts and started %d agents; want none", len(f.prompts), f.starts)
	}
}

func TestResumeWaitsForTurnInProgress(t *testing.T) {
	e, f, result := crashed(t, herdr.Working, true)
	f.waits = []herdr.Status{herdr.Idle}
	// The agent writes its result as its turn ends.
	os.WriteFile(result, []byte(`{"file":"a.py","lines":2}`), 0o644)
	if _, err := e.Do(context.Background(), call(true)); err != nil {
		t.Fatal(err)
	}
	if len(f.prompts) != 0 || len(f.waits) != 0 {
		t.Errorf("prompts %d, unused waits %d", len(f.prompts), len(f.waits))
	}
}

func TestResumeDeliveredWithoutResult(t *testing.T) {
	e, f, result := crashed(t, herdr.Idle, true)
	// The prompt is in the agent's history, wrapped mid-path.
	mid := len(result) / 2
	f.screen = "› p\n  When you have finished...\n  " + result[:mid] + "\n  " + result[mid:] + "\n"
	f.respond = func(f *fake, n int, p string) (herdr.Status, error) {
		os.WriteFile(resultPath(t, p), []byte(`{"file":"a.py","lines":2}`), 0o644)
		return herdr.Idle, nil
	}
	if _, err := e.Do(context.Background(), call(true)); err != nil {
		t.Fatal(err)
	}
	if len(f.prompts) != 1 || !strings.HasPrefix(f.prompts[0], "No result file") {
		t.Errorf("want one follow-up asking for the file; prompts: %q", f.prompts)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(result), "prompt-2.md")); err != nil {
		t.Error("the follow-up should be recorded as attempt 2")
	}
}

func TestResumeNeverDelivered(t *testing.T) {
	e, f, _ := crashed(t, herdr.Idle, false)
	f.screen = "› Try \"fix typecheck errors\"\n"
	f.respond = func(f *fake, n int, p string) (herdr.Status, error) {
		os.WriteFile(resultPath(t, p), []byte("ok"), 0o644)
		return herdr.Idle, nil
	}
	got, err := e.Do(context.Background(), call(false))
	if err != nil || got != "ok" {
		t.Fatalf("got %v, %v", got, err)
	}
	want, _ := os.ReadFile(filepath.Join(e.RunDir, "nodes", "e1", "prompt-1.md"))
	if len(f.prompts) != 1 || f.prompts[0] != string(want) {
		t.Errorf("want the original prompt sent once; prompts: %q", f.prompts)
	}
	if f.starts != 0 {
		t.Error("started a new agent instead of reusing the live one")
	}
}

func TestResumePlainTextDeliveredWithoutFile(t *testing.T) {
	e, f, result := crashed(t, herdr.Idle, false)
	f.screen = "› p ... " + result + "\n● the answer\n"
	f.respond = func(f *fake, n int, p string) (herdr.Status, error) {
		os.WriteFile(resultPath(t, p), []byte("the answer"), 0o644)
		return herdr.Idle, nil
	}
	got, err := e.Do(context.Background(), call(false))
	if err != nil || got != "the answer" {
		t.Fatalf("got %v, %v", got, err)
	}
	if len(f.prompts) != 1 || !strings.HasPrefix(f.prompts[0], "No result file") {
		t.Errorf("want one follow-up asking for the file; prompts: %q", f.prompts)
	}
}

func TestReuseLiveAgentForNewCall(t *testing.T) {
	// A later call for a label whose agent a previous supervisor started.
	e, f, _ := crashed(t, herdr.Idle, false)
	e.Learn("x", "claude")
	f.respond = func(f *fake, n int, p string) (herdr.Status, error) {
		os.WriteFile(resultPath(t, p), []byte("ok"), 0o644)
		return herdr.Idle, nil
	}
	if _, err := e.Do(context.Background(), Call{ExecID: "e2", Label: "x", Prompt: "next"}); err != nil {
		t.Fatal(err)
	}
	if f.starts != 0 || len(f.prompts) != 1 {
		t.Errorf("starts %d, prompts %d", f.starts, len(f.prompts))
	}
}

func TestResumeRidesOutRestoreBlip(t *testing.T) {
	// Right after a herdr restart the restored agent's name disappears for a
	// moment. The resume must wait for it, not fail or start a second agent.
	e, f, result := crashed(t, herdr.Idle, true)
	e.Grace = 5 * time.Second
	f.missing = 2
	os.WriteFile(result, []byte(`{"file":"a.py","lines":2}`), 0o644)
	if _, err := e.Do(context.Background(), call(true)); err != nil {
		t.Fatal(err)
	}
	if f.starts != 0 {
		t.Error("started a second agent while the first was being restored")
	}
}
