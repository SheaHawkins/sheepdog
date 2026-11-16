// Command sheepdog runs TypeScript graphs of coding agents in herdr.
//
// This is the spike 4 binary: just enough CLI to run a graph file and resume
// it, so the same binary can be built and exercised on macOS, Linux and
// Windows. The supervisor runs in the foreground, and agents get a fresh
// herdr workspace instead of the run's own tab.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SheaHawkins/sheepdog/internal/herdr"
	"github.com/SheaHawkins/sheepdog/internal/node"
	"github.com/SheaHawkins/sheepdog/internal/run"
	"github.com/SheaHawkins/sheepdog/internal/script"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage:
  sheepdog [--session NAME] graph run FILE.ts ["instruction"] [-i KEY=VALUE]...
  sheepdog [--session NAME] run resume RUN_ID
  sheepdog version

Runs live in .sheepdog/runs/ under the current directory. The run ID goes to
stderr and the graph's result, as JSON, to stdout.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := cli(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "sheepdog:", err)
		os.Exit(1)
	}
}

func cli(ctx context.Context, argv []string, stdout, stderr io.Writer) error {
	global := flag.NewFlagSet("sheepdog", flag.ContinueOnError)
	global.SetOutput(stderr)
	global.Usage = func() { fmt.Fprint(stderr, usage) }
	session := global.String("session", "", "herdr session")
	if err := global.Parse(argv); err != nil {
		return err
	}
	args := global.Args()
	switch {
	case len(args) == 1 && args[0] == "version":
		fmt.Fprintln(stdout, "sheepdog", version)
		return nil
	case len(args) >= 3 && args[0] == "graph" && args[1] == "run":
		return graphRun(ctx, *session, args[2:], stdout, stderr)
	case len(args) >= 2 && args[0] == "plugin":
		return pluginCmd(args[1:], stdout)
	case len(args) == 3 && args[0] == "run" && args[1] == "resume":
		return runResume(ctx, *session, args[2], stdout, stderr)
	}
	fmt.Fprint(stderr, usage)
	return errors.New("unknown command")
}

func graphRun(ctx context.Context, session string, argv []string, stdout, stderr io.Writer) error {
	// Flags may come before or after the positional arguments.
	inputs := map[string]any{}
	var pos []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "-i" || a == "--input":
			if i+1 == len(argv) {
				return fmt.Errorf("%s needs KEY=VALUE", a)
			}
			i++
			k, v, ok := strings.Cut(argv[i], "=")
			if !ok || k == "" {
				return fmt.Errorf("bad input %q: want KEY=VALUE", argv[i])
			}
			inputs[k] = v
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %s", a)
		default:
			pos = append(pos, a)
		}
	}
	if len(pos) < 1 || len(pos) > 2 {
		return errors.New(`graph run takes FILE.ts and an optional "instruction"`)
	}
	if len(pos) == 2 {
		inputs["instruction"] = pos[1]
	}
	p, err := script.Compile(pos[0])
	if err != nil {
		return err
	}
	root, err := stateDir()
	if err != nil {
		return err
	}
	id := newRunID()
	fmt.Fprintln(stderr, "run", id)
	return execute(ctx, session, filepath.Join(root, "runs", id), id, p, inputs, stdout)
}

func runResume(ctx context.Context, session, id string, stdout, stderr io.Writer) error {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return fmt.Errorf("bad run ID %q", id)
	}
	root, err := stateDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, "runs", id)
	raw, err := os.ReadFile(filepath.Join(dir, "run.json"))
	if err != nil {
		return fmt.Errorf("no run %s: %w", id, err)
	}
	var rec run.Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("run.json: %w", err)
	}
	p, err := script.Compile(rec.Script)
	if err != nil {
		return err
	}
	fmt.Fprintln(stderr, "resuming run", id)
	return execute(ctx, session, dir, id, p, rec.Args, stdout)
}

func execute(ctx context.Context, session, dir, id string, p *script.Program, inputs map[string]any, stdout io.Writer) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Inside herdr, HERDR_BIN_PATH names the herdr binary of the server that
	// launched us; outside it is unset and the CLI finds herdr on the PATH.
	h := &herdr.CLI{Bin: os.Getenv("HERDR_BIN_PATH"), Session: session}
	ws, err := h.CreateWorkspace(ctx, cwd, "sheepdog "+id)
	if err != nil {
		return fmt.Errorf("creating a herdr workspace: %w", err)
	}
	logf := run.Logf(dir)
	result, err := run.Execute(ctx, run.Config{
		Dir: dir, ID: id, Program: p, Args: inputs,
		Executor: &node.Executor{
			Herdr: h, RunID: id, RunDir: dir, Cwd: cwd, RootPane: ws.PaneID,
			Logf: logf,
		},
	})
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

// stateDir returns .sheepdog in the current directory, creating it with the
// .gitignore that keeps run history out of the repository.
func stateDir() (string, error) {
	root, err := filepath.Abs(".sheepdog")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	ignore := filepath.Join(root, ".gitignore")
	if _, err := os.Stat(ignore); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(ignore, []byte("runs/\nloops/\n"), 0o644); err != nil {
			return "", err
		}
	}
	return root, nil
}

// newRunID returns a sortable, lowercase ID: milliseconds since the epoch in
// base 36, then random base-36 digits. Agent names take its last 8 characters,
// so the random part keeps two runs started together apart.
func newRunID() string {
	b := make([]byte, 6)
	rand.Read(b)
	var n uint64
	for _, c := range b {
		n = n<<8 | uint64(c)
	}
	r := strconv.FormatUint(n, 36)
	r = strings.Repeat("0", 6-min(len(r), 6)) + r
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + r[len(r)-6:]
}
