// Command fakeherdr stands in for the herdr binary in sheepdog's smoke test,
// so the real binary can be exercised end to end, process spawning and
// arguments included, on machines without herdr or logged-in agents.
//
// It keeps its state in the JSON file named by FAKEHERDR_STATE and answers
// the commands herdr.CLI sends. Its agents finish every prompt at once: they
// write "echo: <first line of the prompt>" to the result file, or
// {"echo": "<first line>"} when the prompt asks for JSON.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type agent struct {
	Name   string `json:"name"`
	Kind   string `json:"agent"`
	Status string `json:"agent_status"`
	PaneID string `json:"pane_id"`
	TabID  string `json:"tab_id"`
	Seq    int64  `json:"state_change_seq"`
}

type state struct {
	Panes   int               `json:"panes"`
	Seq     int64             `json:"seq"`
	Agents  map[string]*agent `json:"agents"`
	Prompts []string          `json:"prompts"` // every prompt, in order received
	Calls   [][]string        `json:"calls"`   // every command line, as given
}

func main() {
	args := os.Args[1:]
	if len(args) >= 2 && args[0] == "--session" {
		args = args[2:]
	}
	path := os.Getenv("FAKEHERDR_STATE")
	if path == "" {
		fail("internal", "FAKEHERDR_STATE not set")
	}
	unlock := lock(path + ".lock")
	defer unlock()
	var s state
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &s); err != nil {
			unlock()
			fail("internal", err.Error())
		}
	}
	if s.Agents == nil {
		s.Agents = map[string]*agent{}
	}
	s.Calls = append(s.Calls, os.Args[1:])
	out, err := handle(&s, args)
	raw, _ := json.MarshalIndent(&s, "", "  ")
	if werr := os.WriteFile(path, raw, 0o644); werr != nil && err == nil {
		err = &herdrErr{"internal", werr.Error()}
	}
	unlock()
	if err != nil {
		var he *herdrErr
		if errors.As(err, &he) {
			fail(he.code, he.msg)
		}
		fail("internal", err.Error())
	}
	if s, ok := out.(string); ok {
		fmt.Print(s)
		return
	}
	b, _ := json.Marshal(map[string]any{"result": out})
	fmt.Println(string(b))
}

type herdrErr struct{ code, msg string }

func (e *herdrErr) Error() string { return e.code + ": " + e.msg }

func fail(code, msg string) {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": msg}})
	fmt.Fprintln(os.Stderr, string(b))
	os.Exit(1)
}

func handle(s *state, a []string) (any, error) {
	cmd := strings.Join(a[:min(2, len(a))], " ")
	switch cmd {
	case "workspace create":
		s.Panes++
		return map[string]any{"root_pane": pane(s.Panes)}, nil
	case "pane split":
		s.Panes++
		return map[string]any{"pane": pane(s.Panes)}, nil
	case "pane rename", "pane close", "notification show":
		return map[string]any{}, nil
	}
	if len(a) < 3 || a[0] != "agent" {
		return nil, &herdrErr{"unknown_command", strings.Join(a, " ")}
	}
	name := a[2]
	ag := s.Agents[name]
	if a[1] == "start" {
		if ag != nil {
			return nil, &herdrErr{"agent_name_taken", name}
		}
		ag = &agent{Name: name, Kind: flag(a, "--kind"), PaneID: flag(a, "--pane"), TabID: "w1:t1"}
		s.Agents[name] = ag
		s.bump(ag, "idle")
		return map[string]any{"agent": ag}, nil
	}
	if ag == nil {
		return nil, &herdrErr{"agent_not_found", name}
	}
	switch a[1] {
	case "get", "wait":
		return map[string]any{"agent": ag}, nil
	case "read":
		return "fake screen\n", nil
	case "prompt":
		if len(a) < 4 {
			return nil, &herdrErr{"bad_request", "prompt needs text"}
		}
		text := a[3]
		s.Prompts = append(s.Prompts, text)
		if err := answer(text); err != nil {
			return nil, err
		}
		s.bump(ag, "working")
		s.bump(ag, "idle")
		return map[string]any{"agent": ag}, nil
	}
	return nil, &herdrErr{"unknown_command", strings.Join(a, " ")}
}

// answer writes the result file the prompt names: the line after the one
// ending "to this file:".
func answer(prompt string) error {
	lines := strings.Split(strings.ReplaceAll(prompt, "\r\n", "\n"), "\n")
	for i, l := range lines {
		if strings.HasSuffix(l, "to this file:") && i+1 < len(lines) {
			first := lines[0]
			body := []byte("echo: " + first)
			if strings.Contains(prompt, "JSON Schema") {
				body, _ = json.Marshal(map[string]string{"echo": first})
			}
			return os.WriteFile(lines[i+1], body, 0o644)
		}
	}
	return nil // no result instruction: the agent just answers on screen
}

func (s *state) bump(a *agent, status string) {
	s.Seq++
	a.Seq, a.Status = s.Seq, status
}

func pane(n int) map[string]string {
	return map[string]string{"pane_id": fmt.Sprintf("w1:p%d", n), "tab_id": "w1:t1", "workspace_id": "w1"}
}

func flag(a []string, name string) string {
	for i := range a[:len(a)-1] {
		if a[i] == name {
			return a[i+1]
		}
	}
	return ""
}

// lock serializes concurrent invocations; parallel() calls run several at
// once.
func lock(path string) func() {
	deadline := time.Now().Add(30 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			done := false
			return func() {
				if !done {
					done = true
					os.Remove(path)
				}
			}
		}
		if time.Now().After(deadline) {
			fail("internal", "fakeherdr: state lock held for 30s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
