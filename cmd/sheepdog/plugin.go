package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// pluginEnv is what herdr tells a plugin command about where it was invoked.
// herdr sets these for actions, panes and startup hooks; see
// docs/spikes/05-herdr-plugin.md for what each kind of command receives.
type pluginEnv struct {
	Session    string `json:"session"`     // HERDR_SESSION; empty in the default session
	Bin        string `json:"herdr_bin"`   // HERDR_BIN_PATH
	Socket     string `json:"socket"`      // HERDR_SOCKET_PATH
	PluginRoot string `json:"plugin_root"` // HERDR_PLUGIN_ROOT
	StateDir   string `json:"state_dir"`   // HERDR_PLUGIN_STATE_DIR
	Event      string `json:"event,omitempty"`
	Pane       string `json:"pane,omitempty"` // HERDR_PANE_ID: the action's focused pane, or a plugin pane's own
	Context    struct {
		Source       string `json:"invocation_source"`
		FocusedPane  string `json:"focused_pane_id"`
		FocusedCwd   string `json:"focused_pane_cwd"`
		WorkspaceCwd string `json:"workspace_cwd"`
	} `json:"context"`
	Project string `json:"project"` // resolved project directory
}

func readPluginEnv() (*pluginEnv, error) {
	e := &pluginEnv{
		Session:    os.Getenv("HERDR_SESSION"),
		Bin:        os.Getenv("HERDR_BIN_PATH"),
		Socket:     os.Getenv("HERDR_SOCKET_PATH"),
		PluginRoot: os.Getenv("HERDR_PLUGIN_ROOT"),
		StateDir:   os.Getenv("HERDR_PLUGIN_STATE_DIR"),
		Event:      os.Getenv("HERDR_PLUGIN_EVENT"),
		Pane:       os.Getenv("HERDR_PANE_ID"),
	}
	if e.PluginRoot == "" {
		return nil, errors.New("not running as a herdr plugin command (HERDR_PLUGIN_ROOT is unset)")
	}
	if raw := os.Getenv("HERDR_PLUGIN_CONTEXT_JSON"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &e.Context); err != nil {
			return nil, fmt.Errorf("HERDR_PLUGIN_CONTEXT_JSON: %w", err)
		}
	}
	// Plugin commands run in the plugin's own directory, so the project comes
	// from the pane the user was in. A focused plugin pane reports the plugin
	// directory as its cwd, which is never the project.
	for _, dir := range []string{e.Context.FocusedCwd, e.Context.WorkspaceCwd} {
		if dir != "" && !within(dir, e.PluginRoot) {
			e.Project = projectRoot(dir)
			break
		}
	}
	return e, nil
}

func within(dir, root string) bool {
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// projectRoot walks up from dir to the nearest directory holding .sheepdog/,
// else the nearest git root, else dir itself.
func projectRoot(dir string) string {
	gitRoot := ""
	for d := dir; ; d = filepath.Dir(d) {
		if fi, err := os.Stat(filepath.Join(d, ".sheepdog")); err == nil && fi.IsDir() {
			return d
		}
		if gitRoot == "" {
			if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
				gitRoot = d
			}
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	if gitRoot != "" {
		return gitRoot
	}
	return dir
}

// pluginCmd is `sheepdog plugin ...`, the binary's entry point for herdr.
// It is not in the usage text: people do not type these.
func pluginCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("unknown plugin command %q", args)
	}
	e, err := readPluginEnv()
	if err != nil {
		return err
	}
	switch args[0] {
	case "doctor":
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(e)
	case "runs":
		// The action opens the timeline pane; the pane gets the same
		// context, so it shows the project the user was in.
		pluginID := os.Getenv("HERDR_PLUGIN_ID")
		return herdrClient("").Exec(ctx, "plugin", "pane", "open", "--plugin", pluginID, "--entrypoint", "watch")
	case "watch":
		if e.Project == "" {
			return errors.New("no project: open this pane from a pane in your project")
		}
		return watch(ctx, e.Project, "", herdrClient(""), os.Stdin, stdout)
	}
	return fmt.Errorf("unknown plugin command %q", args[0])
}
