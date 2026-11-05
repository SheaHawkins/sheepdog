import { join } from "node:path";
import { parseArgs } from "node:util";
import type { Backend } from "./backend.ts";
import { HerdrBackend } from "./backends/herdr.ts";
import { SimulateBackend } from "./backends/simulate.ts";
import { loadConfig, stateDir, type Config } from "./config.ts";
import { Journal, type Overrides } from "./journal.ts";
import { emptyOverrides, runWorkflow } from "./runner.ts";
import { loadScript } from "./script.ts";
import { parseSelector } from "./selector.ts";
import { formatDuration } from "./util.ts";
import { LiveView, render } from "./view.ts";
import { findWorkflow, listWorkflows } from "./workflows.ts";

const HELP = `sheepdog — scripted multi-agent workflows on herdr

usage:
  sheepdog run <workflow|file.ts> [options]   start a run
  sheepdog resume <run-id> [options]          resume a run (finished calls replay from the journal)
  sheepdog ls                                 list workflows (repo, user, built-in)
  sheepdog runs                               list recent runs
  sheepdog show <run-id> [--json]             show a run's calls and result
  sheepdog watch <run-id>                     live view of a run started elsewhere
  sheepdog cancel <run-id>                    ask a running run to stop

run / resume options:
  --args <json>            input value for the script's \`args\`
  --arg key=value          set one field of \`args\` (value parsed as JSON when possible)
  --agent role=selector    choose the agent for a role, e.g. reviewer=codex or verifier=box2/claude:opus
  --space name             default space for this run
  --space role=name        space for one agent or step role
  --no-view                plain log output instead of the live view

selectors: kind[:model][@effort], fallbacks with "|", "*" for any installed agent
`;

function backendFactory(config: Config): (space: string) => Backend {
  const cache = new Map<string, Backend>();
  return (space: string) => {
    let b = cache.get(space);
    if (b) return b;
    const s = config.spaces[space];
    if (!s) throw new Error(`unknown space "${space}"`);
    switch (s.backend ?? "herdr") {
      case "herdr":
        b = new HerdrBackend({
          machine: s.machine,
          session: s.session,
          agents: config.agents,
          worktreeRoot: join(stateDir(), "worktrees"),
        });
        break;
      case "simulate":
        b = new SimulateBackend();
        break;
      default:
        throw new Error(`space "${space}": unknown backend "${s.backend}"`);
    }
    cache.set(space, b);
    return b;
  };
}

function parseRunFlags(values: Record<string, unknown>, base?: { args: unknown; overrides: Overrides }) {
  let args: unknown = base?.args ?? null;
  if (typeof values.args === "string") {
    try {
      args = JSON.parse(values.args);
    } catch (e) {
      throw new Error(`--args must be JSON: ${(e as Error).message}`);
    }
  }
  for (const kv of (values.arg as string[] | undefined) ?? []) {
    const i = kv.indexOf("=");
    if (i <= 0) throw new Error(`--arg expects key=value, got "${kv}"`);
    const k = kv.slice(0, i);
    const raw = kv.slice(i + 1);
    let v: unknown = raw;
    try {
      v = JSON.parse(raw);
    } catch {
      /* plain string */
    }
    args = { ...(args && typeof args === "object" && !Array.isArray(args) ? args : {}), [k]: v };
  }

  const ov: Overrides = structuredClone(base?.overrides ?? emptyOverrides());
  for (const kv of (values.agent as string[] | undefined) ?? []) {
    const i = kv.indexOf("=");
    if (i <= 0) throw new Error(`--agent expects role=selector, got "${kv}"`);
    const sel = kv.slice(i + 1);
    parseSelector(sel, { allowSpace: true });
    ov.agents[kv.slice(0, i)] = sel;
  }
  for (const v of (values.space as string[] | undefined) ?? []) {
    const i = v.indexOf("=");
    if (i < 0) ov.defaultSpace = v;
    else ov.spaces[v.slice(0, i)] = v.slice(i + 1);
  }
  return { args, overrides: ov };
}

function pidAlive(pid: number | null): boolean {
  if (!pid || pid === process.pid) return false;
  try {
    process.kill(pid, 0);
    return true;
  } catch {
    return false;
  }
}

export async function main(argv: string[]): Promise<number> {
  let parsed;
  try {
    parsed = parseArgs({
      args: argv,
      allowPositionals: true,
      options: {
        args: { type: "string" },
        arg: { type: "string", multiple: true },
        agent: { type: "string", multiple: true },
        space: { type: "string", multiple: true },
        "no-view": { type: "boolean" },
        json: { type: "boolean" },
        help: { type: "boolean", short: "h" },
      },
    });
  } catch (e) {
    process.stderr.write(`${(e as Error).message}\n\n${HELP}`);
    return 2;
  }
  const { values, positionals } = parsed;
  const [cmd, target] = positionals;
  if (values.help || !cmd) {
    process.stdout.write(HELP);
    return cmd || values.help ? 0 : 2;
  }

  try {
    const config = loadConfig();
    const cwd = process.cwd();
    switch (cmd) {
      case "ls":
        return cmdLs(cwd);
      case "runs":
        return withJournal((j) => cmdRuns(j));
      case "show":
        return withJournal((j) => cmdShow(j, need(target, "run id"), !!values.json));
      case "cancel":
        return withJournal((j) => {
          const run = findRun(j, need(target, "run id"));
          if (run.status !== "running") throw new Error(`run ${run.id} is ${run.status}`);
          j.requestCancel(run.id);
          process.stderr.write(`cancel requested for ${run.id}\n`);
          return 0;
        });
      case "watch":
        return withJournal((j) => cmdWatch(j, findRun(j, need(target, "run id")).id, config));
      case "run":
      case "resume":
        return withJournal((j) => cmdRun(j, cmd, need(target, cmd === "run" ? "workflow" : "run id"), values, config, cwd));
      default:
        throw new Error(`unknown command "${cmd}" (see sheepdog --help)`);
    }
  } catch (e) {
    process.stderr.write(`sheepdog: ${(e as Error).message}\n`);
    return 1;
  }
}

function need(v: string | undefined, what: string): string {
  if (!v) throw new Error(`missing ${what}`);
  return v;
}

async function withJournal(fn: (j: Journal) => number | Promise<number>): Promise<number> {
  const j = new Journal(join(stateDir(), "journal.db"));
  try {
    return await fn(j);
  } finally {
    j.close();
  }
}

function findRun(j: Journal, id: string) {
  const run = j.findRun(id);
  if (!run) throw new Error(`no run matching "${id}" (see sheepdog runs)`);
  return run;
}

function cmdLs(cwd: string): number {
  const wfs = listWorkflows(cwd);
  if (!wfs.length) {
    process.stdout.write("no workflows found\n");
    return 0;
  }
  const w = Math.max(...wfs.map((x) => x.name.length));
  for (const wf of wfs) {
    const desc = wf.error ? `error: ${wf.error}` : (wf.description ?? "");
    const shadow = wf.shadows ? ` (overrides ${wf.shadows})` : "";
    process.stdout.write(`${wf.name.padEnd(w)}  ${wf.source.padEnd(7)}  ${desc}${shadow}\n`);
  }
  return 0;
}

function cmdRuns(j: Journal): number {
  const runs = j.listRuns(20);
  if (!runs.length) process.stdout.write("no runs yet\n");
  for (const r of runs) {
    const when = new Date(r.createdAt).toLocaleString();
    const dur = formatDuration(r.updatedAt - r.createdAt);
    const live = r.status === "running" && !pidAlive(r.pid) ? " (runner gone; resume it)" : "";
    process.stdout.write(`${r.id}  ${r.status.padEnd(9)}  ${r.workflow.padEnd(24)}  ${when}  ${dur}${live}\n`);
  }
  return 0;
}

function cmdShow(j: Journal, id: string, json: boolean): number {
  const run = findRun(j, id);
  if (json) {
    process.stdout.write(JSON.stringify({ run, calls: j.listCalls(run.id, run.attempt) }, null, 2) + "\n");
    return 0;
  }
  process.stdout.write(render(j, run.id, { width: process.stdout.columns || 100 }).join("\n") + "\n");
  if (run.status === "done") process.stdout.write(JSON.stringify(run.result, null, 2) + "\n");
  return 0;
}

async function cmdWatch(j: Journal, runId: string, config: Config): Promise<number> {
  return new Promise((resolve) => {
    let timer: NodeJS.Timeout | undefined;
    const cleanup = () => {
      clearInterval(timer);
      process.off("SIGINT", finish);
      resolve(0);
    };
    const finish = () => {
      view.stop();
      cleanup();
    };
    const view = new LiveView({ journal: j, runId, backendFor: backendFactory(config), mode: "watch", onQuit: cleanup });
    view.start();
    if (!view.interactive) process.stderr.write(`watching ${runId} (ctrl+c to stop)\n`);
    timer = setInterval(() => {
      view.update();
      if (j.getRun(runId)?.status !== "running") finish();
    }, 1000);
    process.once("SIGINT", finish);
  });
}

async function cmdRun(
  j: Journal,
  cmd: "run" | "resume",
  target: string,
  values: Record<string, unknown>,
  config: Config,
  cwd: string,
): Promise<number> {
  let scriptPath: string;
  let resume: string | undefined;
  let flags;
  if (cmd === "resume") {
    const run = findRun(j, target);
    if (run.status === "running" && pidAlive(run.pid)) throw new Error(`run ${run.id} is still running (pid ${run.pid})`);
    resume = run.id;
    scriptPath = run.scriptPath;
    flags = parseRunFlags(values, { args: run.args, overrides: run.overrides });
  } else {
    scriptPath = findWorkflow(target, cwd);
    flags = parseRunFlags(values);
  }
  const script = loadScript(scriptPath); // re-read on resume: edited calls re-run, unchanged ones replay
  const backendFor = backendFactory(config);

  const ac = new AbortController();
  let view: LiveView | undefined;
  let sigints = 0;
  const onSigint = () => {
    if (++sigints > 1) process.exit(130);
    process.stderr.write("\ncancelling… (ctrl+c again to exit immediately)\n");
    ac.abort();
  };
  process.on("SIGINT", onSigint);

  const outcome = await runWorkflow({
    script,
    journal: j,
    config,
    backendFor,
    resume,
    cwd,
    args: flags.args,
    overrides: flags.overrides,
    signal: ac.signal,
    onStart: (run) => {
      const plain = !!values["no-view"];
      view = new LiveView({
        journal: j,
        runId: run.id,
        backendFor,
        mode: "run",
        plain,
        onQuit: () => process.stderr.write(`view closed; run ${run.id} continues (sheepdog watch ${run.id})\n`),
      });
      if (!view.interactive) process.stderr.write(`sheepdog: ${cmd === "run" ? "started" : "resumed"} run ${run.id} (${run.workflow})\n`);
      view.start();
    },
    onChange: () => view?.update(),
  });
  view?.stop();
  process.off("SIGINT", onSigint);

  if (outcome.status === "done") {
    process.stdout.write(JSON.stringify(outcome.result, null, 2) + "\n");
    return 0;
  }
  process.stderr.write(`run ${outcome.runId} ${outcome.status}${outcome.error ? ": " + outcome.error : ""}\n`);
  if (outcome.status === "failed") process.stderr.write(`resume with: sheepdog resume ${outcome.runId}\n`);
  return outcome.status === "cancelled" ? 130 : 1;
}
