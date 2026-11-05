import { AbortedError } from "./util.ts";

export class Semaphore {
  #free: number;
  #queue: Array<{ resolve: () => void; reject: (e: Error) => void }> = [];

  constructor(n: number) {
    if (!(n >= 1)) throw new Error(`semaphore size must be >= 1, got ${n}`);
    this.#free = n;
  }

  async acquire(signal?: AbortSignal): Promise<() => void> {
    if (signal?.aborted) throw new AbortedError();
    if (this.#free > 0) {
      this.#free--;
    } else {
      await new Promise<void>((resolve, reject) => {
        const waiter = { resolve, reject };
        this.#queue.push(waiter);
        signal?.addEventListener(
          "abort",
          () => {
            const i = this.#queue.indexOf(waiter);
            if (i >= 0) {
              this.#queue.splice(i, 1);
              reject(new AbortedError());
            }
          },
          { once: true },
        );
      });
    }
    let released = false;
    return () => {
      if (released) return;
      released = true;
      const next = this.#queue.shift();
      if (next) next.resolve();
      else this.#free++;
    };
  }
}

export interface Limits {
  /** max agents running at once across all spaces */
  concurrency: number;
  /** max agents started per run attempt */
  maxAgents: number;
}

/** Concurrency (global + per space) and a per-run spawn cap. Only agent calls take slots. */
export class Scheduler {
  #global: Semaphore;
  #spaces = new Map<string, Semaphore>();
  #spaceLimits: Record<string, number | undefined>;
  #maxAgents: number;
  spawned = 0;

  constructor(limits: Limits, spaceLimits: Record<string, number | undefined> = {}) {
    this.#global = new Semaphore(limits.concurrency);
    this.#maxAgents = limits.maxAgents;
    this.#spaceLimits = spaceLimits;
  }

  /** `spawn` = this acquisition will start a new agent (reattaching doesn't count). */
  async acquire(space: string, spawn: boolean, signal?: AbortSignal): Promise<() => void> {
    if (spawn) {
      if (this.spawned >= this.#maxAgents) {
        throw new Error(`run limit reached: at most ${this.#maxAgents} agents per run (limits.maxAgents)`);
      }
      this.spawned++;
    }
    const releaseGlobal = await this.#global.acquire(signal);
    const limit = this.#spaceLimits[space];
    if (!limit) return releaseGlobal;
    let sem = this.#spaces.get(space);
    if (!sem) this.#spaces.set(space, (sem = new Semaphore(limit)));
    try {
      const releaseSpace = await sem.acquire(signal);
      return () => {
        releaseSpace();
        releaseGlobal();
      };
    } catch (e) {
      releaseGlobal();
      throw e;
    }
  }
}
