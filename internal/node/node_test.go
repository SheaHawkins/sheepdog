package node

import (
	"context"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SheaHawkins/sheepdog/internal/herdr"
)

// fake is a herdr.Client whose agents answer prompts with a test-supplied
// function. respond returns the agent's state when Prompt returns, and an
// optional error in place of that.
type fake struct {
	mu      sync.Mutex
	seq     int64
	agents  map[string]*herdr.Agent
	starts  int
	prompts []string
	waits   []herdr.Status // what the next Wait calls return, in order
	screen  string
	screens []string // what successive Screen calls return
	notes   int

	respond func(f *fake, n int, prompt string) (herdr.Status, error)
}

func (f *fake) bump(a *herdr.Agent, s herdr.Status) { f.seq++; a.Seq = f.seq; a.Status = s }

func (f *fake) CreateWorkspace(context.Context, string, string) (herdr.Pane, error) {
	return herdr.Pane{PaneID: "w1:p1"}, nil
}
func (f *fake) SplitPane(_ context.Context, _, _, _ string) (herdr.Pane, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return herdr.Pane{PaneID: "w1:p" + string(rune('2'+f.starts))}, nil
}
func (f *fake) RenamePane(context.Context, string, string) error { return nil }
func (f *fake) ClosePane(context.Context, string) error          { return nil }
func (f *fake) StartAgent(_ context.Context, name, kind, pane string, _ time.Duration, _ ...string) (herdr.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	if f.agents == nil {
		f.agents = map[string]*herdr.Agent{}
	}
	a := &herdr.Agent{Name: name, Kind: kind, PaneID: pane}
	f.bump(a, herdr.Idle)
	f.agents[name] = a
	return *a, nil
}
func (f *fake) Prompt(_ context.Context, name, text string, _ time.Duration) (herdr.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompts = append(f.prompts, text)
	a := f.agents[name]
	s, err := f.respond(f, len(f.prompts), text)
	f.bump(a, s)
	return *a, err
}
func (f *fake) Wait(_ context.Context, name string, _ time.Duration, _ ...herdr.Status) (herdr.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.agents[name]
	s := herdr.Idle
	if len(f.waits) > 0 {
		s, f.waits = f.waits[0], f.waits[1:]
	}
	f.bump(a, s)
	return *a, nil
}
func (f *fake) GetAgent(_ context.Context, name string) (herdr.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *f.agents[name], nil
}
func (f *fake) Read(context.Context, string, int) (string, error) { return f.screen, nil }
func (f *fake) Screen(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.screens) > 0 {
		s := f.screens[0]
		f.screens = f.screens[1:]
		return s, nil
	}
	return "", nil
}
func (f *fake) Notify(context.Context, string, string) error {
	f.mu.Lock()
	f.notes++
	f.mu.Unlock()
	return nil
}

// resultPath pulls the result file path out of a prompt.
func resultPath(t *testing.T, prompt string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^(/.*/result\.(?:json|txt))$`).FindStringSubmatch(prompt)
	if m == nil {
		t.Fatalf("no result path in prompt:\n%s", prompt)
	}
	return m[1]
}

func newExec(t *testing.T, f *fake) *Executor {
	return &Executor{Herdr: f, RunID: "mw3tqhh12fb6b4", RunDir: t.TempDir(), Cwd: ".", RootPane: "w1:p1"}
}

var schema = map[string]any{
	"type":     "object",
	"required": []any{"file", "lines"},
	"properties": map[string]any{
		"file":  map[string]any{"type": "string"},
		"lines": map[string]any{"type": "integer"},
	},
}

func TestAgentName(t *testing.T) {
	valid := regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	seen := map[string]string{}
	for _, label := range []string{
		"implement", "security-review/security", "security-review-security",
		"validate-F1", "a-really-long-label-that-goes-on-and-on", "日本", "",
	} {
		name := AgentName("mw3tqhh12fb6b4", label)
		if !valid.MatchString(name) {
			t.Errorf("AgentName(%q) = %q, not a valid herdr name", label, name)
		}
		if other, dup := seen[name]; dup {
			t.Errorf("labels %q and %q share the name %q", label, other, name)
		}
		seen[name] = label
	}
	if AgentName("run1", "x") == AgentName("run2", "x") {
		t.Error("the same label in two runs shares a name")
	}
}

func TestSchemaResult(t *testing.T) {
	f := &fake{respond: func(f *fake, n int, p string) (herdr.Status, error) {
		os.WriteFile(resultPath(t, p), []byte(`{"file":"hello.py","lines":1}`), 0o644)
		return herdr.Idle, nil
	}}
	got, err := newExec(t, f).Do(context.Background(), Call{ExecID: "e1", Label: "implement", Kind: "claude", Prompt: "Do it.", Schema: schema})
	if err != nil {
		t.Fatal(err)
	}
	if m := got.(map[string]any); m["file"] != "hello.py" || m["lines"] != 1.0 {
		t.Errorf("got %#v", got)
	}
	if !strings.Contains(f.prompts[0], `"required":["file","lines"]`) {
		t.Errorf("prompt does not carry the schema:\n%s", f.prompts[0])
	}
}

func TestSchemaRetry(t *testing.T) {
	f := &fake{respond: func(f *fake, n int, p string) (herdr.Status, error) {
		body := `{"file":"hello.py"}` // missing lines
		if n == 2 {
			body = `{"file":"hello.py","lines":1}`
		}
		os.WriteFile(resultPath(t, p), []byte(body), 0o644)
		return herdr.Idle, nil
	}}
	if _, err := newExec(t, f).Do(context.Background(), Call{ExecID: "e1", Label: "x", Kind: "claude", Prompt: "p", Schema: schema}); err != nil {
		t.Fatal(err)
	}
	if len(f.prompts) != 2 || !strings.Contains(f.prompts[1], "does not match the schema") || !strings.Contains(f.prompts[1], "lines") {
		t.Errorf("retry prompt should carry the validation error; prompts: %q", f.prompts)
	}
}

func TestSchemaGivesUp(t *testing.T) {
	f := &fake{respond: func(*fake, int, string) (herdr.Status, error) { return herdr.Idle, nil }} // never writes
	_, err := newExec(t, f).Do(context.Background(), Call{ExecID: "e1", Label: "x", Kind: "claude", Prompt: "p", Schema: schema})
	if err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("err = %v", err)
	}
	if len(f.prompts) != 3 {
		t.Errorf("sent %d prompts, want 3", len(f.prompts))
	}
}

func TestStaleResultIgnored(t *testing.T) {
	e := newExec(t, &fake{})
	f := e.Herdr.(*fake)
	// The first call writes a result; the second writes nothing, and must not
	// read the first call's file even though it reuses the exec ID.
	f.respond = func(f *fake, n int, p string) (herdr.Status, error) {
		if n == 1 {
			os.WriteFile(resultPath(t, p), []byte("first"), 0o644)
		}
		return herdr.Idle, nil
	}
	f.screen = "from the pane"
	ctx := context.Background()
	if got, _ := e.Do(ctx, Call{ExecID: "e1", Label: "x", Kind: "claude", Prompt: "p"}); got != "first" {
		t.Fatalf("first = %v", got)
	}
	if got, _ := e.Do(ctx, Call{ExecID: "e1", Label: "x", Prompt: "p"}); got != "from the pane" {
		t.Errorf("second = %v, want the pane fallback", got)
	}
	if f.starts != 1 {
		t.Errorf("started %d agents for one label", f.starts)
	}
}

func TestBlockedMidTurn(t *testing.T) {
	f := &fake{
		respond: func(f *fake, n int, p string) (herdr.Status, error) {
			os.WriteFile(resultPath(t, p), []byte("ok"), 0o644)
			return herdr.Blocked, nil
		},
		waits: []herdr.Status{herdr.Working, herdr.Idle}, // person answers; agent finishes
	}
	var blocked []string
	e := newExec(t, f)
	e.OnBlocked = func(label, _ string) { blocked = append(blocked, label) }
	got, err := e.Do(context.Background(), Call{ExecID: "e1", Label: "sec", Kind: "claude", Prompt: "p"})
	if err != nil || got != "ok" {
		t.Fatalf("got %v, %v", got, err)
	}
	if len(blocked) != 1 || f.notes != 1 {
		t.Errorf("blocked callbacks %v, notifications %d", blocked, f.notes)
	}
	if len(f.prompts) != 1 {
		t.Errorf("sent %d prompts; a blocked agent must never be answered or re-prompted", len(f.prompts))
	}
}

func TestTimeoutAfterDelivery(t *testing.T) {
	f := &fake{respond: func(f *fake, n int, p string) (herdr.Status, error) {
		os.WriteFile(resultPath(t, p), []byte("ok"), 0o644)
		return herdr.Working, &herdr.Error{Code: herdr.CodeStalled}
	}}
	got, err := newExec(t, f).Do(context.Background(), Call{ExecID: "e1", Label: "x", Kind: "claude", Prompt: "p"})
	if err != nil || got != "ok" {
		t.Fatalf("got %v, %v", got, err)
	}
	if len(f.prompts) != 1 {
		t.Errorf("resent a delivered prompt")
	}
}

// stuckFake reports a stall without any state change: the prompt may or may
// not have arrived.
type stuckFake struct{ *fake }

func (s stuckFake) Prompt(_ context.Context, name, text string, _ time.Duration) (herdr.Agent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, text)
	return *s.agents[name], &herdr.Error{Code: herdr.CodeStalled}
}

func TestStallWithoutEvidenceIsNotResent(t *testing.T) {
	s := stuckFake{&fake{}}
	e := newExec(t, s.fake)
	e.Herdr = s
	_, err := e.Do(context.Background(), Call{ExecID: "e1", Label: "x", Kind: "claude", Prompt: "p"})
	if err == nil || !strings.Contains(err.Error(), "not resending") {
		t.Fatalf("err = %v", err)
	}
	if len(s.prompts) != 1 {
		t.Errorf("sent %d prompts", len(s.prompts))
	}
}

func TestKindRules(t *testing.T) {
	e := newExec(t, &fake{respond: func(*fake, int, string) (herdr.Status, error) { return herdr.Idle, nil }})
	ctx := context.Background()
	if _, err := e.Do(ctx, Call{ExecID: "e1", Label: "x", Prompt: "p"}); err == nil {
		t.Error("first call without a kind succeeded")
	}
	e.Do(ctx, Call{ExecID: "e2", Label: "y", Kind: "claude", Prompt: "p"})
	if _, err := e.Do(ctx, Call{ExecID: "e3", Label: "y", Kind: "codex", Prompt: "p"}); err == nil {
		t.Error("changing a label's kind succeeded")
	}
}

// unknownFake starts agents on a screen herdr cannot classify, as codex's
// folder trust dialog is.
type unknownFake struct{ *fake }

func (u unknownFake) StartAgent(ctx context.Context, name, kind, pane string, d time.Duration, args ...string) (herdr.Agent, error) {
	a, err := u.fake.StartAgent(ctx, name, kind, pane, d, args...)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.agents[name].Status = herdr.Unknown
	a.Status = herdr.Unknown
	return a, err
}

func TestNoPromptIntoUnknownScreen(t *testing.T) {
	u := unknownFake{&fake{respond: func(f *fake, n int, p string) (herdr.Status, error) {
		os.WriteFile(resultPath(t, p), []byte("ok"), 0o644)
		return herdr.Idle, nil
	}}}
	u.respond = func(f *fake, n int, p string) (herdr.Status, error) {
		if f.agents[AgentName("mw3tqhh12fb6b4", "x")].Status == herdr.Unknown {
			t.Error("prompt sent while the agent's screen was unknown")
		}
		os.WriteFile(resultPath(t, p), []byte("ok"), 0o644)
		return herdr.Idle, nil
	}
	e := newExec(t, u.fake)
	e.Herdr = u
	var blocked int
	e.OnBlocked = func(string, string) { blocked++ }
	if got, err := e.Do(context.Background(), Call{ExecID: "e1", Label: "x", Kind: "codex", Prompt: "p"}); err != nil || got != "ok" {
		t.Fatalf("got %v, %v", got, err)
	}
	if blocked != 1 {
		t.Errorf("OnBlocked called %d times, want 1", blocked)
	}
}

func TestStartupDialogWaitsForAPerson(t *testing.T) {
	f := &fake{
		// herdr reports idle throughout, as it does for codex's trust dialog.
		screens: []string{"  Trust this\n  folder? Codex can read", "Trust this folder?", "› Ask Codex to do anything"},
		respond: func(f *fake, n int, p string) (herdr.Status, error) {
			if len(f.screens) > 0 {
				t.Error("prompt sent while the trust dialog was on screen")
			}
			os.WriteFile(resultPath(t, p), []byte("ok"), 0o644)
			return herdr.Idle, nil
		},
	}
	e := newExec(t, f)
	e.DialogPoll = time.Millisecond
	var blocked int
	e.OnBlocked = func(string, string) { blocked++ }
	if got, err := e.Do(context.Background(), Call{ExecID: "e1", Label: "x", Kind: "codex", Prompt: "p"}); err != nil || got != "ok" {
		t.Fatalf("got %v, %v", got, err)
	}
	if blocked != 1 || f.notes != 1 {
		t.Errorf("blocked %d, notifications %d; want one each", blocked, f.notes)
	}
}
