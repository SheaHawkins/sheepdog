import { execFile } from "node:child_process";
import { join } from "node:path";
import type { AgentKindConfig } from "../config.ts";
import type { AgentState, AgentStatus, Backend, Handle, StartRequest } from "../backend.ts";
import { AbortedError } from "../util.ts";

/**
 * herdr backend, driven through the `herdr` CLI (every command prints JSON).
 * Written against herdr 0.9.x docs. JSON field names are read defensively because they
 * haven't been verified against a live server yet; see `pick*` helpers.
 */

const BINARIES: Record<string, string> = {
  claude: "claude",
  codex: "codex",
  opencode: "opencode",
  gemini: "gemini",
  cursor: "cursor-agent",
  amp: "amp",
  pi: "pi",
  grok: "grok",
};

/** How each agent CLI takes a model / reasoning effort. Unknown kinds get `--model`. */
function modelArgs(kind: string, model?: string, effort?: string): string[] {
  const out: string[] = [];
  switch (kind) {
    case "codex":
      if (model) out.push("-m", model);
      if (effort) out.push("-c", `model_reasoning_effort="${effort === "max" ? "xhigh" : effort}"`);
      break;
    case "gemini":
      if (model) out.push("-m", model);
      break;
    default:
      if (model) out.push("--model", model);
  }
  return out;
}

export class HerdrError extends Error {
  code: string;
  constructor(code: string, message: string) {
    super(`herdr: ${code}: ${message}`);
    this.name = "HerdrError";
    this.code = code;
  }
}

export interface HerdrOptions {
  machine?: string;
  session?: string;
  agents?: Record<string, AgentKindConfig>;
  /** where isolated worktrees are created */
  worktreeRoot: string;
  bin?: string;
}

type Json = Record<string, any>;

export class HerdrBackend implements Backend {
  #opts: HerdrOptions;
  #available = new Map<string, Promise<boolean>>();
  /** runId → run workspace (for isolation "none") */
  #workspaces = new Map<string, Promise<{ workspace: string; rootPane?: string }>>();

  constructor(opts: HerdrOptions) {
    this.#opts = opts;
  }

  #base(): string[] {
    const a: string[] = [];
    if (this.#opts.machine) a.push("--machine", this.#opts.machine);
    if (this.#opts.session) a.push("--session", this.#opts.session);
    return a;
  }

  #exec(args: string[], signal?: AbortSignal): Promise<Json> {
    return new Promise((resolve, reject) => {
      execFile(
        this.#opts.bin ?? "herdr",
        [...this.#base(), ...args],
        { signal, maxBuffer: 32 * 1024 * 1024 },
        (err, stdout, stderr) => {
          if (signal?.aborted) return reject(new AbortedError());
          if (err) {
            if ((err as NodeJS.ErrnoException).code === "ENOENT") {
              return reject(new HerdrError("not_installed", "the herdr CLI isn't on PATH (https://herdr.dev)"));
            }
            try {
              const e = JSON.parse(stderr).error ?? {};
              return reject(new HerdrError(e.code ?? "error", e.message ?? stderr.trim()));
            } catch {
              return reject(new HerdrError("error", (stderr || err.message).trim()));
            }
          }
          try {
            resolve(stdout.trim() ? JSON.parse(stdout) : {});
          } catch {
            resolve({ text: stdout });
          }
        },
      );
    });
  }

  isAvailable(kind: string): Promise<boolean> {
    let p = this.#available.get(kind);
    if (!p) {
      // Remote machines: trust the selector; herdr reports a clear error if the agent is missing.
      p = this.#opts.machine ? Promise.resolve(true) : onPath(BINARIES[kind] ?? kind);
      this.#available.set(kind, p);
    }
    return p;
  }

  async start(req: StartRequest, signal?: AbortSignal): Promise<Handle> {
    let cwd = req.cwd;
    let workspace: string;
    let pane: string | undefined;

    if (req.isolation === "worktree") {
      const path = join(this.#opts.worktreeRoot, req.runId, req.name);
      const r = await this.#exec(
        ["worktree", "create", "--cwd", req.cwd, "--branch", `sheepdog/${req.runId}/${req.name}`, "--path", path,
          "--label", `${req.workflow}: ${req.label}`.slice(0, 60), "--no-focus"],
        signal,
      );
      cwd = path;
      workspace = pickId(r.result?.workspace, "workspace_id");
      pane = pickOptionalId(r.result?.root_pane ?? r.result?.pane, "pane_id");
    } else {
      const ws = await this.#runWorkspace(req, signal);
      workspace = ws.workspace;
      if (ws.rootPane) {
        pane = ws.rootPane;
        ws.rootPane = undefined; // first agent takes the workspace's root pane
      }
    }

    if (!pane) {
      const r = await this.#exec(
        ["tab", "create", "--workspace", workspace, "--cwd", cwd, "--label", req.label.slice(0, 40), "--no-focus"],
        signal,
      );
      pane = pickId(r.result?.root_pane ?? r.result?.pane, "pane_id");
    }

    const extra = [...modelArgs(req.kind, req.model, req.effort), ...(this.#opts.agents?.[req.kind]?.args ?? [])];
    await this.#exec(
      ["agent", "start", req.name, "--kind", req.kind, "--pane", pane, "--timeout", "60000", ...(extra.length ? ["--", ...extra] : [])],
      signal,
    );
    return { name: req.name, cwd, pane, workspace, machine: this.#opts.machine, session: this.#opts.session };
  }

  async #runWorkspace(req: StartRequest, signal?: AbortSignal) {
    let p = this.#workspaces.get(req.runId);
    if (!p) {
      p = this.#exec(
        ["workspace", "create", "--cwd", req.cwd, "--label", `sheepdog: ${req.workflow}`.slice(0, 60), "--no-focus"],
        signal,
      ).then((r) => ({
        workspace: pickId(r.result?.workspace, "workspace_id"),
        rootPane: pickOptionalId(r.result?.root_pane, "pane_id"),
      }));
      this.#workspaces.set(req.runId, p);
      p.catch(() => this.#workspaces.delete(req.runId));
    }
    return p;
  }

  async prompt(h: Handle, text: string, signal?: AbortSignal): Promise<void> {
    await this.#exec(["agent", "prompt", h.name, text], signal);
  }

  async wait(h: Handle, until: AgentState[], opts: { timeoutMs?: number; signal?: AbortSignal }) {
    const args = ["agent", "wait", h.name];
    for (const s of until) args.push("--until", s);
    if (opts.timeoutMs !== undefined) args.push("--timeout", String(Math.max(1, Math.round(opts.timeoutMs))));
    try {
      const r = await this.#exec(args, opts.signal);
      return toStatus(r.result?.agent ?? r.result);
    } catch (e) {
      if (e instanceof HerdrError && /timeout|timed_out/i.test(e.code + " " + e.message)) return "timeout" as const;
      throw e;
    }
  }

  async status(h: Handle): Promise<AgentStatus | undefined> {
    const agent = await this.#findAgent(h.name);
    return agent ? toStatus(agent) : undefined;
  }

  async alive(h: Handle): Promise<boolean> {
    try {
      return (await this.#findAgent(h.name)) !== undefined;
    } catch {
      return false;
    }
  }

  async #findAgent(name: string): Promise<Json | undefined> {
    const r = await this.#exec(["agent", "list"]);
    const list: Json[] = r.result?.agents ?? (Array.isArray(r.result) ? r.result : []);
    return list.find((a) => a.name === name || a.agent_name === name);
  }

  async stop(h: Handle): Promise<void> {
    await this.#exec(["agent", "send-keys", h.name, "ctrl+c"]).catch(() => {});
  }

  async focus(h: Handle): Promise<void> {
    await this.#exec(["agent", "focus", h.name]);
  }
}

function toStatus(a: Json | undefined): AgentStatus {
  const raw = String(a?.agent_status ?? a?.status ?? a?.state ?? "unknown").toLowerCase();
  const state: AgentState = (["working", "blocked", "idle", "done"] as const).find((s) => s === raw) ?? "unknown";
  const seq = a?.completion_seq ?? a?.completionSeq;
  return { state, completionSeq: typeof seq === "number" ? seq : undefined };
}

function pickOptionalId(v: unknown, field: string): string | undefined {
  if (typeof v === "string") return v;
  if (v && typeof v === "object") {
    const o = v as Json;
    const id = o[field] ?? o.id;
    if (typeof id === "string") return id;
  }
  return undefined;
}

function pickId(v: unknown, field: string): string {
  const id = pickOptionalId(v, field);
  if (!id) throw new HerdrError("unexpected_response", `couldn't find ${field} in ${JSON.stringify(v)}`);
  return id;
}

function onPath(bin: string): Promise<boolean> {
  return new Promise((resolve) => execFile("/bin/sh", ["-c", `command -v ${JSON.stringify(bin)}`], (err) => resolve(!err)));
}
