/**
 * A backend runs agents in one space. The runner owns prompts, results and the journal;
 * backends only start agents, deliver text and report state.
 */

export type AgentState = "working" | "blocked" | "idle" | "done" | "unknown";

export interface AgentStatus {
  state: AgentState;
  /** Increments each time the agent finishes a turn, when the backend knows it. */
  completionSeq?: number;
}

export interface StartRequest {
  /** Unique live name: [a-z][a-z0-9_-]{0,31} */
  name: string;
  kind: string;
  model?: string;
  effort?: string;
  /** Working directory for isolation "none". */
  cwd: string;
  isolation: "none" | "worktree";
  label: string;
  runId: string;
  workflow: string;
}

/** Serializable: stored in the journal so a later process can reattach. */
export interface Handle {
  name: string;
  /** Directory the agent works in (the worktree when isolated). */
  cwd: string;
  [k: string]: unknown;
}

export interface Backend {
  isAvailable(kind: string): Promise<boolean>;
  start(req: StartRequest, signal?: AbortSignal): Promise<Handle>;
  prompt(h: Handle, text: string, signal?: AbortSignal): Promise<void>;
  /** Resolves with the agent's status once it is in one of `until`, or "timeout". */
  wait(
    h: Handle,
    until: AgentState[],
    opts: { timeoutMs?: number; signal?: AbortSignal },
  ): Promise<AgentStatus | "timeout">;
  status(h: Handle): Promise<AgentStatus | undefined>;
  /** Is this handle still a live agent? */
  alive(h: Handle): Promise<boolean>;
  /** Interrupt the agent (used on cancel). */
  stop(h: Handle): Promise<void>;
  /** Bring the agent's pane to the front, if the backend has a UI. */
  focus?(h: Handle): Promise<void>;
}
