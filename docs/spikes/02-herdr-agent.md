# Spike 2: one agent end to end through the herdr CLI

Question: can the supervisor drive an agent with the herdr CLI (start, prompt, wait, result file) reliably enough for graphs to branch on the results?

Answer: yes, with three changes to the spec. claude and codex both completed two-turn calls through the Go code against herdr 0.9.5. The second turn reached the same agent with its context intact. Agent names, startup dialogs and turn tracking each need handling that the spec does not describe yet.

## What was built

- `internal/herdr`: the `Client` interface and `CLI`, which runs `herdr` and decodes its JSON. Server errors come back as `*herdr.Error` with herdr's code (`timeout`, `agent_prompt_stalled`, `agent_blocked`, `agent_not_ready`, …).
- `internal/node`: `Executor.Do(ctx, Call)`, the logic that will sit behind `Host.Agent`. On a label's first call it splits a pane and starts the agent. Then it sends the prompt with a result-file instruction, waits for the turn to end, and reads and validates the file, asking again up to twice with the validation error.
- Unit tests against a fake herdr cover the retry, the pane fallback, blocked agents, timeouts, stale result files and startup dialogs. `TestHerdrEndToEnd` drives a real agent when `SHEEPDOG_HERDR_SESSION` names a running herdr session.

| Kind | Turn 1 (incl. start) | Turn 2 | Notes |
| --- | --- | --- | --- |
| claude | 9.4s | 4.2s | Valid JSON on the first attempt |
| codex | 22s | 8.5s | After the startup-dialog fix, with a person answering two dialogs. Before it, the first prompt was lost and the call needed a retry (27–43s) |

## Findings

**Agent names cannot be `<run>-<label>`.** herdr names are 1–32 characters of `[a-z0-9_-]`, start with a letter, and are unique across the whole server. A label like `security-review/security` is rejected, and a run ID plus a long label overflows. `AgentName` builds `s<run tail>-<label slug>-<hash>`, at most 31 characters. The hash covers the exact run ID and label, so `a/b` and `a-b` never collide. The label goes on the pane title with `pane rename`, so the herdr UI still shows it.

**`agent start` needs a pane; it does not create one.** The spec's `herdr agent start <name> --kind <kind>` is missing `--pane`. The supervisor splits a pane first, from its own pane, then starts the agent in it. The spike splits the first agent to the right and later ones down. A run with many validators will get cramped rows, so layout needs a decision before v1 (more tabs, or a grid).

**Startup dialogs: herdr flags some, misses others, and a missed one is dangerous.**

- claude's folder trust prompt and codex's update notice are reported as `blocked`, and `agent start` returns `agent_not_ready`. The executor notifies and waits for a person.
- codex's folder trust dialog is reported as `idle` (once as `unknown`). `agent start` succeeds, and the first prompt is typed into the dialog. Its Enter selects **Trust and continue**, and the prompt text is lost. This happened twice in testing: sheepdog trusted folders on the user's behalf, then only recovered because the retry asked again.
- codex's update notice defaults to **Update now**, which runs `curl … | sh`. A stray Enter there runs an installer.

The executor now refuses to type into a screen herdr reports as `unknown`. After start, it also checks the visible screen for known dialog text per kind (`StartupDialogs`) and polls until a person has answered. That is brittle string matching. The real fix is in herdr's codex detection rules, and it should be reported upstream.

In practice graphs run in a folder the user already trusts, so trust dialogs are rare in v1. They become routine in v2: a fresh worktree per run is a new folder every time.

**Turn tracking: use `state_change_seq`.** Every agent carries `state_change_seq`, a counter that only grows (it is server-wide, not per agent). The executor reads it before prompting and accepts a settled state only once the counter has moved past that value. This answers the risk "a wait can return on an earlier turn's completion". The CLI cannot wait for a seq above N, so the executor loops over `agent wait`.

**Delivery checks read state, not the screen.** herdr refuses long reads while an agent is working (`agent_not_idle`), so "read the pane before retrying" does not work mid-turn. After a `timeout` or `agent_prompt_stalled`, the executor reads `agent get`: a higher seq proves the prompt landed, so it keeps waiting. An unchanged seq proves nothing, so it fails the call rather than send the prompt twice. `agent_blocked` is the one rejection that guarantees nothing was sent, so that case alone is retried.

**Blocked mid-turn works as the spec says.** In claude's default permission mode, a shell command makes `prompt --wait` return `blocked`. Waiting `--until working --until idle --until done` returns once a person answers, then the turn finishes normally. A standalone `agent wait` right after keys are sent can return the old state immediately; the seq check covers this as well.

**Result files work.** Both agents wrote valid JSON to an absolute path outside their working directory. The retry prompt that includes the validation error worked the one time it was needed. The executor deletes the result file before every attempt, so a file left behind can never be read as a new answer. Codex appended a newline to a plain-text answer. Callers may want to trim, but the spike leaves results exactly as written.

**Permission mode decides how often runs block.** With the user's auto mode, claude ran commands without asking. Started with `-- --permission-mode default`, it blocked on its first shell command. `Executor.AgentArgs` passes native arguments per kind. Graphs probably want this per node, which suggests an `args` option on `agent()` or a profile.

**Smaller observations:**

- `agent read` prints plain text, not JSON like the other commands.
- Notifications were `disabled` in this herdr config (`shown: false`). A blocked run must also show in `run watch` and `runs`, and not rely on the notification alone.
- Panes report the agent's own session ID (`agent_session.value`, the claude conversation UUID). Spike 3 could use it to reattach a conversation after a herdr restart instead of starting a fresh agent.
- `herdr --session NAME server` gives an isolated headless server, which is how the integration test runs without touching the user's main session.

## Spec changes

- Agent names: derived as above, not `<run>-<label>`, with the label on the pane title.
- Lifecycle step 3: split a pane, then `agent start --pane`.
- Blocked agents: include startup dialogs and the `unknown` state; never type into either.
- Delivery check: read the state counter, since the pane cannot be read mid-turn.

## Not covered

- Steering (`node steer`) and prompts sent while a person is typing in the pane.
- Agents other than claude and codex.
- Very long prompts. On macOS, herdr rejects `pane run` commands over the terminal's line limit; `agent prompt` uses bracketed paste and was not tested at large sizes.
- Pane layout for wide `parallel()` calls.
