import { existsSync, readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";
import type { Limits } from "./scheduler.ts";
import { parseDuration } from "./util.ts";

export interface SpaceConfig {
  /** "herdr" (default) or "simulate" */
  backend?: string;
  /** herdr --machine */
  machine?: string;
  /** herdr --session */
  session?: string;
  /** max concurrent agents in this space */
  limit?: number;
}

export interface AgentKindConfig {
  /** extra CLI args passed when starting this agent kind */
  args?: string[];
}

export interface Config {
  defaultSpace: string;
  spaces: Record<string, SpaceConfig>;
  agents: Record<string, AgentKindConfig>;
  limits: Limits & { agentTimeoutMs: number };
  /** resolution order for "*" */
  preference?: string[];
}

export function configDir(): string {
  return process.env.SHEEPDOG_CONFIG_DIR ?? join(process.env.XDG_CONFIG_HOME ?? join(homedir(), ".config"), "sheepdog");
}

export function stateDir(): string {
  return process.env.SHEEPDOG_STATE_DIR ?? join(process.env.XDG_STATE_HOME ?? join(homedir(), ".local", "state"), "sheepdog");
}

export const DEFAULT_CONFIG: Config = {
  defaultSpace: "local",
  spaces: {
    local: { backend: "herdr" },
    simulate: { backend: "simulate" },
  },
  agents: {},
  limits: { concurrency: 6, maxAgents: 50, agentTimeoutMs: parseDuration("20m") },
};

export function loadConfig(dir = configDir()): Config {
  const file = join(dir, "config.json");
  if (!existsSync(file)) return structuredClone(DEFAULT_CONFIG);
  let raw: Record<string, unknown>;
  try {
    raw = JSON.parse(readFileSync(file, "utf8"));
  } catch (e) {
    throw new Error(`${file}: ${(e as Error).message}`);
  }
  const limits = (raw.limits ?? {}) as Record<string, unknown>;
  const cfg: Config = {
    defaultSpace: (raw.defaultSpace as string) ?? DEFAULT_CONFIG.defaultSpace,
    spaces: { ...DEFAULT_CONFIG.spaces, ...((raw.spaces as Record<string, SpaceConfig>) ?? {}) },
    agents: (raw.agents as Record<string, AgentKindConfig>) ?? {},
    limits: {
      concurrency: Number(limits.concurrency ?? DEFAULT_CONFIG.limits.concurrency),
      maxAgents: Number(limits.maxAgents ?? DEFAULT_CONFIG.limits.maxAgents),
      agentTimeoutMs:
        limits.agentTimeout !== undefined
          ? parseDuration(limits.agentTimeout as string)
          : DEFAULT_CONFIG.limits.agentTimeoutMs,
    },
    preference: raw.preference as string[] | undefined,
  };
  if (!cfg.spaces[cfg.defaultSpace]) throw new Error(`${file}: defaultSpace "${cfg.defaultSpace}" isn't defined in spaces`);
  return cfg;
}
