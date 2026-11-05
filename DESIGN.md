# sheepdog

Scripted multi-agent workflows on top of [herdr](https://herdr.dev). A workflow is a small
TypeScript script that starts coding agents (Claude Code, Codex, OpenCode, Gemini, …) in herdr
panes, waits for them, and combines their structured results. You can watch every agent and
step in at any time, because each one runs in a real herdr pane.

## Architecture

```
 ┌──────────────┐   run / cancel / resume   ┌──────────────────────────┐
 │ workflow pane│ ────────────────────────▶ │ runner                   │
 │ (TUI plugin) │ ◀──── journal + events ── │  • script sandbox        │
 └──────────────┘                           │  • scheduler (limits)    │
 ┌──────────────┐                           │  • journal (SQLite)      │
 │ sheepdog CLI │ ────────────────────────▶ │  • backend interface     │
 └──────────────┘                           └────────────┬─────────────┘
                                                         │ start/prompt/wait/collect
                                            ┌────────────▼─────────────┐
                                            │ herdr backend            │──▶ herdr socket
                                            └──────────────────────────┘    (~/.config/herdr/herdr.sock)
```

**Runner.** Loads a workflow script and runs it. Each `agent()` call goes through the scheduler,
which enforces concurrency and spawn limits and then hands the call to a backend. In v1 the runner
lives inside the pane process. Later it can become a daemon that the pane and CLI both talk to.

**Journal.** SQLite, one row per agent call, keyed by
`hash(prompt, opts, resolved agent, model)`. It stores status, pane/agent name, worktree and the
validated result. Resume re-runs the script from the start: calls already in the journal return
their stored result immediately, and only new or changed calls start real agents. Both the pane and
the CLI render from the journal, never from runner memory.

**Backend interface.** It is small, so other backends (for example Rivet sandbox-agent for headless
cloud runs) can be added later:

```ts
interface Backend {
  start(spec: ResolvedAgent, opts: { cwd: string; name: string }): Promise<Handle>;
  prompt(h: Handle, text: string): Promise<void>;
  wait(h: Handle, opts: { timeoutMs: number; signal: AbortSignal }): Promise<"settled" | "blocked">;
  collect(h: Handle): Promise<unknown>;   // read + parse the result file
  stop(h: Handle): Promise<void>;
}
```

**herdr backend.** Drives herdr through its JSON CLI. One `agent()` call runs these steps:
1. `worktree create` when `isolation: "worktree"`; otherwise it uses a new pane in the run's workspace.
2. `agent start <name> --kind <kind> --pane <id>`.
3. `agent prompt` with the task plus a result contract: *"write JSON matching this schema to
   `$SHEEPDOG_RESULT`"*.
4. Wait (with a required timeout) for `idle`/`done` with an advanced `completion_seq`. Live status
   comes from `events.subscribe` (`pane.agent_status_changed`).
5. Validate the result file against the schema. If it is missing or invalid, send one correction
   prompt, then fail.
6. A `blocked` status pauses the call and is shown in the pane, so a human answers it.

The result file is the completion signal. An idle status alone is only a hint.

**Workflow pane.** A herdr plugin (`herdr-plugin.toml`) that opens a small TUI showing:
- the list of workflows
- the agent selector (see below)
- a live tree of phases and agents
- keys: enter to jump to an agent (`agent focus`), `r` run, `x` cancel, `R` resume

Each agent's workspace also reports `wf: <name> · <phase>` with `report-metadata`, so status shows in
herdr's normal sidebar.

## Scripting language

Plain TypeScript (ESM) with injected globals. It intentionally matches the Claude Code workflow API:
`agent`, `parallel`, `pipeline`, `phase`. The one addition is **agent selection**.

```ts
export const meta = {
  name: "review-and-verify",
  description: "Review the diff on two dimensions, verify each finding",
  phases: [{ title: "Review" }, { title: "Verify" }],
  // Roles → selectors. These are the defaults; the pane and CLI can override them per run.
  agents: {
    reviewer: "claude:opus | codex",   // first available wins
    verifier: "codex:gpt-5.4",
  },
};

const Findings = schema({ findings: [{ file: "string", line: "number", title: "string" }] });
const Verdict  = schema({ isReal: "boolean", reason: "string" });

const reviews = await pipeline(
  ["bugs", "perf"],
  (dim) => agent(`Review the current diff for ${dim} issues.`, {
    as: "reviewer", schema: Findings, phase: "Review", label: `review:${dim}`,
  }),
  (r) => parallel(r.findings.map((f) => () =>
    agent(`Adversarially verify: ${f.title} (${f.file}:${f.line})`, {
      as: "verifier", schema: Verdict, phase: "Verify", isolation: "worktree",
    }).then((v) => ({ ...f, verdict: v })),
  )),
);

return { confirmed: reviews.flat().filter((f) => f.verdict.isReal) };
```

### Primitives

| | |
|---|---|
| `agent(prompt, opts?)` | Run one agent. Returns its parsed result (or text, if no `schema`). |
| `parallel(thunks, { limit? })` | Run the functions concurrently. Rejects on the first failure; with `{ settle: true }` returns `{ ok, value }` / `{ ok: false, error }` per task. |
| `pipeline(items, ...stages)` | Each item flows through the stages independently, with no barrier between stages. |
| `phase(title)` | Groups the calls that follow for display. Usually you pass `phase` per call instead. |
| `schema(shape)` | Compact schema helper. A raw JSON Schema works too. |
| `args` | The run's input value (from the CLI or pane). |

`agent` options: `as` (role) or `use` (selector), `schema`, `label`, `phase`, `isolation`
(`"none" | "worktree"`), `cwd`, `timeout`. With neither `as` nor `use`, the `default` role is used
(`*` unless declared). Scripts can't use `import` yet; editor typings are in `types/sheepdog.d.ts`.

### Agent selector

A selector is a short string that resolves to one concrete agent before the call runs:

```
selector := choice ( "|" choice )*        // fallbacks, tried in order
choice   := kind [ ":" model ] [ "@" effort ]   |   "*"
```

Examples:
- `codex` — any Codex model
- `claude:opus@high` — Claude Opus at high effort
- `codex | opencode` — Codex if it is available, otherwise OpenCode
- `*` — any installed agent

"Available" means installed and allowed by the run's limits.

Selectors written in a script never name a space (see [Spaces](#spaces)). Only run-time overrides
and config can add a `space/` prefix, such as `box2/codex`.

There are three ways to choose:
1. **Roles in `meta.agents`.** The script uses `as: "reviewer"`, which keeps the script free of
   specific agents.
2. **Inline `use:`** for a one-off: `agent(p, { use: "gemini" })`.
3. **Per-run overrides.** These win over both of the above:
   - CLI: `sheepdog run review-and-verify --agent reviewer=codex --agent verifier=claude`
   - Pane: a selector row per role before you press run.

The resolved agent is part of the journal key. Changing a role's selector therefore re-runs only
that role's calls on resume.

### Steps and waits

Side effects and waiting on things outside the run (CI, deploys, review approvals) go through
journaled steps. Like `agent()`, they are keyed and stored in the journal, replayed on resume and
shown in the pane.

```ts
const pr = await step("open-pr", () => sh`gh pr create --fill`, { on: "ci" });

const run = await waitFor("ci", async () => {
  const r = JSON.parse(await sh`gh run list --branch ${branch} --limit 1 --json status,conclusion`)[0];
  return r?.status === "completed" ? r : undefined;   // undefined = keep waiting
}, { every: "30s", timeout: "45m", on: "ci" });
```

| | |
|---|---|
| `step(name, fn, { on? })` | Runs `fn` once and stores its return value. On resume it returns the stored value. |
| `waitFor(name, check, { every, timeout, on? })` | Calls `check` until it returns something other than `undefined`/`null`. The original start time is stored, so the timeout doesn't reset on resume. An unfinished wait starts polling again on resume. Three consecutive errors from `check` fail the wait. |
| `sleep(name, duration)` | Waits a fixed time. Journaled, so it survives a restart. |
| `` sh`…` `` | Runs a shell command in the step's space and returns stdout. Allowed only inside `step` and `waitFor`. |

Names are journal keys and must be unique within a run (use `` `ci:${i}` `` in loops). `on` names a
**space role** (see below), not a space.

### Rules
- Scripts must be deterministic. Avoid `Date.now()`, randomness and unordered iteration in prompts,
  or resume will re-run calls.
- Every agent call has a timeout (default 20 min). Run limits default to 6 concurrent agents and
  50 per run.
- Blocked agents are never answered automatically.

## Spaces

A space is a place where agents and steps run. One run has one runner and one journal, but its
calls can go to several spaces. **Spaces are chosen when a run starts, never in the script.**

**Defined in config**, `~/.config/sheepdog/config.json`:
```json
{
  "spaces": {
    "box2": { "backend": "herdr", "machine": "box2", "session": "sheepdog", "limit": 8 }
  }
}
```
`machine` becomes `herdr --machine box2` and `session` becomes `herdr --session sheepdog`. Two spaces
are always defined: `local` (this machine's herdr) and `simulate` (in-process fake agents that answer
with schema-shaped samples, for trying scripts without herdr or tokens).

For the herdr backend a space is `(machine, session)`. Every call for an agent passes the same
`--machine`/`--session`, because herdr IDs belong to one server. herdr never falls back to local,
so an unreachable space fails the call loudly.

**How a space is chosen**, highest priority first:
1. A per-role override: `--agent reviewer=box2/codex`, or `--space ci=box2` for step roles.
2. The run's default space: `--space box2` (also a picker in the pane).
3. The config default (`local`).

The resolved space is part of every journal key, so moving a role to another space re-runs only
that role's calls on resume.

**Remote steps.** A step whose space isn't local runs `sh` through that space's herdr: a hidden pane,
`pane run`, then `pane wait-output` for a sentinel line that carries the exit code. This reuses
herdr's machine config and lets you watch the step. Plain SSH is the fallback.

**Code state across spaces.** Results are JSON, so they move between spaces for free. Files don't.
A worktree call returns `{ branch, commit }`, and a later call can take `checkout: ref`. The target
space then fetches that ref from a shared remote before starting.

## v1 scope
Runner and herdr backend, journal plus resume, the selector, `step`/`waitFor`/`sleep`/`sh`, a CLI
(`run` / `resume` / `ls`), and a minimal pane. Spaces are built into the design (config, run-time
resolution, space in the journal key), but v1 is tested only against `local`. Out of scope for v1:
daemon mode, non-herdr backends, nested workflows, webhook-triggered waits.
