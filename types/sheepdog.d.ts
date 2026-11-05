// Editor typings for sheepdog workflow scripts. Reference from a script with:
//   /// <reference path="<path-to-sheepdog>/types/sheepdog.d.ts" />

type Duration = string | number; // "30s", "45m", "2h", or milliseconds
type JSONSchema = { [k: string]: unknown };

interface AgentOptions {
  /** Role declared in meta.agents (overridable at run time with --agent role=...). */
  as?: string;
  /** Inline selector, e.g. "codex:gpt-5.4@high | claude". Can't name a space. */
  use?: string;
  /** Result schema; the agent must write JSON matching it. Without one, the result is text. */
  schema?: JSONSchema;
  label?: string;
  phase?: string;
  /** "worktree" runs the agent in its own git worktree. */
  isolation?: "none" | "worktree";
  /** Working directory, relative to where the run started. */
  cwd?: string;
  /** Default 20m. Time spent blocked on a human doesn't count. */
  timeout?: Duration;
}

interface StepOptions {
  /** Space role; the actual space is chosen at run time with --space role=name. */
  on?: string;
}

interface WaitForOptions extends StepOptions {
  /** Poll interval, default 30s. */
  every?: Duration;
  /** Default 30m, measured from the first check (survives resume). */
  timeout?: Duration;
}

type Settled<T> = { ok: true; value: T } | { ok: false; error: string };

declare function agent(prompt: string, opts?: AgentOptions): Promise<any>;
declare function parallel<T>(tasks: Array<() => T | Promise<T>>, opts?: { limit?: number }): Promise<T[]>;
declare function parallel<T>(tasks: Array<() => T | Promise<T>>, opts: { limit?: number; settle: true }): Promise<Settled<T>[]>;
declare function pipeline(items: any[], ...stages: Array<(value: any, index: number) => any>): Promise<any[]>;
declare function phase(title: string): void;
declare function schema(shape: unknown): JSONSchema;
/** Runs fn once; on resume, returns the stored result. Names must be unique per run. */
declare function step<T>(name: string, fn: () => T | Promise<T>, opts?: StepOptions): Promise<T>;
/** Calls check until it returns something other than undefined/null. */
declare function waitFor<T>(name: string, check: () => T | undefined | null | Promise<T | undefined | null>, opts?: WaitForOptions): Promise<T>;
declare function sleep(name: string, duration: Duration): Promise<null>;
/** Shell command (interpolations are quoted). Only inside step()/waitFor(). Resolves to stdout. */
declare function sh(strings: TemplateStringsArray, ...values: unknown[]): Promise<string>;
declare const args: any;
