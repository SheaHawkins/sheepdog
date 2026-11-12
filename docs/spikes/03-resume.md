# Spike 3: kill a supervisor mid-run and resume it from the journal

Question: if a supervisor dies partway through a run, can a new one pick up from the journal at the first unfinished call, without re-running finished calls or sending a prompt twice?

Answer: yes. Two real-herdr tests pass with claude. One kills the supervisor with SIGKILL while its agent is mid turn. The other stops the whole herdr server mid turn and starts it again. In both, the new supervisor replays call 1 from the journal, reuses the existing agent with its conversation intact, finishes call 2 and runs call 3. Replay is the easy part. The hard part is the call that was in flight when the supervisor died.

## What was built

- `internal/run`, the supervisor core:
  - `Execute(ctx, Config)` starts or resumes a run. The same config and directory do both.
  - `Journal` writes one JSONL line per completed call and fsyncs it before the script sees the result.
  - A lock file stops two supervisors from driving one run.
  - `run.json` is written by temp file and rename. `events.jsonl` is what `run watch` will tail.
- `script.AgentOptions.Seq`: each call's position, numbered in the order the script makes its calls, not the order they finish.
- `node.Executor` additions:
  - It finds and reuses a live agent instead of starting a new one.
  - It recovers a call that a previous supervisor started.
  - It learns a label's kind from the journal.
- Tests:
  - Unit tests cover replay, `parallel()` replay, a changed graph, torn and damaged journals, the lock, and every way a resumed call can find its agent.
  - `TestKillAndResume` and `TestHerdrRestartAndResume` run against real herdr. The supervisor runs as a child process (the test binary, run again), so it can be killed for real.

| Test | What dies | Resume took | Outcome |
| --- | --- | --- | --- |
| `TestKillAndResume` | Supervisor, SIGKILL, mid turn | 14s | Waited out the turn, took its result file, no resend |
| `TestHerdrRestartAndResume` | herdr server, mid turn | 25s | Agent restored with its conversation; call 2 sent again, then asked once for the file |

## Findings

**Positions come from call order, which is deterministic even with `parallel()`.** Calls reach the host from separate goroutines, so they arrive in no fixed order. The runtime numbers each call on the JS goroutine as the script makes it, and passes the number to the host. A replay numbers its calls the same way, so a journal entry for call 3 matches call 3 even when the `parallel()` calls finished in a different order. A test checks this with calls that finish in reverse order.

**The journal records results, and the node directory records progress within a call.** The journal only knows completed calls. For the call in flight, the new supervisor reads the node directory. Every prompt is written to `prompt-N.md` before it is sent, and the result file is deleted before each send, so:

| Found on resume | Meaning | Action |
| --- | --- | --- |
| Agent working or blocked | A turn is under way, maybe ours | Wait for it to end, then check again |
| Result file present | The turn finished; only the journal write was lost | Use it; no prompt sent |
| No file, and the result path is in the agent's history | Delivered; the turn ended without a result | Ask again for the file, as attempt N+1 |
| No file, and the path is not in the history | Never delivered | Send the last prompt |
| History unreadable | Unknown | Fail the call; never guess |

The result path doubles as a delivery marker, because it is unique to the call and appears in the prompt. Agent UIs wrap long lines, so the check compares with all whitespace removed.

**Agent names made reuse free.** Spike 2 derived names from the run ID and label, so the new supervisor knows which agent belongs to which label, and `agent get` finds it. It reuses the pane and the conversation. The spec's "store agent names, not pane IDs" holds up.

**A herdr restart restores claude agents with their names, panes and conversations.** After `herdr server stop` and a fresh `herdr server`, each claude agent came back under its old name and pane ID. Asked what it had answered before, a restored agent replied "ONE, TWO, THREE". Codex agents were not restored; their panes came back as plain shells. A resumed codex label therefore gets a new agent with no memory of earlier calls. The run still finishes, but a review cycle loses its context.

**Right after a restart, a restored agent's name disappears for a moment.** The agent showed up `idle`, then `agent read` returned `agent_not_found`, then the name was back. herdr is probably relaunching the agent into its old conversation. Without handling, resume either failed the call or, worse, started a second agent for the label. The executor now retries `agent_not_found` for 30 seconds for any agent it knows existed: one with a journal entry, or one with a prompt file for the call in flight.

**After a restart, the history check is unreliable, but resending is still correct.** In the restart test, the delivery check did not find call 2's prompt in the restored history, though the prompt had been delivered. Restored history is partial, and the read raced the relaunch. So the prompt went out again. That was the right outcome: a restart kills the agent process, so the interrupted turn could never finish. A turn that did finish leaves its result file, which is checked first.

**Turns can end with work still running, so the plain-text fallback was too eager.** In the first version of the kill test, claude refused to `sleep` in the foreground. It ran the command in a background shell and ended its turn, promising to write the result file when the sleep finished. herdr reported `idle` with the work still going. The executor then read the pane at once and returned the whole screen as the result. Plain-text calls now ask again for the file, as schema calls do, and read the pane only after the retries run out. The fallback still returns the entire screen, not an answer, so it is a last resort.

**Smaller decisions:**

- **Failed calls are not journaled.** A resume retries them. If the script caught an error and took another branch, the retry may succeed. The replay then diverges, and the mismatch check fails the run with a clear message instead of guessing.
- **A mismatch is fatal even if the script catches it.** The host cancels the run's context with the mismatch as the cause.
- **A torn last journal line is dropped.** It was never acknowledged, so the script never saw that result. Damage anywhere else stops the resume.
- **The lock holds the supervisor's PID.** A lock whose process is gone is taken over. A resume racing another resume is settled by `O_EXCL` on the retry, though a narrow window remains, noted in the code. The liveness check uses `kill -0` on Unix and `OpenProcess` on Windows.
- **After a SIGKILL, `run.json` still says `running` with a dead PID.** That is how `runs` and `loops list` can spot an orphaned run: status `running` with no live supervisor. The CLI does not do this yet.
- **The journal stores a SHA-256 of each prompt, not the prompt itself.** The node directory already keeps the full text.

## Spec changes

- Resume: describe the in-flight call recovery, the lock, and that failed calls are retried.
- Results from files: ask again before falling back to the pane, for plain-text calls too.
- Risks: herdr restores claude agents but not codex; the supervisor itself still has to be resumed.

## Not covered

- Restarting the supervisor automatically. After a herdr restart, the supervisor's own pane comes back as a plain shell, so a run waits for `run resume`. Loops will need this.
- Resume after a crash of the machine itself, where agent processes die along with the supervisor. This resembles the herdr restart case without herdr restoring the agents.
- `graph()` child runs and their pinned versions.
- Codex resume beyond noting that its agents are not restored. Its pane does expose a session ID, which a later version could use to relaunch the agent into its old conversation.
