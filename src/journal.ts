import { mkdirSync } from "node:fs";
import { dirname } from "node:path";
import { DatabaseSync } from "node:sqlite";

export type RunStatus = "running" | "done" | "failed" | "cancelled";
export type CallKind = "agent" | "step" | "wait" | "sleep";
export type CallStatus = "running" | "blocked" | "done" | "failed" | "cancelled";

export interface Overrides {
  /** role → selector (may carry a space prefix) */
  agents: Record<string, string>;
  /** role → space */
  spaces: Record<string, string>;
  /** default space for this run */
  defaultSpace?: string;
}

export interface Run {
  id: string;
  workflow: string;
  scriptPath: string;
  cwd: string;
  args: unknown;
  overrides: Overrides;
  status: RunStatus;
  result: unknown;
  error: string | null;
  attempt: number;
  pid: number | null;
  cancelRequested: boolean;
  createdAt: number;
  updatedAt: number;
}

export interface Call {
  runId: string;
  key: string;
  kind: CallKind;
  label: string;
  phase: string | null;
  status: CallStatus;
  /** formatted resolved agent, e.g. local/codex:gpt-5.4 */
  agent: string | null;
  space: string | null;
  handle: Record<string, unknown> | null;
  result: unknown;
  error: string | null;
  data: Record<string, unknown>;
  seq: number;
  attempt: number;
  tries: number;
  startedAt: number;
  updatedAt: number;
  finishedAt: number | null;
}

const SCHEMA = `
CREATE TABLE IF NOT EXISTS runs (
  id TEXT PRIMARY KEY,
  workflow TEXT NOT NULL,
  script_path TEXT NOT NULL,
  cwd TEXT NOT NULL,
  args TEXT NOT NULL,
  overrides TEXT NOT NULL,
  status TEXT NOT NULL,
  result TEXT,
  error TEXT,
  attempt INTEGER NOT NULL DEFAULT 0,
  pid INTEGER,
  cancel_requested INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS calls (
  run_id TEXT NOT NULL,
  key TEXT NOT NULL,
  kind TEXT NOT NULL,
  label TEXT NOT NULL,
  phase TEXT,
  status TEXT NOT NULL,
  agent TEXT,
  space TEXT,
  handle TEXT,
  result TEXT,
  error TEXT,
  data TEXT NOT NULL DEFAULT '{}',
  seq INTEGER NOT NULL,
  attempt INTEGER NOT NULL,
  tries INTEGER NOT NULL DEFAULT 1,
  started_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  finished_at INTEGER,
  PRIMARY KEY (run_id, key)
);
`;

type Row = Record<string, unknown>;

const json = (v: unknown) => (v === undefined ? null : JSON.stringify(v));
const parse = (v: unknown) => (v == null ? null : JSON.parse(String(v)));

function toRun(r: Row): Run {
  return {
    id: String(r.id),
    workflow: String(r.workflow),
    scriptPath: String(r.script_path),
    cwd: String(r.cwd),
    args: parse(r.args),
    overrides: parse(r.overrides),
    status: r.status as RunStatus,
    result: parse(r.result),
    error: (r.error as string) ?? null,
    attempt: Number(r.attempt),
    pid: r.pid == null ? null : Number(r.pid),
    cancelRequested: Number(r.cancel_requested) === 1,
    createdAt: Number(r.created_at),
    updatedAt: Number(r.updated_at),
  };
}

function toCall(r: Row): Call {
  return {
    runId: String(r.run_id),
    key: String(r.key),
    kind: r.kind as CallKind,
    label: String(r.label),
    phase: (r.phase as string) ?? null,
    status: r.status as CallStatus,
    agent: (r.agent as string) ?? null,
    space: (r.space as string) ?? null,
    handle: parse(r.handle),
    result: parse(r.result),
    error: (r.error as string) ?? null,
    data: parse(r.data) ?? {},
    seq: Number(r.seq),
    attempt: Number(r.attempt),
    tries: Number(r.tries),
    startedAt: Number(r.started_at),
    updatedAt: Number(r.updated_at),
    finishedAt: r.finished_at == null ? null : Number(r.finished_at),
  };
}

export interface CallPatch {
  status?: CallStatus;
  label?: string;
  phase?: string | null;
  agent?: string | null;
  space?: string | null;
  handle?: Record<string, unknown> | null;
  result?: unknown;
  error?: string | null;
  data?: Record<string, unknown>;
  attempt?: number;
  tries?: number;
  startedAt?: number;
  finishedAt?: number | null;
}

const PATCH_COLUMNS: Record<keyof CallPatch, [string, (v: unknown) => unknown]> = {
  status: ["status", (v) => v],
  label: ["label", (v) => v],
  phase: ["phase", (v) => v],
  agent: ["agent", (v) => v],
  space: ["space", (v) => v],
  handle: ["handle", json],
  result: ["result", (v) => (v === undefined ? null : JSON.stringify(v))],
  error: ["error", (v) => v],
  data: ["data", json],
  attempt: ["attempt", (v) => v],
  tries: ["tries", (v) => v],
  startedAt: ["started_at", (v) => v],
  finishedAt: ["finished_at", (v) => v],
};

export class Journal {
  readonly db: DatabaseSync;
  readonly now: () => number;

  constructor(path: string, now: () => number = Date.now) {
    if (path !== ":memory:") mkdirSync(dirname(path), { recursive: true });
    this.db = new DatabaseSync(path);
    this.db.exec("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;");
    this.db.exec(SCHEMA);
    this.now = now;
  }

  close(): void {
    this.db.close();
  }

  createRun(r: { id: string; workflow: string; scriptPath: string; cwd: string; args: unknown; overrides: Overrides }): Run {
    const t = this.now();
    this.db
      .prepare(
        `INSERT INTO runs (id, workflow, script_path, cwd, args, overrides, status, attempt, created_at, updated_at)
         VALUES (?, ?, ?, ?, ?, ?, 'running', 0, ?, ?)`,
      )
      .run(r.id, r.workflow, r.scriptPath, r.cwd, json(r.args ?? null), json(r.overrides), t, t);
    return this.getRun(r.id)!;
  }

  getRun(id: string): Run | undefined {
    const row = this.db.prepare("SELECT * FROM runs WHERE id = ?").get(id) as Row | undefined;
    return row ? toRun(row) : undefined;
  }

  /** Finds a run by full id or unique prefix. */
  findRun(idOrPrefix: string): Run | undefined {
    const exact = this.getRun(idOrPrefix);
    if (exact) return exact;
    const rows = this.db.prepare("SELECT * FROM runs WHERE id LIKE ? LIMIT 2").all(idOrPrefix + "%") as Row[];
    return rows.length === 1 ? toRun(rows[0]) : undefined;
  }

  listRuns(limit = 20): Run[] {
    return (this.db.prepare("SELECT * FROM runs ORDER BY created_at DESC LIMIT ?").all(limit) as Row[]).map(toRun);
  }

  /** Starts a new attempt (first run or resume). Returns the attempt number. */
  startAttempt(id: string, patch: { pid: number; overrides?: Overrides; args?: unknown }): number {
    const run = this.getRun(id);
    if (!run) throw new Error(`no run ${id}`);
    const attempt = run.attempt + 1;
    this.db
      .prepare(
        `UPDATE runs SET attempt = ?, status = 'running', error = NULL, result = NULL, pid = ?, cancel_requested = 0,
         overrides = ?, args = ?, updated_at = ? WHERE id = ?`,
      )
      .run(
        attempt,
        patch.pid,
        json(patch.overrides ?? run.overrides),
        json(patch.args === undefined ? run.args : patch.args),
        this.now(),
        id,
      );
    return attempt;
  }

  finishRun(id: string, status: RunStatus, result?: unknown, error?: string): void {
    this.db
      .prepare("UPDATE runs SET status = ?, result = ?, error = ?, pid = NULL, updated_at = ? WHERE id = ?")
      .run(status, result === undefined ? null : JSON.stringify(result), error ?? null, this.now(), id);
  }

  requestCancel(id: string): void {
    this.db.prepare("UPDATE runs SET cancel_requested = 1, updated_at = ? WHERE id = ?").run(this.now(), id);
  }

  isCancelRequested(id: string): boolean {
    const row = this.db.prepare("SELECT cancel_requested FROM runs WHERE id = ?").get(id) as Row | undefined;
    return Number(row?.cancel_requested) === 1;
  }

  getCall(runId: string, key: string): Call | undefined {
    const row = this.db.prepare("SELECT * FROM calls WHERE run_id = ? AND key = ?").get(runId, key) as Row | undefined;
    return row ? toCall(row) : undefined;
  }

  /** Inserts a new call row; seq is assigned in order of first appearance. */
  insertCall(c: {
    runId: string;
    key: string;
    kind: CallKind;
    label: string;
    phase: string | null;
    status: CallStatus;
    attempt: number;
    agent?: string | null;
    space?: string | null;
    data?: Record<string, unknown>;
  }): Call {
    const t = this.now();
    const seqRow = this.db.prepare("SELECT COALESCE(MAX(seq), 0) + 1 AS n FROM calls WHERE run_id = ?").get(c.runId) as Row;
    this.db
      .prepare(
        `INSERT INTO calls (run_id, key, kind, label, phase, status, agent, space, data, seq, attempt, started_at, updated_at)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
      )
      .run(
        c.runId,
        c.key,
        c.kind,
        c.label,
        c.phase,
        c.status,
        c.agent ?? null,
        c.space ?? null,
        json(c.data ?? {}),
        Number(seqRow.n),
        c.attempt,
        t,
        t,
      );
    return this.getCall(c.runId, c.key)!;
  }

  updateCall(runId: string, key: string, patch: CallPatch): void {
    const sets: string[] = [];
    const vals: unknown[] = [];
    for (const [k, v] of Object.entries(patch) as [keyof CallPatch, unknown][]) {
      if (v === undefined && k !== "result") continue;
      const [col, enc] = PATCH_COLUMNS[k];
      sets.push(`${col} = ?`);
      vals.push(enc(v));
    }
    sets.push("updated_at = ?");
    vals.push(this.now());
    this.db
      .prepare(`UPDATE calls SET ${sets.join(", ")} WHERE run_id = ? AND key = ?`)
      .run(...(vals as never[]), runId, key);
  }

  /** Calls touched by the given attempt (default: the run's latest), in order of first appearance. */
  listCalls(runId: string, attempt?: number): Call[] {
    const rows =
      attempt === undefined
        ? this.db.prepare("SELECT * FROM calls WHERE run_id = ? ORDER BY seq").all(runId)
        : this.db.prepare("SELECT * FROM calls WHERE run_id = ? AND attempt = ? ORDER BY seq").all(runId, attempt);
    return (rows as Row[]).map(toCall);
  }
}
