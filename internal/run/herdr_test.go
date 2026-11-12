package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SheaHawkins/sheepdog/internal/herdr"
	"github.com/SheaHawkins/sheepdog/internal/node"
)

// The kill test runs the supervisor in a child process (this test binary,
// re-executed), so it can be killed with SIGKILL like a real crash.
func TestMain(m *testing.M) {
	if os.Getenv("SHEEPDOG_TEST_SUPERVISOR") == "1" {
		os.Exit(childSupervisor())
	}
	os.Exit(m.Run())
}

func childSupervisor() int {
	_, err := Execute(context.Background(), herdrConfig(os.Getenv("SHEEPDOG_TEST_DIR"), os.Getenv("SHEEPDOG_TEST_PANE")))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func herdrConfig(dir, pane string) Config {
	p, err := compileFile("testdata/kill.ts")
	if err != nil {
		panic(err)
	}
	return Config{
		Dir: dir, ID: filepath.Base(dir), Program: p,
		Executor: &node.Executor{
			Herdr: &herdr.CLI{Session: os.Getenv("SHEEPDOG_HERDR_SESSION")},
			RunID: filepath.Base(dir), RunDir: dir,
			Cwd: os.Getenv("SHEEPDOG_HERDR_CWD"), RootPane: pane,
			Logf: Logf(dir),
		},
	}
}

// TestKillAndResume kills a supervisor with SIGKILL while its agent is mid
// turn, then resumes the run. It needs a running herdr session and a folder
// claude already trusts:
//
//	herdr --session sheepdog-spike server &
//	SHEEPDOG_HERDR_SESSION=sheepdog-spike SHEEPDOG_HERDR_CWD=/trusted/scratch \
//	  go test ./internal/run -run KillAndResume -v
func TestKillAndResume(t *testing.T) { crashAndResume(t, false) }

// TestHerdrRestartAndResume stops the herdr server while the agent is mid
// turn, starts it again, and resumes the run. It stops every pane in the
// session, so it also needs SHEEPDOG_HERDR_RESTART=1.
func TestHerdrRestartAndResume(t *testing.T) {
	if os.Getenv("SHEEPDOG_HERDR_RESTART") != "1" {
		t.Skip("SHEEPDOG_HERDR_RESTART not set")
	}
	crashAndResume(t, true)
}

func crashAndResume(t *testing.T, restartHerdr bool) {
	session, cwd := os.Getenv("SHEEPDOG_HERDR_SESSION"), os.Getenv("SHEEPDOG_HERDR_CWD")
	if session == "" || cwd == "" {
		t.Skip("SHEEPDOG_HERDR_SESSION and SHEEPDOG_HERDR_CWD not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	h := &herdr.CLI{Session: session}
	root, err := h.CreateWorkspace(ctx, cwd, "sheepdog-kill")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "k"+time.Now().Format("150405"))

	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), "SHEEPDOG_TEST_SUPERVISOR=1", "SHEEPDOG_TEST_DIR="+dir, "SHEEPDOG_TEST_PANE="+root.PaneID)
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}

	// Kill it once call 2 has been sent and the agent is working on it.
	agent := node.AgentName(filepath.Base(dir), "worker")
	for {
		if ctx.Err() != nil {
			child.Process.Kill()
			t.Fatal("call 2 never started")
		}
		if _, err := os.Stat(filepath.Join(dir, "nodes", "e2", "prompt-1.md")); err == nil {
			if a, err := h.GetAgent(ctx, agent); err == nil && a.Status == herdr.Working {
				break
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	time.Sleep(2 * time.Second)
	if restartHerdr {
		// The supervisor's herdr calls start failing, and it gives up.
		if out, err := exec.Command("herdr", "--session", session, "server", "stop").CombinedOutput(); err != nil {
			t.Fatalf("server stop: %v: %s", err, out)
		}
		child.Wait()
		t.Logf("stopped herdr during call 2; supervisor exited: %s", readRecord(t, dir).Status)
		server := exec.Command("herdr", "--session", session, "server")
		if err := server.Start(); err != nil {
			t.Fatal(err)
		}
		t.Logf("restarted herdr (pid %d); it keeps running after the test", server.Process.Pid)
		for {
			if a, err := h.GetAgent(ctx, agent); err == nil && (a.Status == herdr.Idle || a.Status == herdr.Done) {
				t.Logf("agent restored as %s in %s", a.Status, a.PaneID)
				break
			}
			if ctx.Err() != nil {
				t.Fatal("agent never came back after the restart")
			}
			time.Sleep(time.Second)
		}
	} else {
		if err := child.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		child.Wait()
		t.Logf("killed supervisor %d during call 2", child.Process.Pid)

		rec := readRecord(t, dir)
		if rec.Status != Running || rec.PID != child.Process.Pid {
			t.Errorf("after the crash run.json says %s, pid %d", rec.Status, rec.PID)
		}
		if _, err := os.Stat(filepath.Join(dir, "supervisor.lock")); err != nil {
			t.Error("the crash should leave a stale lock behind")
		}
	}

	start := time.Now()
	got, err := Execute(ctx, herdrConfig(dir, root.PaneID))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("resumed in %s: %q", time.Since(start).Round(time.Millisecond), got)
	words := []string{"ONE", "TWO", "THREE"}
	for i, v := range got.([]any) {
		if strings.ToUpper(strings.TrimSpace(v.(string))) != words[i] {
			t.Errorf("result %d = %q, want %s", i+1, v, words[i])
		}
	}

	// What the resume did, from the event log.
	log := resumeEvents(t, dir)
	t.Logf("resume events:\n  %s", strings.Join(log, "\n  "))
	text := strings.Join(log, "\n")
	for _, want := range []string{"reusing agent", "resuming e2"} {
		if !strings.Contains(text, want) {
			t.Errorf("resume did not log %q", want)
		}
	}
	bads := []string{"call 1 (e1)", "starting claude agent", "reading the pane"}
	if !restartHerdr {
		// After a plain crash the agent's history always shows the prompt.
		bads = append(bads, "never reached the agent")
	}
	for _, bad := range bads {
		if strings.Contains(text, bad) {
			t.Errorf("resume logged %q", bad)
		}
	}
}

// resumeEvents returns the event messages after the resume began.
func resumeEvents(t *testing.T, dir string) []string {
	raw, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	seen := false
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e struct{ Msg string }
		json.Unmarshal([]byte(line), &e)
		if strings.HasPrefix(e.Msg, "resuming (") {
			seen = true
		}
		if seen {
			out = append(out, e.Msg)
		}
	}
	return out
}
