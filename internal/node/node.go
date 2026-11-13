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
	// Grace is how long an agent known to exist may be missing before a call
	// fails. herdr drops a restored agent's name briefly while it relaunches
	// it after a restart. Default 30s.
	Grace time.Duration
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
	mu    sync.Mutex
	name  string
	kind  string
	pane  string
	known bool // an earlier supervisor of this run started its agent
}

// Do runs one call and returns its result: the parsed JSON when the call has
// a schema, otherwise the result text.
//
// Do can pick up a call that a previous supervisor started and did not
// finish: the call's node directory already holds a prompt. It then works out
// how far the call got instead of sending the prompt again blindly.
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
	dir := filepath.Join(e.RunDir, "nodes", c.ExecID)
	if countPrompts(dir) > 0 {
		n.known = true // a prompt was written, so the agent was started
	}
	if err := e.ensureStarted(ctx, n, c.Label); err != nil {
		return nil, fmt.Errorf("start %s: %w", c.Label, err)
	}
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
	retries := 2
	if e.MaxRetries != nil {
		retries = *e.MaxRetries
	}

	attempt := 0
	prompt := c.Prompt + resultInstruction(resultPath, c.Schema)
	if prior := countPrompts(dir); prior > 0 {
		res, problem, delivered, err := e.recoverCall(ctx, n, c, dir, prior, resultPath, validator)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", c.Label, err)
		}
		switch {
		case res != nil:
			return res.v, nil
		case delivered && problem != "":
			// The agent got the last prompt and finished without a usable
			// result: carry on as if that attempt had just failed.
			if prior > retries {
				if c.Schema == nil {
					res, _, err := e.collect(ctx, n, c, dir, resultPath, validator, true)
					if err != nil {
						return nil, err
					}
					return res.v, nil
				}
				return nil, fmt.Errorf("%s: no valid result after %d attempts: %s", c.Label, prior, problem)
			}
			attempt = prior
			prompt = problem + "\nWrite the result again." + resultInstruction(resultPath, c.Schema)
		default:
			// Never delivered: send the last prompt that was written.
			attempt = prior - 1
			raw, err := os.ReadFile(promptFile(dir, prior))
			if err != nil {
				return nil, err
			}
			prompt = string(raw)
		}
	}

	for ; ; attempt++ {
		// A file left by an earlier attempt must not count as this one's.
		if err := os.Remove(resultPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		// Written before sending, so a successor knows a prompt may be out.
		if err := os.WriteFile(promptFile(dir, attempt+1), []byte(prompt), 0o644); err != nil {
			return nil, err
		}
		if err := e.turn(ctx, n, c.Label, prompt); err != nil {
			return nil, fmt.Errorf("%s: %w", c.Label, err)
		}

		res, problem, err := e.collect(ctx, n, c, dir, resultPath, validator, attempt >= retries)
		if err != nil {
			return nil, err
		}
		if res != nil {
			return res.v, nil
		}
		if attempt >= retries {
			return nil, fmt.Errorf("%s: no valid result after %d attempts: %s", c.Label, attempt+1, problem)
		}
		e.logf("%s: %s Asking again.", c.Label, firstLine(problem))
		prompt = problem + "\nWrite the result again." + resultInstruction(resultPath, c.Schema)
	}
}

type result struct{ v any }

// collect reads the result of a finished turn. It returns the result, or the
// problem to send back to the agent.
//
// A plain-text call with no file is asked again like any other, and only the
// last attempt falls back to the pane: a turn can end before the file is
// written (claude can leave a background shell running to write it), and a
// pane read returns the whole screen, not an answer.
func (e *Executor) collect(ctx context.Context, n *node, c Call, dir, resultPath string, validator *jsonschema.Schema, last bool) (*result, string, error) {
	raw, readErr := os.ReadFile(resultPath)
	if c.Schema == nil {
		if readErr == nil {
			return &result{string(raw)}, "", nil
		}
		if !last {
			return nil, fmt.Sprintf("No result file was written to %s.", resultPath), nil
		}
		e.logf("%s: still no result file, reading the pane", c.Label)
		text, err := e.read(ctx, n.name, 200)
		if err != nil {
			return nil, "", fmt.Errorf("%s: no result file and pane read failed: %w", c.Label, err)
		}
		_ = os.WriteFile(filepath.Join(dir, "output.txt"), []byte(text), 0o644)
		return &result{text}, "", nil
	}
	if readErr != nil {
		return nil, fmt.Sprintf("No result file was written to %s.", resultPath), nil
	}
	v, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Sprintf("The result file %s is not valid JSON: %v.", resultPath, err), nil
	}
	if err := validator.Validate(v); err != nil {
		return nil, fmt.Sprintf("The result file %s does not match the schema:\n%v", resultPath, err), nil
	}
	// Re-decode with encoding/json so numbers are float64, as JSON.parse
	// would give the script.
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "", err
	}
	return &result{out}, "", nil
}

// recoverCall works out how far a call a previous supervisor started got.
// prior is the number of prompts it wrote; the last may or may not have been
// sent. It returns a result if the agent finished with one, or whether the
// last prompt was delivered and, if the turn ended, the problem with it.
func (e *Executor) recoverCall(ctx context.Context, n *node, c Call, dir string, prior int, resultPath string, validator *jsonschema.Schema) (res *result, problem string, delivered bool, err error) {
	e.logf("%s: resuming %s, which a previous supervisor started (%d prompt(s) written)", c.Label, c.ExecID, prior)
	a, err := e.getAgent(ctx, n.name)
	if err != nil {
		return nil, "", false, err
	}
	// Let any turn in progress end first: it may be ours.
	if a.Status == herdr.Blocked {
		if a, err = e.waitOutBlocked(ctx, n, c.Label, a); err != nil {
			return nil, "", false, err
		}
	}
	if a.Status == herdr.Working || a.Status == herdr.Unknown {
		e.logf("%s: agent is %s; waiting for its turn to end", c.Label, a.Status)
		for a.Status != herdr.Idle && a.Status != herdr.Done {
			if a.Status == herdr.Blocked {
				a, err = e.waitOutBlocked(ctx, n, c.Label, a)
			} else {
				a, err = e.Herdr.Wait(ctx, n.name, remaining(ctx))
			}
			if err != nil {
				return nil, "", false, err
			}
		}
	}

	// A result file can only come from a delivered prompt: it is deleted
	// before every send.
	if _, statErr := os.Stat(resultPath); statErr == nil {
		res, problem, err := e.collect(ctx, n, c, dir, resultPath, validator, false)
		if err != nil || res != nil {
			if res != nil {
				e.logf("%s: the turn ended with a result; using it", c.Label)
			}
			return res, "", true, err
		}
		return nil, problem, true, nil
	}

	// No file. The prompt names the result path, so the agent's history
	// shows whether the prompt arrived. Wrapping can split the path across
	// lines, so compare with all whitespace removed.
	history, err := e.read(ctx, n.name, 2000)
	if err != nil {
		return nil, "", false, fmt.Errorf("cannot tell whether %s was delivered (%v); not resending", c.ExecID, err)
	}
	if !strings.Contains(squeeze(history), squeeze(resultPath)) {
		e.logf("%s: the last prompt never reached the agent; sending it", c.Label)
		return nil, "", false, nil
	}
	return nil, fmt.Sprintf("No result file was written to %s.", resultPath), true, nil
}

func promptFile(dir string, n int) string {
	return filepath.Join(dir, fmt.Sprintf("prompt-%d.md", n))
}

func countPrompts(dir string) int {
	n := 0
	for {
		if _, err := os.Stat(promptFile(dir, n+1)); err != nil {
			return n
		}
		n++
	}
}

func squeeze(s string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			return -1
		}
		return r
	}, s)
}

// Learn records a label's kind, for a label whose calls were replayed from
// the journal, so later calls may leave the kind out. The agent itself is
// found or started on the label's next live call.
func (e *Executor) Learn(label, kind string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.nodes == nil {
		e.nodes = map[string]*node{}
	}
	if _, ok := e.nodes[label]; !ok && kind != "" {
		e.nodes[label] = &node{name: AgentName(e.RunID, label), kind: kind, known: true}
	}
}

// KindOf returns the kind of a label's agent, or "" for an unknown label.
func (e *Executor) KindOf(label string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if n, ok := e.nodes[label]; ok {
		return n.kind
	}
	return ""
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
	// The agent may already exist: a previous supervisor for this run started
	// it. Names are derived from the run and label, so it can be found again,
	// with its context intact.
	get := e.Herdr.GetAgent
	if n.known {
		// The journal says this label had an agent. If herdr is still
		// restoring panes after a restart, give the agent time to reappear
		// rather than start a second one.
		get = e.getAgent
	}
	if a, err := get(ctx, n.name); err == nil {
		if a.Kind != n.kind {
			return fmt.Errorf("agent %s is %s, but the label is %s", n.name, a.Kind, n.kind)
		}
		e.logf("%s: reusing agent %s in %s", label, n.name, a.PaneID)
		n.pane = a.PaneID
		e.mu.Lock()
		e.lastPane = a.PaneID
		e.mu.Unlock()
		return nil
	} else if !herdr.IsCode(err, herdr.CodeAgentNotFound) {
		return err
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
	before, err := e.getAgent(ctx, n.name)
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

// getAgent is GetAgent for an agent that should exist: agent_not_found is
// retried for e.Grace.
func (e *Executor) getAgent(ctx context.Context, name string) (herdr.Agent, error) {
	var a herdr.Agent
	err := e.graced(ctx, func() (err error) { a, err = e.Herdr.GetAgent(ctx, name); return })
	return a, err
}

func (e *Executor) read(ctx context.Context, name string, lines int) (string, error) {
	var s string
	err := e.graced(ctx, func() (err error) { s, err = e.Herdr.Read(ctx, name, lines); return })
	return s, err
}

func (e *Executor) graced(ctx context.Context, f func() error) error {
	grace := e.Grace
	if grace == 0 {
		grace = 30 * time.Second
	}
	deadline := time.Now().Add(grace)
	for {
		err := f()
		if !herdr.IsCode(err, herdr.CodeAgentNotFound) || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Second):
		}
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
