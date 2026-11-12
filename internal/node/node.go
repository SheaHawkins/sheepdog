// Package node runs one agent() call against a herdr agent: start the agent
// the first time a label appears, send the prompt, wait for the turn to end,
// and read the result file.
package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/SheaHawkins/sheepdog/internal/herdr"
)

// Call is one agent() call.
type Call struct {
	ExecID  string // unique within the run; names the node directory
	Label   string
	Kind    string // required the first time a label appears
	Prompt  string
	Schema  map[string]any
	Timeout time.Duration // 0 means no limit beyond ctx
}

// Executor runs calls for one run. It is safe for concurrent use; calls to
// the same label are serialized, because one agent takes one prompt at a time.
type Executor struct {
	Herdr    herdr.Client
	RunID    string
	RunDir   string // .sheepdog/runs/<run-id>
	Cwd      string // where agents start
	RootPane string // the supervisor's pane; agent panes are split from it

	// AgentArgs are native arguments per kind, passed after `--` to
	// `herdr agent start`.
	AgentArgs    map[string][]string
	StartTimeout time.Duration // default 60s
	DialogPoll   time.Duration // how often to re-check a startup dialog; default 2s
	// MaxRetries is how many times an invalid or missing result file is asked
	// for again. Default 2.
	MaxRetries *int

	// OnBlocked is called when an agent is waiting on a person.
	OnBlocked func(label, agent string)
	// Logf receives progress lines for the run's event log.
	Logf func(format string, args ...any)

	mu       sync.Mutex
	nodes    map[string]*node
	lastPane string
}

type node struct {
	mu   sync.Mutex
	name string
	kind string
	pane string
}

// Do runs one call and returns its result: the parsed JSON when the call has
// a schema, otherwise the result text.
func (e *Executor) Do(ctx context.Context, c Call) (any, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	n, err := e.node(c)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := e.ensureStarted(ctx, n, c.Label); err != nil {
		return nil, fmt.Errorf("start %s: %w", c.Label, err)
	}

	dir := filepath.Join(e.RunDir, "nodes", c.ExecID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	resultPath := filepath.Join(dir, "result.txt")
	if c.Schema != nil {
		resultPath = filepath.Join(dir, "result.json")
	}
	var validator *jsonschema.Schema
	if c.Schema != nil {
		if validator, err = compileSchema(c.Schema); err != nil {
			return nil, fmt.Errorf("%s: bad schema: %w", c.Label, err)
		}
	}

	prompt := c.Prompt + resultInstruction(resultPath, c.Schema)
	retries := 2
	if e.MaxRetries != nil {
		retries = *e.MaxRetries
	}
	for attempt := 0; ; attempt++ {
		// A file left by an earlier attempt must not count as this one's.
		if err := os.Remove(resultPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("prompt-%d.md", attempt+1)), []byte(prompt), 0o644); err != nil {
			return nil, err
		}
		if err := e.turn(ctx, n, c.Label, prompt); err != nil {
			return nil, fmt.Errorf("%s: %w", c.Label, err)
		}

		raw, readErr := os.ReadFile(resultPath)
		if c.Schema == nil {
			if readErr == nil {
				return string(raw), nil
			}
			// Fallback: the agent answered on screen instead.
			e.logf("%s: no result file, reading the pane", c.Label)
			text, err := e.Herdr.Read(ctx, n.name, 200)
			if err != nil {
				return nil, fmt.Errorf("%s: no result file and pane read failed: %w", c.Label, err)
			}
			_ = os.WriteFile(filepath.Join(dir, "output.txt"), []byte(text), 0o644)
			return text, nil
		}

		var problem string
		var v any
		switch {
		case readErr != nil:
			problem = fmt.Sprintf("No result file was written to %s.", resultPath)
		default:
			v, err = jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
			if err != nil {
				problem = fmt.Sprintf("The result file %s is not valid JSON: %v.", resultPath, err)
			} else if err := validator.Validate(v); err != nil {
				problem = fmt.Sprintf("The result file %s does not match the schema:\n%v", resultPath, err)
			}
		}
		if problem == "" {
			// Re-decode with encoding/json so numbers are float64, as JSON.parse
			// would give the script.
			var out any
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, err
			}
			return out, nil
		}
		if attempt >= retries {
			return nil, fmt.Errorf("%s: no valid result after %d attempts: %s", c.Label, attempt+1, problem)
		}
		e.logf("%s: %s Asking again.", c.Label, firstLine(problem))
		prompt = problem + "\nWrite the result again." + resultInstruction(resultPath, c.Schema)
	}
}

func (e *Executor) node(c Call) (*node, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.nodes == nil {
		e.nodes = map[string]*node{}
	}
	n, ok := e.nodes[c.Label]
	if ok {
		if c.Kind != "" && c.Kind != n.kind {
			return nil, fmt.Errorf("label %q is a %s agent, not %s", c.Label, n.kind, c.Kind)
		}
		return n, nil
	}
	if c.Kind == "" {
		return nil, fmt.Errorf("label %q appears for the first time without a kind", c.Label)
	}
	n = &node{name: AgentName(e.RunID, c.Label), kind: c.Kind}
	e.nodes[c.Label] = n
	return n, nil
}

// ensureStarted splits a pane and starts the agent if it is not running yet.
// It holds n.mu but takes e.mu only to pick the pane to split.
func (e *Executor) ensureStarted(ctx context.Context, n *node, label string) error {
	if n.pane != "" {
		return nil
	}
	e.mu.Lock()
	from, dir := e.RootPane, "right"
	if e.lastPane != "" {
		from, dir = e.lastPane, "down"
	}
	pane, err := e.Herdr.SplitPane(ctx, from, dir, e.Cwd)
	if err == nil {
		e.lastPane = pane.PaneID
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}
	_ = e.Herdr.RenamePane(ctx, pane.PaneID, label)

	timeout := e.StartTimeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	e.logf("%s: starting %s agent %s in %s", label, n.kind, n.name, pane.PaneID)
	_, err = e.Herdr.StartAgent(ctx, n.name, n.kind, pane.PaneID, timeout, e.AgentArgs[n.kind]...)
	if herdr.IsCode(err, herdr.CodeNotReady) {
		// A startup dialog, such as a folder trust prompt. A person answers it.
		a, gerr := e.Herdr.GetAgent(ctx, n.name)
		if gerr != nil {
			return err
		}
		if a, err = e.waitOutBlocked(ctx, n, label, a); err != nil {
			return err
		}
		// After the dialog the agent may still be loading.
		if a.Status != herdr.Idle && a.Status != herdr.Done {
			if _, err = e.Herdr.Wait(ctx, n.name, 0, herdr.Idle, herdr.Done); err != nil {
				return err
			}
		}
	} else if err != nil {
		return err
	}
	if err := e.waitOutStartupDialog(ctx, n, label); err != nil {
		return err
	}
	n.pane = pane.PaneID
	return nil
}

// StartupDialogs are screen texts of startup dialogs that herdr reports as
// idle, by agent kind. Codex's folder trust dialog is one: a prompt sent into
// it is lost, and its Enter answers "Trust and continue".
var StartupDialogs = map[string][]string{
	"codex":  {"Trust this folder?"},
	"claude": {"Is this a project you created or one you trust?"},
}

// waitOutStartupDialog waits, for as long as the screen shows a known startup
// dialog, for a person to answer it.
func (e *Executor) waitOutStartupDialog(ctx context.Context, n *node, label string) error {
	markers := StartupDialogs[n.kind]
	if len(markers) == 0 {
		return nil
	}
	poll := e.DialogPoll
	if poll == 0 {
		poll = 2 * time.Second
	}
	reported := false
	for {
		screen, err := e.Herdr.Screen(ctx, n.name)
		if err != nil {
			return err
		}
		if !containsAny(collapse(screen), markers) {
			if reported {
				e.logf("%s: startup dialog answered", label)
			}
			return nil
		}
		if !reported {
			reported = true
			e.logf("%s: startup dialog on screen, waiting for a person", label)
			if e.OnBlocked != nil {
				e.OnBlocked(label, n.name)
			}
			_ = e.Herdr.Notify(ctx, "sheepdog: "+label+" is waiting", "Agent "+n.name+" is showing a startup dialog.")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

// collapse joins wrapped lines so a marker split across them still matches.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// turn sends one prompt and returns when the turn it started has ended.
func (e *Executor) turn(ctx context.Context, n *node, label, prompt string) error {
	before, err := e.Herdr.GetAgent(ctx, n.name)
	if err != nil {
		return err
	}
	if before.Status == herdr.Unknown {
		// Herdr sees an agent but cannot classify its screen. Codex's folder
		// trust dialog looks like this, and a prompt sent into it is lost while
		// its Enter answers the dialog. So never type into an unknown screen.
		e.logf("%s: herdr cannot tell whether the agent is ready; waiting for a person", label)
		if e.OnBlocked != nil {
			e.OnBlocked(label, n.name)
		}
		_ = e.Herdr.Notify(ctx, "sheepdog: "+label+" is waiting", "Agent "+n.name+" shows a screen herdr does not recognize.")
		if before, err = e.Herdr.Wait(ctx, n.name, remaining(ctx), herdr.Idle, herdr.Done); err != nil {
			return err
		}
	}
	if before.Status == herdr.Blocked {
		if before, err = e.waitOutBlocked(ctx, n, label, before); err != nil {
			return err
		}
	}
	if before.Status == herdr.Working {
		// Nothing of ours is running (calls to one label are serialized), so
		// this is leftover work, perhaps a person steering. Let it finish.
		if before, err = e.Herdr.Wait(ctx, n.name, 0); err != nil {
			return err
		}
	}

	a, err := e.Herdr.Prompt(ctx, n.name, prompt, remaining(ctx))
	switch {
	case err == nil:
	case herdr.IsCode(err, herdr.CodeBlocked):
		// Rejected before any input was sent, so sending again is safe.
		if _, err := e.waitOutBlocked(ctx, n, label, herdr.Agent{Name: n.name, Status: herdr.Blocked}); err != nil {
			return err
		}
		return e.turn(ctx, n, label, prompt)
	case herdr.IsCode(err, herdr.CodeTimeout), herdr.IsCode(err, herdr.CodeStalled):
		// Neither proves the prompt was not delivered. A state change after
		// the snapshot does: then the turn is under way and we keep waiting.
		now, gerr := e.Herdr.GetAgent(context.WithoutCancel(ctx), n.name)
		if gerr != nil || now.Seq <= before.Seq {
			return fmt.Errorf("prompt not confirmed delivered (%v); not resending", err)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("prompt delivered but the call timed out while the agent was %s", now.Status)
		}
		a = now
	default:
		return err
	}

	for {
		switch {
		case a.Status == herdr.Blocked:
			if a, err = e.waitOutBlocked(ctx, n, label, a); err != nil {
				return err
			}
		case (a.Status == herdr.Idle || a.Status == herdr.Done) && a.Seq > before.Seq:
			return nil
		default:
			if a, err = e.Herdr.Wait(ctx, n.name, remaining(ctx)); err != nil {
				return err
			}
			if a.Seq <= before.Seq && a.Status.Settled() {
				// A settled state from before our prompt; wait for real work.
				if a, err = e.Herdr.Wait(ctx, n.name, remaining(ctx), herdr.Working, herdr.Blocked); err != nil {
					return err
				}
			}
		}
	}
}

// waitOutBlocked reports a blocked agent and waits until a person has dealt
// with it. Sheepdog never answers on an agent's behalf.
func (e *Executor) waitOutBlocked(ctx context.Context, n *node, label string, a herdr.Agent) (herdr.Agent, error) {
	e.logf("%s: blocked, waiting for a person", label)
	if e.OnBlocked != nil {
		e.OnBlocked(label, n.name)
	}
	_ = e.Herdr.Notify(ctx, "sheepdog: "+label+" is waiting", "Agent "+n.name+" needs an answer.")
	a, err := e.Herdr.Wait(ctx, n.name, remaining(ctx), herdr.Working, herdr.Idle, herdr.Done)
	if err == nil {
		e.logf("%s: unblocked", label)
	}
	return a, err
}

func (e *Executor) logf(format string, args ...any) {
	if e.Logf != nil {
		e.Logf(format, args...)
	}
}

func remaining(ctx context.Context) time.Duration {
	d, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	if r := time.Until(d); r > time.Millisecond {
		return r
	}
	return time.Millisecond
}

func resultInstruction(path string, schema map[string]any) string {
	if schema == nil {
		return fmt.Sprintf("\n\n---\nWhen you have finished, write your final answer as plain text to this file:\n%s\n", path)
	}
	s, _ := json.Marshal(schema)
	return fmt.Sprintf("\n\n---\nWhen you have finished, write your result to this file:\n%s\n"+
		"The file must contain only JSON that validates against this JSON Schema:\n%s\n", path, s)
}

func compileSchema(schema map[string]any) (*jsonschema.Schema, error) {
	// Round-trip so the compiler sees its own number types.
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema.json", doc); err != nil {
		return nil, err
	}
	return c.Compile("schema.json")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

var unsafe = regexp.MustCompile(`[^a-z0-9]+`)

// AgentName is the herdr agent name for a label in a run. Herdr names are
// 1-32 characters of [a-z0-9_-] starting with a letter and unique on the
// server, so a label such as "security-review/security" cannot be used as is.
// The name keeps a readable slug and adds a hash of the exact run and label,
// so two labels never share a name.
func AgentName(runID, label string) string {
	sum := sha256.Sum256([]byte(runID + "\x00" + label))
	hash := hex.EncodeToString(sum[:])[:6]
	run := unsafe.ReplaceAllString(strings.ToLower(runID), "")
	if len(run) > 8 {
		run = run[len(run)-8:] // the tail of a time-ordered ID varies most
	}
	slug := strings.Trim(unsafe.ReplaceAllString(strings.ToLower(label), "-"), "-")
	if len(slug) > 15 {
		slug = strings.TrimRight(slug[:15], "-")
	}
	name := "s" + run + "-" + slug + "-" + hash
	if slug == "" {
		name = "s" + run + "-" + hash
	}
	return name
}
