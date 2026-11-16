# Spike 5: sheepdog as a herdr plugin

Questions:

1. Does herdr launch `bin/sheepdog`, a path relative to the plugin, on every OS?
2. Do pane IDs survive a herdr restart, so a supervisor can be restarted in its own pane?
3. Can a startup hook tell which herdr session it runs in?

Then: build the plugin pane that shows a run.

Answers: yes to all three, with details below. The pane shows a live run timeline, as chosen from three mocks: every `agent()` call in order, `parallel()` calls side by side, each loop round below the last.

## Findings

**herdr resolves relative commands against the plugin directory, on all three OSes.** A command of `["bin/sheepdog", ...]` ran as `<plugin root>/bin/sheepdog`, with the plugin root as its working directory. On Windows it ran `bin\sheepdog.exe` with no `.exe` in the manifest. A CI job links a test plugin on Linux, macOS and Windows and runs it as a startup hook and as an action.

**herdr sets `HERDR_SESSION`, though its docs do not mention it.** It holds the session name in startup hooks, actions, plugin panes and ordinary panes, and it is empty in the default session. The socket path also carries the name (`…/sessions/<name>/herdr.sock`, against `…/herdr/herdr.sock` for the default session), as a fallback. So the startup hook can keep only its own session's runs.

**Every herdr pane sets `HERDR_PANE_ID`, `HERDR_SESSION` and `HERDR_BIN_PATH`.** `sheepdog graph run` typed into a herdr pane now splits its agents beside that pane in the right session, with no `--session` flag and no new workspace.

**Actions get the focused pane's directory directly.** `HERDR_PLUGIN_CONTEXT_JSON` includes `focused_pane_cwd` and `workspace_cwd`, so finding the project needs no extra `herdr pane get`. One catch: when the focused pane is a plugin pane, its directory is the plugin's own, so sheepdog skips any directory inside the plugin root.

**Pane IDs and labels survive a server restart, but every pane comes back as a bare shell.** The plugin's own pane is no exception. After a restart, the supervisor's pane and the sheepdog pane both show a shell prompt. The startup hook ran again on restart, as documented, so it can relaunch both. This is what users would see without it:

```
 · myproject             ┌──────────────────────────┐┌ Probe pane ─────────────────┐
   main                  │proj %                    ││probe-plugin %               │
                         └──────────────────────────┘│                             │
                         ┌ supervisor ──────────────┐│                             │
                         │proj %                    ││                             │
```

**Plugin panes get the opener's context.** A pane opened by an action receives the context of the pane that was focused when the action ran, so the sheepdog pane knows which project to show. Quitting the pane's program closes the pane.

**Smaller observations:**

- `herdr plugin link` works with no server running. `herdr plugin unlink` needs one, and fails with `server_not_running` otherwise.
- Plugin state lives in `~/.local/state/herdr/plugins/<id>` and config in `~/.config/herdr/plugins/config/<id>` on macOS.
- `herdr plugin log list` keeps each command's stdout and stderr, which CI uses to check results.
- A herdr client attached inside a pane of another session renders the full herdr screen into that pane. That is how the screens in this doc were captured.

## The run timeline pane

`sheepdog plugin watch` runs in the pane, and `sheepdog run watch [RUN_ID]` shows the same view in any terminal. It reads the run directory once a second and asks herdr for the status of each agent with a call in flight.

- The supervisor's event log now has structured call events (`call_started`, `call_done`, `call_failed`) with the call's position, label, kind and exec ID, so viewers don't parse messages.
- Calls whose run times overlap form a `parallel()` group. They are drawn side by side when they fit, and as a bracketed list when they don't.
- A label's second and later calls are numbered (`implement #2`). Done calls show their duration and a short summary of the result. Running calls show herdr's live status: working, or waiting on you.
- Keys: <kbd>↑</kbd><kbd>↓</kbd> select a call, <kbd>enter</kbd> focuses its agent in herdr, <kbd>←</kbd><kbd>→</kbd> switch runs, <kbd>q</kbd> closes the pane. The pane moves to a new run when one starts, unless the user has picked another.
- `run.json` now records the graph's name and the run's start time.

## Demo: a real run, as a user sees it

The plugin was linked, the sheepdog pane opened from its action, and `examples/review-cycle.ts` typed into the project's pane. The run used four claude agents over two review rounds and took 1m11s. The pane, mid-run and at the end:

```
 review-cycle  run mwjn763b  ● running  33s      review-cycle  run mwjn763b  ✓ succeeded  1m11s

  ✓ implement    claude     11s  done              ✓ implement       claude     11s  done
  │                                                │
  ✓ review       claude     14s  2 findings        ✓ review          claude     14s  2 findings
  ├───────────────┐                                ├─────────────────┐
  ◐ validate-F1   ◐ validate-F2                    ✓ validate-F1     ✓ validate-F2
  │ working 8s    │ working 8s                     │ isReal: true    │ isReal: false
  ├───────────────┘                                ├─────────────────┘
                                                   · round 1: 1 confirmed
                                                   │
                                                   ✓ implement #2    claude      7s  fixed
                                                   │
                                                   ✓ review #2       claude     10s  2 findings
                                                   ├─────────────────┐
                                                   ✓ validate-F1 #2  ✓ validate-F2 #2
                                                   │ isReal: false   │ isReal: false
                                                   ├─────────────────┘
                                                   · round 2: 0 confirmed
```

The whole herdr screen mid-run. Left to right: herdr's sidebar with the four agents, the project pane where the run was typed, the agents stacked beside it, and the sheepdog pane:

```
 ● myproject             ┌──────────────────┐┌ implement ──────────┐┌ sheepdog ─────────────────────────────────┐
   main                  │run mwjn763bc8xz2z││  planted mistake: it││ review-cycle  run mwjn763b  ● running  33s│
                         │                  ││  says sheepdogs     ││                                           │
                         │                  ││  were first         ││  ✓ implement    claude     11s  done      │
                         │                  ││  domesticated on the││  │                                        │
                         │                  │└─────────────────────┘│  ✓ review       claude     14s  2 findings│
 agents           grouped│                  │┌ review ─────────────┐│  ├───────────────┐                        │
 ○ myproject             │                  ││✻ Sautéed for 9s     ││  ◐ validate-F1   ◐ validate-F2            │
   s3bc8xz2z-implement-b…│                  │└─────────────────────┘│  │ working 8s    │ working 8s             │
 ○ myproject             │                  │┌ validate-F1 ────────┐│  ├───────────────┘                        │
   s3bc8xz2z-review-d6dc…│                  ││✳ Frosting… (4s)     ││                                           │
 ● myproject             │                  │└─────────────────────┘│                                           │
   s3bc8xz2z-validate-f1…│                  │┌ validate-F2 ────────┐│                                           │
 ● myproject             │                  ││  ◐ medium · /effo…  ││ ↑↓ select · enter go to agent · ←→ runs · │
   s3bc8xz2z-validate-f2…└──────────────────┘└─────────────────────┘└───────────────────────────────────────────┘
```

Watching real runs found problems that tests had not:

- **The pane did not notice a new run.** It loaded the run list once, so it kept showing the previous run while a new one worked beside it. It now reloads every second, and moves to a new run unless the user has picked one.
- **Every new agent raised a false alarm.** For their first seconds, new claude agents showed `? needs a look`, because herdr reports `unknown` while claude draws its startup screen. The pane now says "starting" for the first 30 seconds of a call.
- **Parallel columns jumped to a list when results came in.** The summaries were too wide and repeated the label (`id: F1` under `validate-F1`). Fields that repeat the label are now left out, and the width check measures what the columns draw. The columns stay put, down to a 40-column pane.
- **The rounds were hard to read.** The script's own `log()` lines now appear in place (`· round 1: 1 confirmed`), which shows where each round ends.
- **A long footer wrapped and scrolled the header off the top.** Every line is now clipped to the pane width.

Still open:

- **The pane is squeezed in a busy workspace.** herdr splits a `split` plugin pane off the focused pane, so a pane opened late in a crowded workspace can be 20 columns wide. Opening it as its own tab, or as a wide popup, would avoid that.
- **Long runs scroll.** The outline view, which draws the script's shape with past, present and future calls, is planned next. This timeline becomes its fallback.
- **The startup hook that resumes runs after a restart isn't built yet.** This spike confirmed it can work: the hook runs on every restart, knows its session, and pane IDs survive.
