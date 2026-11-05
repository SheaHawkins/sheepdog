# sheepdog

Scripted multi-agent workflows on top of [herdr](https://herdr.dev). Write a small TypeScript
script; sheepdog starts Claude Code, Codex, OpenCode and other agents in herdr panes, waits for
their structured results, and records every call in a journal so runs can be resumed.
See [DESIGN.md](DESIGN.md) for the architecture.

Requires Node ≥ 24 (it runs the `.ts` sources directly; there are no dependencies) and herdr for
real agents.

```sh
npm link                        # puts `sheepdog` on PATH
sheepdog ls                     # workflows: .sheepdog/workflows (repo), ~/.config/sheepdog/workflows, built-in
sheepdog run review-and-verify  # live view: ↑↓ select, enter jump to agent, x cancel, q close view
sheepdog run review-and-verify --agent reviewer=codex --space verifier=box2
sheepdog run ./scratch.ts --space simulate   # fake agents: try a script without spending tokens
sheepdog runs                   # recent runs
sheepdog resume <run-id>        # finished calls replay from the journal; edited calls re-run
sheepdog watch <run-id> | show <run-id> [--json] | cancel <run-id>
```

## A workflow

```ts
/// <reference path="/path/to/sheepdog/types/sheepdog.d.ts" />
export const meta = {
  name: "fix-ci",
  agents: { fixer: "codex | claude" },      // roles → selectors; override per run with --agent
};

const branch = await step("branch", () => sh`git branch --show-current`);
const run = await waitFor("ci", async () => {
  const r = JSON.parse(await sh`gh run list --branch ${branch} --limit 1 --json status,conclusion`)[0];
  return r?.status === "completed" ? r : undefined;
}, { every: "30s", timeout: "45m" });

if (run.conclusion !== "success") {
  return await agent(`CI failed on ${branch}. Read the logs with gh, fix it, and push.`, {
    as: "fixer",
    schema: schema({ fixed: "boolean", summary: "string" }),
  });
}
return { fixed: false, summary: "CI is green" };
```

Primitives: `agent`, `parallel`, `pipeline`, `phase`, `schema`, `step`, `waitFor`, `sleep`, `sh`,
`args`. Selectors: `kind[:model][@effort]`, `|` for fallbacks, `*` for any installed agent.

## Configuration

`~/.config/sheepdog/config.json` (all optional):

```json
{
  "defaultSpace": "local",
  "spaces": {
    "box2": { "backend": "herdr", "machine": "box2", "session": "sheepdog", "limit": 8 }
  },
  "agents": { "claude": { "args": ["--permission-mode", "acceptEdits"] } },
  "limits": { "concurrency": 6, "maxAgents": 50, "agentTimeout": "20m" }
}
```

`local` (herdr on this machine) and `simulate` (fake agents) are always defined. State lives in
`~/.local/state/sheepdog/` (`SHEEPDOG_STATE_DIR`, `SHEEPDOG_CONFIG_DIR` override).

## Development

```sh
npm test
```

## Status

The runner, journal, selectors, steps and CLI are tested against the simulate backend. The herdr
backend follows herdr 0.9.x's documented CLI but hasn't been run against a live herdr yet; field
names in herdr's JSON output are read defensively (`src/backends/herdr.ts`).
