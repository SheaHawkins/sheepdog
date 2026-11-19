# sheepdog

sheepdog runs TypeScript graphs of coding agents in [herdr](https://github.com/ogulcancelik/herdr). A graph is a script that calls `agent()` for each step. Every call is a real Claude Code or Codex agent in its own herdr pane. Runs are journaled, so a run that stops can resume where it left off.

As a herdr plugin, sheepdog adds a pane that shows a run as it happens:

```
 review-cycle  run mwjn763b  ✓ succeeded  1m11s

  ✓ implement       claude     11s  done
  │
  ✓ review          claude     14s  2 findings
  ├─────────────────┐
  ✓ validate-F1     ✓ validate-F2
  │ isReal: true    │ isReal: false
  ├─────────────────┘
  · round 1: 1 confirmed
  │
  ✓ implement #2    claude      7s  fixed
```

This is early work. [SPEC.md](SPEC.md) has the design, and [docs/spikes](docs/spikes) has what each spike found.

## Requirements

- herdr 0.9.5 or later
- Go 1.26 or later, to build sheepdog
- `claude` (Claude Code) or `codex` installed and logged in, depending on which agents a graph uses. The examples use `claude`.

## Set up

Build the binary and link this checkout into herdr. You only need to do this once:

```sh
git clone https://github.com/SheaHawkins/sheepdog.git ~/code/sheepdog
cd ~/code/sheepdog
go build -o bin/sheepdog ./cmd/sheepdog
herdr plugin link "$PWD"
```

The link points at the checkout. After you pull changes, run `go build` again; there's no need to relink.

## Try it

1. Make a scratch project. The example graph writes a `notes.md` file there:

   ```sh
   mkdir -p ~/scratch/sheepdog-demo && cd ~/scratch/sheepdog-demo && git init
   ```

2. In herdr (the terminal or the app), open a pane and `cd ~/scratch/sheepdog-demo`.

3. Open the sheepdog pane:

   ```sh
   herdr plugin action invoke sheepdog.runs
   ```

   It opens beside the current pane and shows the project's runs, which is none at first. sheepdog picks the project from the focused pane's directory.

4. Start a run from the same pane:

   ```sh
   ~/code/sheepdog/bin/sheepdog graph run ~/code/sheepdog/examples/review-cycle.ts "sheepdogs, including one clear factual mistake"
   ```

   The agents open in panes beside it. The graph writes notes, reviews them, checks each finding in parallel and fixes what holds up, for up to three rounds. The sheepdog pane switches to the new run and updates every second. For now, the pane you started the run from stays busy until the run finishes. When it does, it prints the graph's result as JSON.

### Keys in the sheepdog pane

| Key | Does |
| --- | --- |
| <kbd>↑</kbd> <kbd>↓</kbd> | select a call |
| <kbd>enter</kbd> | jump to that call's agent |
| <kbd>←</kbd> <kbd>→</kbd> | older and newer runs |
| <kbd>q</kbd> | close the pane |

To open the pane with a key, add this to `~/.config/herdr/config.toml`:

```toml
[[keys.command]]
key = "prefix+g"
type = "plugin_action"
command = "sheepdog.runs"
description = "sheepdog runs"
```

### Things you might hit

- **"waiting on you".** An agent needs input. Often it's Claude asking whether to trust a folder it hasn't seen before. Select the call, press <kbd>enter</kbd> to jump to it, and answer the prompt. The run then carries on.
- **The pane shows another folder's runs.** The pane that had focus when you opened it wasn't in the project. Close it with <kbd>q</kbd>, focus a pane in the project, and open it again.
- **The pane is narrow.** It opens as a split off the focused pane, so a crowded workspace squeezes it. Open it from a wide pane.

## Without the plugin

The sheepdog binary also works in any terminal:

```sh
sheepdog graph run FILE.ts ["instruction"] [-i KEY=VALUE]...   # run a graph
sheepdog run watch [RUN_ID]                                    # the same timeline, in this terminal
sheepdog run resume RUN_ID                                     # carry on a stopped run
```

Runs live in `.sheepdog/runs/` under the current directory. Inside herdr, the agents open beside the current pane. Outside herdr, sheepdog makes a new herdr workspace for them; pass `--session NAME` to pick the session.

## Writing a graph

A graph is a TypeScript file. `args.instruction` is the text after the file name, and `agent()` sends one prompt to a labelled agent. Two calls with the same label talk to the same agent. Pass a JSON schema to get a structured result back:

```ts
export const meta = { name: 'hello' }

const a = await agent<string>(`Answer with only the word ${args.instruction}.`, { label: 'worker', kind: 'claude' })
const b = await agent<{ word: string }>('What word did you just answer? Reply in the result file.', {
  label: 'worker',
  schema: { type: 'object', properties: { word: { type: 'string' } }, required: ['word'] },
})
return { a, b }
```

`parallel()` runs calls at the same time, and `log()` adds a line to the timeline. See [examples/](examples) and [SPEC.md](SPEC.md) for more.

## Remove the plugin

```sh
herdr plugin unlink sheepdog
```

A herdr server must be running for this to work.

## Development

```sh
go test ./...                 # unit tests and a smoke test with a fake herdr
go test -short ./...          # skip the smoke test, which builds binaries
```

CI runs the tests on Linux, macOS and Windows, and links a test plugin into a real herdr on each. Commits follow [Conventional Commits](https://www.conventionalcommits.org/), checked by cocogitto.
