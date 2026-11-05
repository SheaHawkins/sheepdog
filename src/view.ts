import type { Backend, Handle } from "./backend.ts";
import type { Call, Journal, Run } from "./journal.ts";
import { formatDuration } from "./util.ts";

// The live view and the plain log both write to a terminal only when stderr/stdout is one.
const useColor = () => !process.env.NO_COLOR && !!process.stderr.isTTY;
const sgr = (code: string) => (s: string) => (useColor() ? `\x1b[${code}m${s}\x1b[0m` : s);
const c = {
  dim: sgr("2"),
  bold: sgr("1"),
  red: sgr("31"),
  green: sgr("32"),
  yellow: sgr("33"),
  cyan: sgr("36"),
  inverse: sgr("7"),
};

export function icon(call: Call): string {
  if (call.status === "done") return call.data.replayed ? c.dim("↺") : c.green("✓");
  if (call.status === "failed") return c.red("✗");
  if (call.status === "cancelled") return c.dim("⊘");
  if (call.status === "blocked") return c.yellow("⚠");
  if (call.kind === "wait" || call.kind === "sleep") return c.cyan("⏳");
  return c.cyan("●");
}

function runIcon(run: Run): string {
  switch (run.status) {
    case "done":
      return c.green("✓ done");
    case "failed":
      return c.red("✗ failed");
    case "cancelled":
      return c.dim("⊘ cancelled");
    default:
      return c.cyan("⟳ running");
  }
}

function detail(call: Call, now: number): string {
  if (call.kind === "agent") return call.agent ?? "";
  if (call.kind === "wait") return `${call.space ?? ""} · ${call.data.checks ?? 0} checks`;
  if (call.kind === "sleep") {
    const left = Number(call.data.wakeAt) - now;
    return call.status === "running" && left > 0 ? `wakes in ${formatDuration(left)}` : "";
  }
  return call.space ?? "";
}

function elapsed(call: Call, now: number): string {
  if (call.data.replayed) return "cached";
  return formatDuration((call.finishedAt ?? now) - call.startedAt);
}

/** Visible width, ignoring ANSI escapes (good enough for our glyphs). */
function vlen(s: string): number {
  return [...s.replace(/\x1b\[[0-9;]*m/g, "")].length;
}

function fit(s: string, width: number): string {
  const n = vlen(s);
  if (n <= width) return s + " ".repeat(width - n);
  const plain = [...s.replace(/\x1b\[[0-9;]*m/g, "")];
  return plain.slice(0, Math.max(0, width - 1)).join("") + "…";
}

export interface RenderOptions {
  width: number;
  selected?: number;
  interactive?: boolean;
  now?: number;
  message?: string;
}

/** Calls of the run's latest attempt, ordered for display (grouped by phase in order of appearance). */
export function displayCalls(journal: Journal, run: Run): Call[] {
  const calls = journal.listCalls(run.id, run.attempt);
  const order: string[] = [];
  const groups = new Map<string, Call[]>();
  for (const call of calls) {
    const p = call.phase ?? "";
    if (!groups.has(p)) {
      groups.set(p, []);
      order.push(p);
    }
    groups.get(p)!.push(call);
  }
  return order.flatMap((p) => groups.get(p)!);
}

export function render(journal: Journal, runId: string, o: RenderOptions): string[] {
  const run = journal.getRun(runId);
  if (!run) return [`no run ${runId}`];
  const now = o.now ?? Date.now();
  const w = Math.max(40, o.width);
  const calls = displayCalls(journal, run);
  const lines: string[] = [];
  const dur = formatDuration((run.status === "running" ? now : run.updatedAt) - run.createdAt);
  lines.push(`${c.bold("sheepdog")} · ${run.workflow} · ${c.dim(run.id)} · ${runIcon(run)} ${c.dim(dur)}`);
  if (run.attempt > 1) lines.push(c.dim(`attempt ${run.attempt} (resumed)`));
  lines.push("");

  const labelW = Math.max(16, Math.floor((w - 16) * 0.5));
  const detailW = Math.max(10, w - labelW - 16);
  let phase: string | null | undefined;
  calls.forEach((call, i) => {
    if (call.phase !== phase) {
      phase = call.phase;
      lines.push(c.bold(phase ?? "(no phase)"));
    }
    const sel = o.interactive && i === o.selected;
    const row = `${sel ? "›" : " "} ${icon(call)} ${fit(call.label, labelW)} ${fit(c.dim(detail(call, now)), detailW)} ${c.dim(elapsed(call, now))}`;
    lines.push(sel ? c.bold(row) : row);
    if (call.status === "blocked") lines.push(`      ${c.yellow("needs you")} ${c.dim(o.interactive ? "· enter to jump to the agent" : "· answer it in its herdr pane")}`);
    if (call.status === "failed" && call.error) lines.push(`      ${c.red(fit(call.error.split("\n")[0], w - 6))}`);
  });
  if (!calls.length) lines.push(c.dim("  (no calls yet)"));

  if (run.status === "failed" && run.error) {
    lines.push("", c.red("error: ") + run.error.split("\n")[0]);
  }
  lines.push("");
  if (o.message) lines.push(c.yellow(o.message));
  if (o.interactive) {
    lines.push(c.dim(run.status === "running" ? "↑↓ select · enter jump to agent · x cancel run · q close view" : "↑↓ select · enter jump to agent · q quit"));
  }
  return lines;
}

export interface LiveViewOptions {
  journal: Journal;
  runId: string;
  backendFor: (space: string) => Backend;
  /** "run": this process is the runner (q closes the view, the run continues headless). */
  mode: "run" | "watch";
  /** force line-based output even on a TTY */
  plain?: boolean;
  onQuit?: () => void;
}

/** Full-screen live view on a TTY; prints status transitions otherwise. */
export class LiveView {
  #o: LiveViewOptions;
  #tty: boolean;
  #selected = 0;
  #timer?: NodeJS.Timeout;
  #active = false;
  #message?: string;
  #lastStatus = new Map<string, string>();
  #onKey = (d: Buffer) => this.#key(d.toString());

  constructor(o: LiveViewOptions) {
    this.#o = o;
    this.#tty = !o.plain && !!process.stdout.isTTY && !!process.stdin.isTTY;
  }

  get interactive() {
    return this.#tty;
  }

  start() {
    this.#active = true;
    if (this.#tty) {
      process.stdout.write("\x1b[?1049h\x1b[?25l");
      process.stdin.setRawMode(true);
      process.stdin.resume();
      process.stdin.on("data", this.#onKey);
      this.#timer = setInterval(() => this.update(), 500);
    }
    this.update();
  }

  update() {
    if (!this.#active) return;
    if (!this.#tty) return this.#logTransitions();
    const lines = render(this.#o.journal, this.#o.runId, {
      width: process.stdout.columns || 80,
      selected: this.#selected,
      interactive: true,
      message: this.#message,
    });
    const rows = process.stdout.rows || 40;
    process.stdout.write("\x1b[H\x1b[2J" + lines.slice(0, rows - 1).join("\n"));
  }

  /** Leaves the full-screen view and prints a final static frame. */
  stop() {
    if (!this.#active) return;
    this.#active = false;
    if (this.#tty) {
      clearInterval(this.#timer);
      process.stdin.off("data", this.#onKey);
      process.stdin.setRawMode(false);
      process.stdin.pause();
      process.stdout.write("\x1b[?25h\x1b[?1049l");
      const lines = render(this.#o.journal, this.#o.runId, { width: process.stdout.columns || 80 });
      process.stdout.write(lines.join("\n") + "\n");
    } else {
      this.#logTransitions();
    }
  }

  #logTransitions() {
    const run = this.#o.journal.getRun(this.#o.runId);
    if (!run) return;
    const now = Date.now();
    for (const call of this.#o.journal.listCalls(run.id, run.attempt)) {
      const s = call.status + (call.data.replayed ? ":replayed" : "");
      if (this.#lastStatus.get(call.key) === s) continue;
      this.#lastStatus.set(call.key, s);
      const extra = call.status === "failed" && call.error ? ` — ${call.error.split("\n")[0]}` : "";
      process.stderr.write(`${icon(call)} ${call.label} ${c.dim(detail(call, now))} ${c.dim(elapsed(call, now))}${extra}\n`);
    }
  }

  #calls(): Call[] {
    const run = this.#o.journal.getRun(this.#o.runId);
    return run ? displayCalls(this.#o.journal, run) : [];
  }

  #key(k: string) {
    const calls = this.#calls();
    this.#message = undefined;
    if (k === "\x1b[A" || k === "k") this.#selected = Math.max(0, this.#selected - 1);
    else if (k === "\x1b[B" || k === "j") this.#selected = Math.min(Math.max(0, calls.length - 1), this.#selected + 1);
    else if (k === "\r" || k === "\n") void this.#jump(calls[this.#selected]);
    else if (k === "x") {
      this.#o.journal.requestCancel(this.#o.runId);
      this.#message = "cancelling…";
    } else if (k === "q" || k === "\x03") {
      if (k === "\x03" && this.#o.mode === "run") {
        this.#o.journal.requestCancel(this.#o.runId);
        this.#message = "cancelling… (the run stops its agents first)";
      } else {
        this.stop();
        this.#o.onQuit?.();
        return;
      }
    }
    this.update();
  }

  async #jump(call: Call | undefined) {
    if (!call || call.kind !== "agent" || !call.handle || !call.space) {
      this.#message = "select an agent to jump to it";
      return this.update();
    }
    const backend = this.#o.backendFor(call.space);
    if (!backend.focus) {
      this.#message = `space "${call.space}" has no panes to jump to`;
      return this.update();
    }
    try {
      await backend.focus(call.handle as Handle);
    } catch (e) {
      this.#message = `couldn't focus agent: ${(e as Error).message}`;
    }
    this.update();
  }
}
