import { existsSync, readdirSync, statSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { configDir } from "./config.ts";
import { loadScript, workflowNameFromPath } from "./script.ts";

export type WorkflowSource = "repo" | "user" | "builtin";

export interface WorkflowEntry {
  name: string;
  path: string;
  source: WorkflowSource;
  description?: string;
  error?: string;
  /** a same-named workflow it overrides */
  shadows?: WorkflowSource;
}

export const BUILTIN_DIR = resolve(dirname(fileURLToPath(import.meta.url)), "..", "workflows");

export function findRepoRoot(cwd: string): string | undefined {
  let d = resolve(cwd);
  for (;;) {
    if (existsSync(join(d, ".git"))) return d;
    const up = dirname(d);
    if (up === d) return undefined;
    d = up;
  }
}

/** Search order: repo (.sheepdog/workflows), user (~/.config/sheepdog/workflows), built-in. */
export function workflowDirs(cwd: string): { dir: string; source: WorkflowSource }[] {
  const dirs: { dir: string; source: WorkflowSource }[] = [];
  const root = findRepoRoot(cwd);
  if (root) dirs.push({ dir: join(root, ".sheepdog", "workflows"), source: "repo" });
  dirs.push({ dir: join(configDir(), "workflows"), source: "user" });
  dirs.push({ dir: BUILTIN_DIR, source: "builtin" });
  return dirs;
}

function scriptsIn(dir: string): string[] {
  if (!existsSync(dir) || !statSync(dir).isDirectory()) return [];
  return readdirSync(dir)
    .filter((f) => /\.(ts|js)$/.test(f) && !f.endsWith(".d.ts"))
    .sort()
    .map((f) => join(dir, f));
}

export function listWorkflows(cwd: string): WorkflowEntry[] {
  const byName = new Map<string, WorkflowEntry>();
  for (const { dir, source } of workflowDirs(cwd)) {
    for (const path of scriptsIn(dir)) {
      const name = workflowNameFromPath(path);
      const prior = byName.get(name);
      if (prior) {
        prior.shadows ??= source;
        continue;
      }
      const entry: WorkflowEntry = { name, path, source };
      try {
        entry.description = loadScript(path).meta.description;
      } catch (e) {
        entry.error = (e as Error).message;
      }
      byName.set(name, entry);
    }
  }
  return [...byName.values()];
}

/** A path (contains "/" or ends in .ts/.js) or a workflow name. */
export function findWorkflow(nameOrPath: string, cwd: string): string {
  if (nameOrPath.includes("/") || /\.(ts|js)$/.test(nameOrPath)) {
    const p = resolve(cwd, nameOrPath);
    if (!existsSync(p)) throw new Error(`no such file: ${p}`);
    return p;
  }
  for (const { dir } of workflowDirs(cwd)) {
    for (const ext of [".ts", ".js"]) {
      const p = join(dir, nameOrPath + ext);
      if (existsSync(p)) return p;
    }
  }
  throw new Error(`no workflow named "${nameOrPath}" (see \`sheepdog ls\`)`);
}
