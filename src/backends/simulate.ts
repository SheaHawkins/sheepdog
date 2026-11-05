import { mkdirSync, writeFileSync } from "node:fs";
import { dirname } from "node:path";
import type { AgentState, AgentStatus, Backend, Handle, StartRequest } from "../backend.ts";
import { sampleOf, type JSONSchema } from "../schema.ts";
import { AbortedError, abortableSleep } from "../util.ts";
import { parseResultContract } from "../prompt.ts";

export interface SimulatedTurn {
  /** Value written to the result file. Omit to write nothing (agent "forgot"). */
  result?: unknown;
  /** Raw file contents instead of JSON.stringify(result). */
  raw?: string;
  /** Go "blocked" for this long before finishing. */
  blockMs?: number;
  delayMs?: number;
}

export type Responder = (prompt: string, req: StartRequest, turn: number) => SimulatedTurn | Promise<SimulatedTurn>;

interface Agent {
  req: StartRequest;
  state: AgentState;
  seq: number;
  turns: number;
  listeners: Set<() => void>;
  prompts: string[];
  /** result contract from the first prompt; later prompts (corrections) refer back to it */
  contract?: { path: string; schema?: JSONSchema };
}

/**
 * In-process fake agents. Used by tests (with a responder) and by the built-in `simulate`
 * space, which answers every prompt with a schema-shaped sample after a short delay.
 */
export class SimulateBackend implements Backend {
  agents = new Map<string, Agent>();
  started: StartRequest[] = [];
  #respond: Responder;
  #unavailable: Set<string>;

  constructor(opts: { respond?: Responder; unavailable?: string[] } = {}) {
    this.#respond = opts.respond ?? defaultResponder;
    this.#unavailable = new Set(opts.unavailable ?? []);
  }

  async isAvailable(kind: string) {
    return !this.#unavailable.has(kind);
  }

  async start(req: StartRequest): Promise<Handle> {
    if (this.agents.has(req.name)) throw new Error(`agent name ${req.name} already in use`);
    this.started.push(req);
    this.agents.set(req.name, { req, state: "idle", seq: 0, turns: 0, listeners: new Set(), prompts: [] });
    return { name: req.name, cwd: req.cwd };
  }

  #set(a: Agent, state: AgentState) {
    a.state = state;
    if (state === "idle" || state === "done") a.seq++;
    for (const l of [...a.listeners]) l();
  }

  async prompt(h: Handle, text: string): Promise<void> {
    const a = this.#get(h);
    a.prompts.push(text);
    const turn = a.turns++;
    this.#set(a, "working");
    void (async () => {
      const r = await this.#respond(text, a.req, turn);
      await abortableSleep(r.delayMs ?? 5);
      if (r.blockMs) {
        this.#set(a, "blocked");
        await abortableSleep(r.blockMs);
        this.#set(a, "working");
      }
      const contract = parseResultContract(text) ?? a.contract;
      a.contract = contract;
      if (contract && (r.result !== undefined || r.raw !== undefined)) {
        mkdirSync(dirname(contract.path), { recursive: true });
        writeFileSync(contract.path, r.raw ?? (typeof r.result === "string" ? r.result : JSON.stringify(r.result)));
      }
      this.#set(a, "done");
    })();
  }

  async wait(h: Handle, until: AgentState[], opts: { timeoutMs?: number; signal?: AbortSignal }) {
    const a = this.#get(h);
    if (until.includes(a.state)) return { state: a.state, completionSeq: a.seq };
    return new Promise<AgentStatus | "timeout">((resolve, reject) => {
      const done = (v: AgentStatus | "timeout") => {
        a.listeners.delete(check);
        clearTimeout(timer);
        opts.signal?.removeEventListener("abort", onAbort);
        resolve(v);
      };
      const check = () => {
        if (until.includes(a.state)) done({ state: a.state, completionSeq: a.seq });
      };
      const onAbort = () => {
        a.listeners.delete(check);
        clearTimeout(timer);
        reject(new AbortedError());
      };
      const timer = opts.timeoutMs !== undefined ? setTimeout(() => done("timeout"), opts.timeoutMs) : undefined;
      opts.signal?.addEventListener("abort", onAbort, { once: true });
      a.listeners.add(check);
    });
  }

  async status(h: Handle) {
    const a = this.agents.get(h.name);
    return a ? { state: a.state, completionSeq: a.seq } : undefined;
  }

  async alive(h: Handle) {
    return this.agents.has(h.name);
  }

  async stop(h: Handle) {
    const a = this.agents.get(h.name);
    if (a) this.#set(a, "idle");
  }

  #get(h: Handle): Agent {
    const a = this.agents.get(h.name);
    if (!a) throw new Error(`no agent ${h.name}`);
    return a;
  }
}

const defaultResponder: Responder = (prompt) => {
  const c = parseResultContract(prompt);
  const delayMs = 400 + Math.floor(Math.random() * 1600);
  if (!c?.schema) return { result: `Simulated answer to: ${prompt.split("\n")[0].slice(0, 80)}`, delayMs };
  return { result: sampleOf(c.schema as JSONSchema), delayMs };
};
