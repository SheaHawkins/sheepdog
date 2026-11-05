import { AsyncLocalStorage } from "node:async_hooks";
import { spawn } from "node:child_process";
import { randomBytes } from "node:crypto";
import { resolve as resolvePath } from "node:path";
import type { Backend, Handle } from "./backend.ts";
import type { Config } from "./config.ts";
import type { Call, Journal, Overrides, Run, RunStatus } from "./journal.ts";
import { buildPrompt, clearResult, correctionPrompt, readResult, resultPathFor } from "./prompt.ts";
import { Scheduler, Semaphore } from "./scheduler.ts";
import { schema as schemaFn, type JSONSchema } from "./schema.ts";
import { compileScript, type Script } from "./script.ts";
import { formatAgent, parseSelector, resolveSelector, type ResolvedAgent } from "./selector.ts";
import {
  AbortedError,
  errorMessage,
  formatDuration,
  hashOf,
  parseDuration,
  realClock,
  throwIfAborted,
  type Clock,
} from "./util.ts";

export interface RunWorkflowOptions {
  script: Script;
  journal: Journal;
  config: Config;
  backendFor: (space: string) => Backend;
  /** Resume this run instead of starting a new one. */
  resume?: string;
  runId?: string;
  cwd?: string;
  args?: unknown;
  /** On resume, replaces the stored overrides when given. */
  overrides?: Overrides;
  signal?: AbortSignal;
  clock?: Clock;
  /** Called whenever journal state changes (for live views). */
  onChange?: () => void;
  /** Called once the run row exists. */
  onStart?: (run: Run) => void;
}

export interface RunOutcome {
  runId: string;
  status: RunStatus;
  result?: unknown;
  error?: string;
}

export interface AgentOptions {
  as?: string;
  use?: string;
  schema?: JSONSchema;
  label?: string;
  phase?: string;
  isolation?: "none" | "worktree";
  cwd?: string;
  timeout?: string | number;
  /** @deprecated alias of timeout */
  timeoutMs?: number;
}

export interface StepOptions {
  /** space role; the space itself is chosen at run time */
  on?: string;
}

export interface WaitForOptions extends StepOptions {
  every?: string | number;
  timeout?: string | number;
}

export function newRunId(): string {
  return Date.now().toString(36) + randomBytes(3).toString("hex");
}

export function emptyOverrides(): Overrides {
  return { agents: {}, spaces: {} };
}

export async function runWorkflow(o: RunWorkflowOptions): Promise<RunOutcome> {
  const { journal, config, script } = o;
  let run: Run;
  if (o.resume) {
    const r = journal.getRun(o.resume);
    if (!r) throw new Error(`no run ${o.resume}`);
    run = r;
  } else {
    run = journal.createRun({
      id: o.runId ?? newRunId(),
      workflow: script.meta.name,
      scriptPath: script.path,
      cwd: resolvePath(o.cwd ?? process.cwd()),
      args: o.args ?? null,
      overrides: o.overrides ?? emptyOverrides(),
    });
  }
  const overrides = o.overrides ?? run.overrides ?? emptyOverrides();
  const args = o.args !== undefined ? o.args : run.args;
  validateOverrides(overrides, config);

  const attempt = journal.startAttempt(run.id, { pid: process.pid, overrides, args });
  run = journal.getRun(run.id)!;
  o.onStart?.(run);

  const ac = new AbortController();
  const onOuterAbort = () => ac.abort();
  o.signal?.addEventListener("abort", onOuterAbort, { once: true });
  if (o.signal?.aborted) ac.abort();
  const poll = setInterval(() => {
    if (journal.isCancelRequested(run.id)) ac.abort();
  }, 1000);
  poll.unref();

  const ctx = new RunContext({
    run,
    attempt,
    script,
    overrides,
    config,
    journal,
    backendFor: o.backendFor,
    signal: ac.signal,
    clock: o.clock ?? realClock,
    onChange: o.onChange ?? (() => {}),
  });

  try {
    const fn = compileScript(script);
    const result = await fn(...ctx.globals(structuredClone(args)));
    throwIfAborted(ac.signal);
    const stored = result === undefined ? null : JSON.parse(JSON.stringify(result));
    journal.finishRun(run.id, "done", stored);
    return { runId: run.id, status: "done", result: stored };
  } catch (e) {
    if (ac.signal.aborted) {
      await ctx.settleInflight();
      journal.finishRun(run.id, "cancelled", undefined, "cancelled");
      return { runId: run.id, status: "cancelled", error: "cancelled" };
    }
    // Agents still running are left alone: `resume` reattaches to them.
    const msg = errorMessage(e);
    journal.finishRun(run.id, "failed", undefined, msg);
    return { runId: run.id, status: "failed", error: msg };
  } finally {
    clearInterval(poll);
    o.signal?.removeEventListener("abort", onOuterAbort);
    o.onChange?.();
  }
}

function validateOverrides(ov: Overrides, config: Config) {
  for (const [role, sel] of Object.entries(ov.agents)) {
    for (const c of parseSelector(sel, { allowSpace: true })) {
      if (c.space && !config.spaces[c.space]) throw new Error(`--agent ${role}=${sel}: unknown space "${c.space}"`);
    }
  }
  for (const [role, space] of Object.entries(ov.spaces)) {
    if (!config.spaces[space]) throw new Error(`--space ${role}=${space}: unknown space "${space}"`);
  }
  if (ov.defaultSpace && !config.spaces[ov.defaultSpace]) throw new Error(`--space ${ov.defaultSpace}: unknown space`);
}

interface StepContext {
  space: string;
  cwd: string;
  signal: AbortSignal;
}

interface ContextInit {
  run: Run;
  attempt: number;
  script: Script;
  overrides: Overrides;
  config: Config;
  journal: Journal;
  backendFor: (space: string) => Backend;
  signal: AbortSignal;
  clock: Clock;
  onChange: () => void;
}

class RunContext {
  #i: ContextInit;
  #scheduler: Scheduler;
  #occurrences = new Map<string, number>();
  #names = new Set<string>();
  #phase: string | null = null;
  #als = new AsyncLocalStorage<StepContext>();
  #inflight = new Set<Promise<unknown>>();

  constructor(init: ContextInit) {
    this.#i = init;
    const spaceLimits: Record<string, number | undefined> = {};
    for (const [name, s] of Object.entries(init.config.spaces)) spaceLimits[name] = s.limit;
    this.#scheduler = new Scheduler(init.config.limits, spaceLimits);
  }

  get #runId() {
    return this.#i.run.id;
  }

  globals(args: unknown): unknown[] {
    // Order must match GLOBALS in script.ts.
    return [
      (p: string, o?: AgentOptions) => this.agent(p, o),
      (t: unknown[], o?: { limit?: number; settle?: boolean }) => this.parallel(t, o),
      (items: unknown[], ...stages: ((v: unknown, i: number) => unknown)[]) => this.pipeline(items, ...stages),
      (title: string) => this.phase(title),
      schemaFn,
      (name: string, fn: () => unknown, o?: StepOptions) => this.step(name, fn, o),
      (name: string, check: () => unknown, o?: WaitForOptions) => this.waitFor(name, check, o),
      (name: string, d: string | number) => this.sleep(name, d),
      (strings: TemplateStringsArray, ...vals: unknown[]) => this.sh(strings, ...vals),
      args,
    ];
  }

  async settleInflight() {
    await Promise.allSettled([...this.#inflight]);
  }

  #track<T>(p: Promise<T>): Promise<T> {
    this.#inflight.add(p);
    p.then(
      () => this.#inflight.delete(p),
      () => this.#inflight.delete(p),
    );
    return p;
  }

  #changed() {
    try {
      this.#i.onChange();
    } catch {
      /* views must not break runs */
    }
  }

  // ---- composition -------------------------------------------------------

  phase(title: string) {
    if (typeof title !== "string" || !title) throw new TypeError("phase(title): title must be a non-empty string");
    this.#phase = title;
  }

  async parallel(tasks: unknown[], opts: { limit?: number; settle?: boolean } = {}) {
    if (!Array.isArray(tasks)) throw new TypeError("parallel(tasks): tasks must be an array of functions");
    const sem = opts.limit ? new Semaphore(opts.limit) : undefined;
    const runOne = async (t: unknown) => {
      const release = sem ? await sem.acquire(this.#i.signal) : undefined;
      try {
        return await (typeof t === "function" ? (t as () => unknown)() : t);
      } finally {
        release?.();
      }
    };
    const ps = tasks.map(runOne);
    if (!opts.settle) return Promise.all(ps);
    const rs = await Promise.allSettled(ps);
    return rs.map((r) => (r.status === "fulfilled" ? { ok: true, value: r.value } : { ok: false, error: errorMessage(r.reason) }));
  }

  async pipeline(items: unknown[], ...stages: ((v: unknown, i: number) => unknown)[]) {
    if (!Array.isArray(items)) throw new TypeError("pipeline(items, ...stages): items must be an array");
    return Promise.all(
      items.map(async (item, i) => {
        let v: unknown = item;
        for (const stage of stages) v = await stage(v, i);
        return v;
      }),
    );
  }

  // ---- agents ------------------------------------------------------------

  agent(prompt: string, opts: AgentOptions = {}): Promise<unknown> {
    if (typeof prompt !== "string" || !prompt.trim()) {
      return Promise.reject(new TypeError("agent(prompt): prompt must be a non-empty string"));
    }
    if (opts.as && opts.use) return Promise.reject(new TypeError("agent(): pass either `as` (role) or `use` (selector), not both"));
    // The occurrence index is taken synchronously so identical calls are numbered in call order.
    const role = opts.use ? undefined : (opts.as ?? "default");
    const base = hashOf({ prompt, schema: opts.schema ?? null, isolation: opts.isolation ?? "none", cwd: opts.cwd ?? null, role: role ?? null, use: opts.use ?? null });
    const n = (this.#occurrences.get(base) ?? 0) + 1;
    this.#occurrences.set(base, n);
    const phase = opts.phase ?? this.#phase;
    return this.#track(this.#agentCall(prompt, opts, role, base, n, phase));
  }

  async #resolveAgent(role: string | undefined, use: string | undefined): Promise<ResolvedAgent> {
    const { overrides, config, script } = this.#i;
    let sel: string;
    let allowSpace = false;
    if (use) sel = use;
    else if (role && overrides.agents[role]) {
      sel = overrides.agents[role];
      allowSpace = true;
    } else if (role && script.meta.agents?.[role]) sel = script.meta.agents[role];
    else if (role === "default") sel = "*";
    else throw new Error(`unknown agent role "${role}": declare it in meta.agents or pass --agent ${role}=<selector>`);

    const choices = parseSelector(sel, { allowSpace });
    const defaultSpace = (role && overrides.spaces[role]) || overrides.defaultSpace || config.defaultSpace;
    for (const c of choices) {
      const s = c.space ?? defaultSpace;
      if (!config.spaces[s]) throw new Error(`unknown space "${s}"`);
    }
    return resolveSelector(choices, {
      defaultSpace,
      isAvailable: (space, kind) => this.#i.backendFor(space).isAvailable(kind),
      preference: config.preference,
    });
  }

  async #agentCall(prompt: string, opts: AgentOptions, role: string | undefined, base: string, n: number, phase: string | null) {
    const { journal, signal, attempt, run, clock } = this.#i;
    throwIfAborted(signal);
    const agent = await this.#resolveAgent(role, opts.use);
    const key = `agent:${hashOf({ base, agent })}#${n}`;
    const label = opts.label ?? summarize(prompt);
    const schema = opts.schema;
    const timeoutMs = opts.timeout !== undefined ? parseDuration(opts.timeout) : (opts.timeoutMs ?? this.#i.config.limits.agentTimeoutMs);
    const backend = this.#i.backendFor(agent.space);

    const existing = journal.getCall(run.id, key);
    if (existing?.status === "done") {
      journal.updateCall(run.id, key, { attempt, label, phase, data: { ...existing.data, replayed: true } });
      this.#changed();
      return existing.result;
    }

    // Reattach to an agent left running by a previous attempt.
    let handle: Handle | undefined;
    let data: Record<string, unknown> = {};
    if (existing && (existing.status === "running" || existing.status === "blocked") && existing.handle) {
      const rp = existing.data.resultPath as string | undefined;
      if (rp && existing.data.prompted) {
        const r = readResult(rp, schema);
        if (r.ok) {
          journal.updateCall(run.id, key, { attempt, status: "done", result: r.value, finishedAt: clock.now(), data: { ...existing.data, replayed: false } });
          this.#changed();
          return r.value;
        }
      }
      if (await backend.alive(existing.handle as Handle)) {
        handle = existing.handle as Handle;
        data = { ...existing.data };
      }
    }

    const fmt = formatAgent(agent);
    let call: Call;
    if (existing) {
      journal.updateCall(run.id, key, {
        attempt, status: "running", label, phase, agent: fmt, space: agent.space, error: null,
        startedAt: clock.now(), finishedAt: null, data: { ...data, replayed: false },
      });
      call = journal.getCall(run.id, key)!;
    } else {
      call = journal.insertCall({ runId: run.id, key, kind: "agent", label, phase, status: "running", attempt, agent: fmt, space: agent.space });
    }
    this.#changed();

    let release: (() => void) | undefined;
    try {
      release = await this.#scheduler.acquire(agent.space, !handle, signal);
      let tries = call.tries;
      if (!handle) {
        tries = existing ? call.tries + 1 : 1;
        const name = `sd-${run.id.slice(-6)}-${call.seq}-${tries}`;
        handle = await backend.start(
          {
            name, kind: agent.kind, model: agent.model, effort: agent.effort,
            cwd: resolvePath(run.cwd, opts.cwd ?? "."), isolation: opts.isolation ?? "none",
            label, runId: run.id, workflow: this.#i.script.meta.name,
          },
          signal,
        );
        const resultPath = resultPathFor(handle.cwd, run.id, key, !!schema);
        clearResult(resultPath);
        data = { resultPath, prompted: false };
        journal.updateCall(run.id, key, { handle, tries, data });
        this.#changed();
      }
      const resultPath = data.resultPath as string;

      let seqBefore: number | undefined;
      const reattached = !!data.prompted;
      if (!data.prompted) {
        seqBefore = (await backend.status(handle))?.completionSeq;
        await backend.prompt(handle, buildPrompt(prompt, resultPath, schema), signal);
        data = { ...data, prompted: true };
        journal.updateCall(run.id, key, { data });
      }

      const value = await this.#collect(handle, backend, key, resultPath, schema, timeoutMs, seqBefore, reattached);
      journal.updateCall(run.id, key, { status: "done", result: value, finishedAt: clock.now() });
      return value;
    } catch (e) {
      if (signal.aborted) {
        if (handle) await backend.stop(handle).catch(() => {});
        journal.updateCall(run.id, key, { status: "cancelled", error: "cancelled", finishedAt: clock.now() });
        throw new AbortedError();
      }
      journal.updateCall(run.id, key, { status: "failed", error: errorMessage(e), finishedAt: clock.now() });
      throw new Error(`agent "${label}" (${fmt}): ${errorMessage(e)}`);
    } finally {
      release?.();
      this.#changed();
    }
  }

  /** Waits for the agent to settle, reads the result file, and sends one correction if it is unusable. */
  async #collect(
    h: Handle, backend: Backend, key: string, resultPath: string, schema: JSONSchema | undefined,
    timeoutMs: number, seqBefore: number | undefined, reattached: boolean,
  ): Promise<unknown> {
    const budget = { activeMs: 0 };
    await this.#waitTurn(h, backend, key, timeoutMs, budget, seqBefore, resultPath, reattached);
    let r = readResult(resultPath, schema);
    if (r.ok) return r.value;

    const seq = (await backend.status(h))?.completionSeq;
    await backend.prompt(h, correctionPrompt(resultPath, r.error, schema), this.#i.signal);
    await this.#waitTurn(h, backend, key, timeoutMs, budget, seq, resultPath, false);
    r = readResult(resultPath, schema);
    if (r.ok) return r.value;
    throw new Error(r.error);
  }

  async #waitTurn(
    h: Handle, backend: Backend, key: string, timeoutMs: number, budget: { activeMs: number },
    seqBefore: number | undefined, resultPath: string, reattached: boolean,
  ): Promise<void> {
    const { journal, clock, signal, run } = this.#i;
    const timedOut = () => new Error(`timed out after ${formatDuration(timeoutMs)}`);
    const timed = async <T>(p: () => Promise<T>) => {
      const t0 = clock.now();
      try {
        return await p();
      } finally {
        budget.activeMs += clock.now() - t0;
      }
    };
    const resultExists = () => readResult(resultPath).ok;

    // Without a completion counter, give the agent a moment to pick up the prompt first,
    // so we don't mistake the pre-prompt idle state for a finished turn.
    if (seqBefore === undefined && !reattached) {
      await timed(() => backend.wait(h, ["working", "blocked"], { timeoutMs: Math.min(15_000, timeoutMs), signal }));
    }

    for (;;) {
      const remaining = timeoutMs - budget.activeMs;
      if (remaining <= 0) throw timedOut();
      const st = await timed(() => backend.wait(h, ["idle", "done", "blocked"], { timeoutMs: remaining, signal }));
      if (st === "timeout") throw timedOut();

      if (st.state === "blocked") {
        journal.updateCall(run.id, key, { status: "blocked" });
        this.#changed();
        // A human answers blocked agents; this time doesn't count against the timeout.
        await backend.wait(h, ["working", "idle", "done"], { signal });
        journal.updateCall(run.id, key, { status: "running" });
        this.#changed();
        continue;
      }

      const notStarted =
        seqBefore !== undefined && st.completionSeq !== undefined && st.completionSeq <= seqBefore && !resultExists();
      if (notStarted) {
        const w = await timed(() => backend.wait(h, ["working", "blocked"], { timeoutMs: Math.min(15_000, Math.max(1, timeoutMs - budget.activeMs)), signal }));
        if (w === "timeout" && !resultExists()) throw new Error("agent never started working on the prompt");
        continue;
      }
      return;
    }
  }

  // ---- steps -------------------------------------------------------------

  #stepSpace(on?: string): string {
    const { overrides, config } = this.#i;
    const space = (on && overrides.spaces[on]) || overrides.defaultSpace || config.defaultSpace;
    if (!config.spaces[space]) throw new Error(`unknown space "${space}"`);
    return space;
  }

  #claimName(kind: string, name: string) {
    if (typeof name !== "string" || !name) throw new TypeError(`${kind}(name, ...): name must be a non-empty string`);
    const id = `${kind}:${name}`;
    if (this.#names.has(id)) throw new Error(`${kind} name "${name}" is used twice in this run; names must be unique (add an index, e.g. "${name}:\${i}")`);
    this.#names.add(id);
  }

  /** Inserts or re-arms a step-like call. Returns the existing row (if any) before re-arming. */
  #beginStepCall(kind: "step" | "wait" | "sleep", key: string, label: string, space: string | null, data: Record<string, unknown>) {
    const { journal, run, attempt, clock } = this.#i;
    const existing = journal.getCall(run.id, key);
    if (existing) {
      journal.updateCall(run.id, key, {
        attempt, status: "running", error: null, phase: this.#phase, data,
        startedAt: existing.status === "running" ? existing.startedAt : clock.now(), finishedAt: null,
      });
    } else {
      journal.insertCall({ runId: run.id, key, kind, label, phase: this.#phase, status: "running", attempt, space, data });
    }
    this.#changed();
  }

  #replay(key: string): { hit: true; value: unknown } | { hit: false; prev?: Call } {
    const { journal, run, attempt } = this.#i;
    const prev = journal.getCall(run.id, key);
    if (prev?.status === "done") {
      journal.updateCall(run.id, key, { attempt, phase: this.#phase, data: { ...prev.data, replayed: true } });
      this.#changed();
      return { hit: true, value: prev.result };
    }
    return { hit: false, prev };
  }

  #finishStep(key: string, value: unknown) {
    const { journal, run, clock } = this.#i;
    const stored = value === undefined ? null : JSON.parse(JSON.stringify(value));
    journal.updateCall(run.id, key, { status: "done", result: stored, finishedAt: clock.now() });
    this.#changed();
    return stored;
  }

  #failStep(key: string, e: unknown): never {
    const { journal, run, clock, signal } = this.#i;
    const cancelled = signal.aborted;
    journal.updateCall(run.id, key, { status: cancelled ? "cancelled" : "failed", error: errorMessage(e), finishedAt: clock.now() });
    this.#changed();
    throw cancelled ? new AbortedError() : e;
  }

  step(name: string, fn: () => unknown, opts: StepOptions = {}): Promise<unknown> {
    return this.#track(
      (async () => {
        this.#claimName("step", name);
        if (typeof fn !== "function") throw new TypeError("step(name, fn): fn must be a function");
        const space = this.#stepSpace(opts.on);
        const key = `step:${name}@${space}`;
        const r = this.#replay(key);
        if (r.hit) return r.value;
        this.#beginStepCall("step", key, name, space, {});
        try {
          throwIfAborted(this.#i.signal);
          const v = await this.#als.run({ space, cwd: this.#i.run.cwd, signal: this.#i.signal }, fn);
          return this.#finishStep(key, v);
        } catch (e) {
          this.#failStep(key, e);
        }
      })(),
    );
  }

  waitFor(name: string, check: () => unknown, opts: WaitForOptions = {}): Promise<unknown> {
    return this.#track(
      (async () => {
        this.#claimName("waitFor", name);
        if (typeof check !== "function") throw new TypeError("waitFor(name, check): check must be a function");
        const every = parseDuration(opts.every ?? "30s");
        const timeout = parseDuration(opts.timeout ?? "30m");
        const space = this.#stepSpace(opts.on);
        const key = `wait:${name}@${space}`;
        const r = this.#replay(key);
        if (r.hit) return r.value;
        const { clock, signal, journal, run } = this.#i;
        // An interrupted wait keeps its original start, so the timeout doesn't reset on resume.
        const startedAt =
          r.prev?.status === "running" && typeof r.prev.data.startedAt === "number" ? r.prev.data.startedAt : clock.now();
        let data: Record<string, unknown> = { startedAt, every, timeout, checks: 0 };
        this.#beginStepCall("wait", key, name, space, data);
        let consecutiveErrors = 0;
        try {
          for (;;) {
            throwIfAborted(signal);
            let v: unknown;
            try {
              v = await this.#als.run({ space, cwd: run.cwd, signal }, check);
              consecutiveErrors = 0;
            } catch (e) {
              if (signal.aborted || ++consecutiveErrors >= 3) throw e;
            }
            data = { ...data, checks: Number(data.checks) + 1, lastCheckAt: clock.now() };
            journal.updateCall(run.id, key, { data });
            this.#changed();
            if (v !== undefined && v !== null) return this.#finishStep(key, v);
            const left = startedAt + timeout - clock.now();
            if (left <= 0) throw new Error(`waitFor "${name}" timed out after ${formatDuration(timeout)}`);
            await clock.sleep(Math.min(every, left), signal);
          }
        } catch (e) {
          this.#failStep(key, e);
        }
      })(),
    );
  }

  sleep(name: string, d: string | number): Promise<unknown> {
    return this.#track(
      (async () => {
        this.#claimName("sleep", name);
        const ms = parseDuration(d);
        const key = `sleep:${name}`;
        const r = this.#replay(key);
        if (r.hit) return null;
        const { clock, signal } = this.#i;
        const wakeAt = r.prev?.status === "running" && typeof r.prev.data.wakeAt === "number" ? r.prev.data.wakeAt : clock.now() + ms;
        this.#beginStepCall("sleep", key, name, null, { wakeAt });
        try {
          await clock.sleep(Math.max(0, wakeAt - clock.now()), signal);
          return this.#finishStep(key, null);
        } catch (e) {
          this.#failStep(key, e);
        }
      })(),
    );
  }

  sh(strings: TemplateStringsArray, ...vals: unknown[]): Promise<string> {
    const ctx = this.#als.getStore();
    if (!ctx) return Promise.reject(new Error("sh`...` can only be used inside step() or waitFor()"));
    if (!Array.isArray(strings) || !("raw" in strings)) return Promise.reject(new TypeError("sh must be used as a tagged template: sh`cmd ${arg}`"));
    const space = this.#i.config.spaces[ctx.space];
    if (space?.machine) {
      return Promise.reject(new Error(`sh in remote space "${ctx.space}" isn't supported yet; steps run locally in v1`));
    }
    const cmd = strings.reduce((acc, s, i) => acc + s + (i < vals.length ? shellQuote(vals[i]) : ""), "");
    return runShell(cmd, ctx.cwd, ctx.signal);
  }
}

export function shellQuote(v: unknown): string {
  if (Array.isArray(v)) return v.map(shellQuote).join(" ");
  const s = v === undefined || v === null ? "" : String(v);
  return `'${s.replace(/'/g, `'\\''`)}'`;
}

export class ShellError extends Error {
  code: number | null;
  stdout: string;
  stderr: string;
  constructor(cmd: string, code: number | null, stdout: string, stderr: string) {
    super(`command failed (exit ${code}): ${cmd}${stderr.trim() ? "\n" + stderr.trim().slice(-2000) : ""}`);
    this.name = "ShellError";
    this.code = code;
    this.stdout = stdout;
    this.stderr = stderr;
  }
}

function runShell(cmd: string, cwd: string, signal: AbortSignal): Promise<string> {
  return new Promise((resolve, reject) => {
    if (signal.aborted) return reject(new AbortedError());
    const child = spawn("/bin/sh", ["-c", cmd], { cwd, signal, stdio: ["ignore", "pipe", "pipe"] });
    let out = "";
    let err = "";
    child.stdout.on("data", (d) => (out += d));
    child.stderr.on("data", (d) => (err += d));
    child.on("error", (e) => reject(signal.aborted ? new AbortedError() : e));
    child.on("close", (code) => {
      if (signal.aborted) return reject(new AbortedError());
      if (code === 0) resolve(out.replace(/\n$/, ""));
      else reject(new ShellError(cmd, code, out, err));
    });
  });
}

function summarize(prompt: string): string {
  const line = prompt.trim().split("\n")[0].replace(/\s+/g, " ");
  return line.length > 48 ? line.slice(0, 47) + "…" : line;
}
