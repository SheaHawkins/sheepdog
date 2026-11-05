import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import type { Backend } from "../src/backend.ts";
import { SimulateBackend, type Responder } from "../src/backends/simulate.ts";
import { DEFAULT_CONFIG, type Config } from "../src/config.ts";
import { Journal, type Overrides } from "../src/journal.ts";
import { runWorkflow, type RunWorkflowOptions } from "../src/runner.ts";
import { parseScript } from "../src/script.ts";
import { parseResultContract } from "../src/prompt.ts";
import type { Clock } from "../src/util.ts";

export function tmpCwd(): string {
  return mkdtempSync(join(tmpdir(), "sheepdog-test-"));
}

/** Virtual time: sleep advances the clock instantly. */
export function fakeClock(start = 1_000_000): Clock & { t: number } {
  const c = {
    t: start,
    now: () => c.t,
    sleep: async (ms: number, signal?: AbortSignal) => {
      if (signal?.aborted) throw new Error("cancelled");
      c.t += ms;
      await new Promise((r) => setImmediate(r));
    },
  };
  return c;
}

export interface Env {
  journal: Journal;
  cwd: string;
  backends: Record<string, SimulateBackend>;
  config: Config;
  run(src: string, o?: Partial<RunWorkflowOptions> & { overrides?: Overrides }): ReturnType<typeof runWorkflow>;
}

export function makeEnv(o: { respond?: Responder; unavailable?: string[]; config?: Partial<Config>; spaces?: string[] } = {}): Env {
  const journal = new Journal(":memory:");
  const cwd = tmpCwd();
  const spaces = o.spaces ?? ["local"];
  const backends: Record<string, SimulateBackend> = {};
  const config: Config = {
    ...structuredClone(DEFAULT_CONFIG),
    spaces: {},
    ...o.config,
  };
  for (const s of spaces) {
    backends[s] = new SimulateBackend({ respond: o.respond ?? echoResponder, unavailable: o.unavailable });
    config.spaces[s] = { backend: "simulate" };
  }
  config.defaultSpace = spaces[0];
  return {
    journal,
    cwd,
    backends,
    config,
    run: (src, ro = {}) =>
      runWorkflow({
        script: parseScript(src, "/virtual/wf.ts"),
        journal,
        config,
        backendFor: (s) => backends[s] as Backend,
        cwd,
        ...ro,
      }),
  };
}

/** Answers schema prompts with { echo: <first line> } when the schema has an `echo` field, else text. */
export const echoResponder: Responder = (prompt) => {
  const c = parseResultContract(prompt);
  const first = prompt.split("\n")[0];
  if (c?.schema) return { result: { echo: first } };
  return { result: `answer: ${first}` };
};
