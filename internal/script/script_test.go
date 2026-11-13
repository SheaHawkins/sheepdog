package script

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost stands in for herdr. Agent blocks for a while, like
// `herdr agent prompt --wait` would, and records what it was asked.
type fakeHost struct {
	mu       sync.Mutex
	calls    []string // "label: prompt"
	logs     []string
	inFlight int
	maxIn    int
	respond  func(prompt string, opts AgentOptions, n int) (any, error)
}

func (h *fakeHost) Agent(ctx context.Context, prompt string, opts AgentOptions) (any, error) {
	h.mu.Lock()
	h.calls = append(h.calls, opts.Label+": "+prompt)
	n := len(h.calls)
	h.inFlight++
	h.maxIn = max(h.maxIn, h.inFlight)
	h.mu.Unlock()

	time.Sleep(50 * time.Millisecond)

	h.mu.Lock()
	h.inFlight--
	h.mu.Unlock()
	if h.respond == nil {
		return "ok", nil
	}
	return h.respond(prompt, opts, n)
}

func (h *fakeHost) Log(msg string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.logs = append(h.logs, msg)
}

func mustCompile(t *testing.T, name string) *Program {
	t.Helper()
	p, err := Compile("testdata/" + name)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	return p
}

func TestSpecExample(t *testing.T) {
	p := mustCompile(t, "secure-implement.ts")
	securityRuns := 0
	host := &fakeHost{respond: func(prompt string, opts AgentOptions, n int) (any, error) {
		switch {
		case opts.Label == "security":
			securityRuns++
			if opts.Schema["required"] == nil {
				t.Errorf("schema from the imported file did not reach the host: %v", opts.Schema)
			}
			if securityRuns == 1 {
				return map[string]any{"findings": []any{
					map[string]any{"id": "1", "title": "SQL injection"},
					map[string]any{"id": "2", "title": "XSS"},
					map[string]any{"id": "3", "title": "SSRF"},
				}}, nil
			}
			return map[string]any{"findings": []any{}}, nil
		case strings.HasPrefix(opts.Label, "validate-"):
			return map[string]any{"id": opts.Label[9:], "isReal": opts.Label == "validate-1", "reason": "checked"}, nil
		}
		return "done", nil
	}}

	start := time.Now()
	result, err := Run(context.Background(), p, map[string]any{"instruction": "add login"}, host)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	want := []string{
		"implement: Implement: add login",
		"security: Security review the diff.",
		// the three validators, in any order
		"implement: Fix these confirmed findings: [{\"id\":\"1\",\"isReal\":true,\"reason\":\"checked\"}]",
		"security: Security review the diff.",
	}
	got := host.calls
	if len(got) != 7 {
		t.Fatalf("got %d calls, want 7: %q", len(got), got)
	}
	validators := got[2:5]
	got = append(append([]string{}, got[:2]...), got[5:]...)
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d = %q, want %q", i, got[i], want[i])
		}
	}
	for _, v := range validators {
		if !strings.HasPrefix(v, "validate-") {
			t.Errorf("expected a validator call, got %q", v)
		}
	}

	// parallel() really ran the validators at the same time.
	if host.maxIn != 3 {
		t.Errorf("max concurrent agent calls = %d, want 3", host.maxIn)
	}
	// 7 calls of 50ms each; sequential validators would take 350ms.
	if elapsed > 300*time.Millisecond {
		t.Errorf("run took %v; parallel calls look serialised", elapsed)
	}

	if m, ok := result.(map[string]any); !ok || m["rounds"] != int64(2) {
		t.Errorf("result = %#v, want {rounds: 2}", result)
	}
	if strings.Join(host.logs, "|") != "round 0: 1 confirmed|round 1: 0 confirmed" {
		t.Errorf("logs = %q", host.logs)
	}
}

func TestNilArgsIsAnEmptyObject(t *testing.T) {
	p := mustCompile(t, "secure-implement.ts")
	host := &fakeHost{respond: func(_ string, opts AgentOptions, _ int) (any, error) {
		return map[string]any{"findings": []any{}}, nil
	}}
	if _, err := Run(context.Background(), p, nil, host); err != nil {
		t.Fatal(err)
	}
	if host.calls[0] != "implement: Implement: undefined" {
		t.Errorf("first call = %q", host.calls[0])
	}
}

func TestMetaWithoutRunning(t *testing.T) {
	p := mustCompile(t, "secure-implement.ts")
	meta, err := Meta(p)
	if err != nil {
		t.Fatal(err)
	}
	if meta["name"] != "secure-implement" {
		t.Errorf("name = %v", meta["name"])
	}
	loop, _ := meta["loop"].(map[string]any)
	if loop["every"] != "6h" {
		t.Errorf("loop = %v", meta["loop"])
	}
}

func TestErrorsKeepOriginalLineNumbers(t *testing.T) {
	p := mustCompile(t, "errors.ts")
	host := &fakeHost{respond: func(string, AgentOptions, int) (any, error) {
		return nil, errors.New("agent exited")
	}}
	_, err := Run(context.Background(), p, nil, host)
	if err == nil {
		t.Fatal("expected the script's throw to fail the run")
	}
	if !strings.Contains(err.Error(), "boom from line 11") {
		t.Errorf("error = %v", err)
	}
	// The inline source map points back at the .ts file and line.
	if !strings.Contains(err.Error(), "errors.ts:11") {
		t.Errorf("stack does not point at errors.ts:11:\n%v", err)
	}
	if len(host.logs) != 1 || host.logs[0] != "caught: agent exited" {
		t.Errorf("a rejected agent() should be catchable; logs = %q", host.logs)
	}
}

func TestCompileErrors(t *testing.T) {
	for name, want := range map[string]string{
		"syntax.ts": "unexpected end of file",
		"npm.ts":    `can only import relative paths, not "lodash"`,
	} {
		_, err := Compile("testdata/" + name)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: error = %v, want it to contain %q", name, err, want)
		}
	}
}

func TestNondeterminismIsUnavailable(t *testing.T) {
	p := mustCompile(t, "nondeterministic.ts")
	for _, which := range []string{"random", "now", "new"} {
		_, err := Run(context.Background(), p, map[string]any{"which": which}, &fakeHost{})
		if err == nil || !strings.Contains(err.Error(), "deterministic") {
			t.Errorf("%s: error = %v", which, err)
		}
	}
}

func TestMaxCalls(t *testing.T) {
	p := mustCompile(t, "runaway.ts")
	host := &fakeHost{}
	_, err := Run(context.Background(), p, nil, host)
	if err == nil || !strings.Contains(err.Error(), "maxCalls (5)") {
		t.Fatalf("error = %v", err)
	}
	if len(host.calls) != 5 {
		t.Errorf("host saw %d calls, want 5", len(host.calls))
	}
}

func TestHangIsDetected(t *testing.T) {
	p := mustCompile(t, "hang.ts")
	_, err := Run(context.Background(), p, nil, &fakeHost{})
	if err == nil || !strings.Contains(err.Error(), "never finish") {
		t.Fatalf("error = %v", err)
	}
}

func TestSplitHeader(t *testing.T) {
	cases := []struct {
		name, src, body, err string
	}{
		{name: "plain", src: "export const meta = { a: 1 }\nawait x()", body: "\nawait x()"},
		{name: "semicolon", src: "export const meta = {a: '}'}; await x()", body: " await x()"},
		{name: "as const", src: "export const meta = {\n  a: `${'}'}`,\n}\n  as const\nreturn 1", body: "\nreturn 1"},
		{name: "imports first", src: "import { A } from './a'\nimport type { B } from \"./b\";\nexport const meta = {}\nlog(A)", body: "\nlog(A)"},
		{name: "comments", src: "// leading\n/* block } */\nexport const meta = { a: 1 } // trailing\nx()", body: "// trailing\nx()"},
		{name: "no meta", src: "await agent('x', {})", err: "must begin with `export const meta"},
		{name: "other export", src: "export const meta = {}\nexport function f() {}", err: "only `export const meta`"},
		{name: "export first", src: "export function f() {}", err: "only `export const meta`"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			header, body, err := splitHeader(c.src)
			if c.err != "" {
				if err == nil || !strings.Contains(err.Error(), c.err) {
					t.Fatalf("error = %v, want %q", err, c.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if body != c.body {
				t.Errorf("body = %q, want %q", body, c.body)
			}
			if header+body != c.src {
				t.Errorf("header + body does not reassemble the source")
			}
		})
	}
}

func TestSeqFollowsCallOrder(t *testing.T) {
	p := mustCompile(t, "seq.ts")
	var mu sync.Mutex
	seqs := map[string]int{}
	host := &fakeHost{respond: func(prompt string, opts AgentOptions, _ int) (any, error) {
		// Finish in reverse issue order.
		time.Sleep(time.Duration(4-opts.Seq) * 10 * time.Millisecond)
		mu.Lock()
		seqs[prompt] = opts.Seq
		mu.Unlock()
		return prompt, nil
	}}
	if _, err := Run(context.Background(), p, nil, host); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"p3": 1, "p2": 2, "p1": 3, "last": 4}
	for k, v := range want {
		if seqs[k] != v {
			t.Errorf("seq of %s = %d, want %d", k, seqs[k], v)
		}
	}
}
