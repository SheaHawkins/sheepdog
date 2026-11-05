import { createHash } from "node:crypto";

/** JSON.stringify with sorted object keys, so equal values always hash equally. */
export function stableStringify(v: unknown): string {
  if (v === undefined) return "null";
  if (v === null || typeof v !== "object") return JSON.stringify(v);
  if (Array.isArray(v)) return "[" + v.map(stableStringify).join(",") + "]";
  const o = v as Record<string, unknown>;
  return (
    "{" +
    Object.keys(o)
      .filter((k) => o[k] !== undefined)
      .sort()
      .map((k) => JSON.stringify(k) + ":" + stableStringify(o[k]))
      .join(",") +
    "}"
  );
}

export function hashOf(v: unknown, len = 16): string {
  return createHash("sha256").update(stableStringify(v)).digest("hex").slice(0, len);
}

const UNITS: Record<string, number> = { ms: 1, s: 1000, m: 60_000, h: 3_600_000 };

/** "30s", "45m", "1.5h", "250ms" or a number of milliseconds. */
export function parseDuration(d: string | number): number {
  if (typeof d === "number") {
    if (!Number.isFinite(d) || d < 0) throw new Error(`invalid duration: ${d}`);
    return d;
  }
  const m = /^(\d+(?:\.\d+)?)(ms|s|m|h)$/.exec(d.trim());
  if (!m) throw new Error(`invalid duration "${d}" (use e.g. 250ms, 30s, 45m, 2h)`);
  return Math.round(Number(m[1]) * UNITS[m[2]]);
}

export function formatDuration(ms: number): string {
  if (ms < 1000) return `${ms}ms`;
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m${s % 60 ? String(s % 60).padStart(2, "0") + "s" : ""}`;
  return `${Math.floor(m / 60)}h${String(m % 60).padStart(2, "0")}m`;
}

export class AbortedError extends Error {
  constructor(message = "cancelled") {
    super(message);
    this.name = "AbortedError";
  }
}

export function throwIfAborted(signal?: AbortSignal): void {
  if (signal?.aborted) throw new AbortedError();
}

export function abortableSleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) return reject(new AbortedError());
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    const onAbort = () => {
      clearTimeout(t);
      reject(new AbortedError());
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

export interface Clock {
  now(): number;
  sleep(ms: number, signal?: AbortSignal): Promise<void>;
}

export const realClock: Clock = { now: () => Date.now(), sleep: abortableSleep };

export function errorMessage(e: unknown): string {
  return e instanceof Error ? e.message : String(e);
}
