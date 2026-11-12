// Package herdr drives coding agents through the herdr CLI.
//
// Client is the one interface the rest of sheepdog sees. CLI implements it by
// spawning `herdr` and parsing its JSON; a socket implementation can replace
// it later.
package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Status is an agent's lifecycle state as herdr reports it.
type Status string

const (
	Idle    Status = "idle"
	Working Status = "working"
	Blocked Status = "blocked"
	Done    Status = "done"
	Unknown Status = "unknown"
)

// Settled reports whether the agent has stopped working: ready for input, or
// waiting on a person.
func (s Status) Settled() bool { return s == Idle || s == Done || s == Blocked }

// Agent is herdr's view of one agent.
type Agent struct {
	Name   string `json:"name"`
	Kind   string `json:"agent"`
	Status Status `json:"agent_status"`
	PaneID string `json:"pane_id"`
	TabID  string `json:"tab_id"`
	// Seq is herdr's state-change counter. It is server-wide and only grows,
	// so a value above the one read before a prompt proves the agent changed
	// state after it.
	Seq int64 `json:"state_change_seq"`
}

// Pane is a terminal pane.
type Pane struct {
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
}

// Error is an error herdr returned as JSON.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("herdr: %s: %s", e.Code, e.Message) }

// Error codes sheepdog acts on.
const (
	CodeTimeout       = "timeout"
	CodeStalled       = "agent_prompt_stalled"
	CodeBlocked       = "agent_blocked"
	CodeNotReady      = "agent_not_ready"
	CodeNotIdle       = "agent_not_idle"
	CodeInvalidName   = "invalid_agent_name"
	CodeAgentNotFound = "agent_not_found"
)

// IsCode reports whether err is a herdr error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Client is everything sheepdog needs from herdr.
type Client interface {
	CreateWorkspace(ctx context.Context, cwd, label string) (Pane, error)
	SplitPane(ctx context.Context, pane, direction, cwd string) (Pane, error)
	RenamePane(ctx context.Context, pane, label string) error
	ClosePane(ctx context.Context, pane string) error
	// StartAgent starts an agent of the given kind in an idle shell pane and
	// returns once herdr sees it ready. A startup dialog returns CodeNotReady.
	StartAgent(ctx context.Context, name, kind, pane string, timeout time.Duration, args ...string) (Agent, error)
	// Prompt submits text and waits for the first settled state after it.
	Prompt(ctx context.Context, name, text string, timeout time.Duration) (Agent, error)
	// Wait waits for one of the states (settled states when none are given).
	Wait(ctx context.Context, name string, timeout time.Duration, until ...Status) (Agent, error)
	GetAgent(ctx context.Context, name string) (Agent, error)
	// Read returns recent terminal text. Herdr refuses long reads while the
	// agent is working (CodeNotIdle).
	Read(ctx context.Context, name string, lines int) (string, error)
	// Screen returns the currently visible text.
	Screen(ctx context.Context, name string) (string, error)
	Notify(ctx context.Context, title, body string) error
}

// CLI implements Client by running the herdr binary.
type CLI struct {
	Bin     string // defaults to "herdr"
	Session string // passed as --session when set
}

var _ Client = (*CLI)(nil)

func (c *CLI) run(ctx context.Context, out any, args ...string) error {
	_, err := c.exec(ctx, out, args...)
	return err
}

// exec runs herdr and decodes the JSON result into out (when out is not nil).
// It also returns raw stdout, for the commands that print plain text.
func (c *CLI) exec(ctx context.Context, out any, args ...string) ([]byte, error) {
	bin := c.Bin
	if bin == "" {
		bin = "herdr"
	}
	if c.Session != "" {
		args = append([]string{"--session", c.Session}, args...)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	// Server errors are JSON on stderr with exit status 1.
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	for _, b := range [][]byte{stderr.Bytes(), stdout.Bytes()} {
		if json.Unmarshal(bytes.TrimSpace(b), &env) == nil && env.Error != nil {
			return nil, &Error{Code: env.Error.Code, Message: env.Error.Message}
		}
	}
	if runErr != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("herdr %s: %v: %s", strings.Join(args, " "), runErr, strings.TrimSpace(stderr.String()))
	}
	if out == nil {
		return stdout.Bytes(), nil
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil || env.Result == nil {
		return nil, fmt.Errorf("herdr %s: unexpected output: %q", args[0], stdout.String())
	}
	return stdout.Bytes(), json.Unmarshal(env.Result, out)
}

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

func (c *CLI) CreateWorkspace(ctx context.Context, cwd, label string) (Pane, error) {
	var r struct {
		RootPane Pane `json:"root_pane"`
	}
	err := c.run(ctx, &r, "workspace", "create", "--cwd", cwd, "--label", label, "--no-focus")
	return r.RootPane, err
}

func (c *CLI) SplitPane(ctx context.Context, pane, direction, cwd string) (Pane, error) {
	var r struct {
		Pane Pane `json:"pane"`
	}
	err := c.run(ctx, &r, "pane", "split", pane, "--direction", direction, "--cwd", cwd, "--no-focus")
	return r.Pane, err
}

func (c *CLI) RenamePane(ctx context.Context, pane, label string) error {
	return c.run(ctx, nil, "pane", "rename", pane, label)
}

func (c *CLI) ClosePane(ctx context.Context, pane string) error {
	return c.run(ctx, nil, "pane", "close", pane)
}

type agentResult struct {
	Agent Agent `json:"agent"`
}

func (c *CLI) StartAgent(ctx context.Context, name, kind, pane string, timeout time.Duration, args ...string) (Agent, error) {
	var r agentResult
	a := []string{"agent", "start", name, "--kind", kind, "--pane", pane}
	if timeout > 0 {
		a = append(a, "--timeout", ms(timeout))
	}
	if len(args) > 0 {
		a = append(append(a, "--"), args...)
	}
	err := c.run(ctx, &r, a...)
	return r.Agent, err
}

func (c *CLI) Prompt(ctx context.Context, name, text string, timeout time.Duration) (Agent, error) {
	var r agentResult
	a := []string{"agent", "prompt", name, text, "--wait"}
	if timeout > 0 {
		a = append(a, "--timeout", ms(timeout))
	}
	err := c.run(ctx, &r, a...)
	return r.Agent, err
}

func (c *CLI) Wait(ctx context.Context, name string, timeout time.Duration, until ...Status) (Agent, error) {
	var r agentResult
	a := []string{"agent", "wait", name}
	for _, s := range until {
		a = append(a, "--until", string(s))
	}
	if timeout > 0 {
		a = append(a, "--timeout", ms(timeout))
	}
	err := c.run(ctx, &r, a...)
	return r.Agent, err
}

func (c *CLI) GetAgent(ctx context.Context, name string) (Agent, error) {
	var r agentResult
	err := c.run(ctx, &r, "agent", "get", name)
	return r.Agent, err
}

func (c *CLI) Read(ctx context.Context, name string, lines int) (string, error) {
	// agent read prints plain text, not JSON.
	out, err := c.exec(ctx, nil, "agent", "read", name, "--source", "recent-unwrapped", "--lines", strconv.Itoa(lines))
	return string(out), err
}

func (c *CLI) Screen(ctx context.Context, name string) (string, error) {
	out, err := c.exec(ctx, nil, "agent", "read", name, "--source", "visible")
	return string(out), err
}

func (c *CLI) Notify(ctx context.Context, title, body string) error {
	return c.run(ctx, nil, "notification", "show", title, "--body", body, "--sound", "request")
}
