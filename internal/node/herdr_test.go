package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SheaHawkins/sheepdog/internal/herdr"
)

// TestHerdrEndToEnd drives a real agent through the herdr CLI. It needs a
// running herdr server in a session you are happy to experiment in:
//
//	herdr --session sheepdog-spike server &
//	SHEEPDOG_HERDR_SESSION=sheepdog-spike go test ./internal/node -run Herdr -v
//
// SHEEPDOG_HERDR_KIND picks the agent kind (default claude), and
// SHEEPDOG_HERDR_CWD the folder it works in, which the agent must already
// trust; otherwise its trust dialog blocks the test until a person answers.
func TestHerdrEndToEnd(t *testing.T) {
	session := os.Getenv("SHEEPDOG_HERDR_SESSION")
	if session == "" {
		t.Skip("SHEEPDOG_HERDR_SESSION not set")
	}
	kind := os.Getenv("SHEEPDOG_HERDR_KIND")
	if kind == "" {
		kind = "claude"
	}
	cwd := os.Getenv("SHEEPDOG_HERDR_CWD")
	if cwd == "" {
		cwd = t.TempDir()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	h := &herdr.CLI{Session: session}
	root, err := h.CreateWorkspace(ctx, cwd, "sheepdog-test")
	if err != nil {
		t.Fatal(err)
	}
	runID := "t" + time.Now().Format("150405")
	e := &Executor{
		Herdr: h, RunID: runID, RunDir: filepath.Join(t.TempDir(), "run"), Cwd: cwd, RootPane: root.PaneID,
		Logf:      t.Logf,
		OnBlocked: func(label, agent string) { t.Logf("BLOCKED %s (%s): answer it in herdr", label, agent) },
	}

	start := time.Now()
	got, err := e.Do(ctx, Call{
		ExecID: "e1", Label: "implement", Kind: kind, Schema: schema,
		Prompt: "Create a file hello.py in the current directory that prints 'hello, sheepdog'.",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("turn 1 in %s: %v", time.Since(start).Round(time.Millisecond), got)
	if m, ok := got.(map[string]any); !ok || m["file"] == nil {
		t.Fatalf("result %#v", got)
	}

	// A second call to the same label goes to the same agent, context intact.
	start = time.Now()
	got, err = e.Do(ctx, Call{
		ExecID: "e2", Label: "implement",
		Prompt: "What exactly does the file you just created print? Answer with only the printed text.",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("turn 2 in %s: %q", time.Since(start).Round(time.Millisecond), got)
	if s, _ := got.(string); s == "" {
		t.Fatal("empty result")
	}
}
