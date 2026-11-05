import { existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { validate, type JSONSchema } from "./schema.ts";

/**
 * The result contract: every agent prompt ends with an instruction to write the final
 * result to a file. The file (not the agent's idle state) is the completion signal.
 */

const PATH_TAG = "SHEEPDOG_RESULT_PATH:";
const SCHEMA_TAG = "SHEEPDOG_RESULT_SCHEMA:";

export function buildPrompt(task: string, resultPath: string, schema?: JSONSchema): string {
  const lines = [
    task.trimEnd(),
    "",
    "---",
    "When you are finished, write your final result to this file:",
    `${PATH_TAG} ${resultPath}`,
  ];
  if (schema) {
    lines.push(
      "The file must contain only JSON (no prose, no code fences) that matches this JSON Schema:",
      `${SCHEMA_TAG} ${JSON.stringify(schema)}`,
    );
  } else {
    lines.push("Write your final answer there as plain text or Markdown.");
  }
  lines.push("Write that file last, after all other work is done: it tells sheepdog you are finished.");
  return lines.join("\n");
}

export function correctionPrompt(resultPath: string, problem: string, schema?: JSONSchema): string {
  return [
    `sheepdog couldn't use your result: ${problem}`,
    `Please write it again to ${resultPath}.`,
    schema ? `It must be only JSON matching: ${JSON.stringify(schema)}` : "",
  ]
    .filter(Boolean)
    .join("\n");
}

export function parseResultContract(prompt: string): { path: string; schema?: JSONSchema } | undefined {
  const pathLine = prompt.split("\n").find((l) => l.startsWith(PATH_TAG));
  if (!pathLine) return undefined;
  const schemaLine = prompt.split("\n").find((l) => l.startsWith(SCHEMA_TAG));
  return {
    path: pathLine.slice(PATH_TAG.length).trim(),
    schema: schemaLine ? JSON.parse(schemaLine.slice(SCHEMA_TAG.length).trim()) : undefined,
  };
}

/**
 * Result files live in <agent cwd>/.sheepdog/out/. That directory ignores itself in git, so
 * worktrees and repos stay clean, and it is inside the agent's workspace so sandboxed agents can write it.
 */
export function resultPathFor(cwd: string, runId: string, key: string, json: boolean): string {
  const dir = join(cwd, ".sheepdog", "out");
  mkdirSync(dir, { recursive: true });
  const ignore = join(dir, ".gitignore");
  if (!existsSync(ignore)) writeFileSync(ignore, "*\n");
  return join(dir, `${runId}-${key.replace(/[^a-z0-9]/gi, "_")}.${json ? "json" : "md"}`);
}

export function clearResult(path: string): void {
  rmSync(path, { force: true });
}

export type ReadResult = { ok: true; value: unknown } | { ok: false; missing: boolean; error: string };

export function readResult(path: string, schema?: JSONSchema): ReadResult {
  if (!existsSync(path)) return { ok: false, missing: true, error: `no result file at ${path}` };
  const text = readFileSync(path, "utf8");
  if (!schema) return { ok: true, value: text.trim() };
  let value: unknown;
  try {
    value = JSON.parse(stripFences(text));
  } catch (e) {
    return { ok: false, missing: false, error: `result file isn't valid JSON (${(e as Error).message})` };
  }
  const errs = validate(schema, value);
  if (errs.length) return { ok: false, missing: false, error: `result doesn't match the schema: ${errs.slice(0, 5).join("; ")}` };
  return { ok: true, value };
}

function stripFences(t: string): string {
  const m = /^\s*```(?:json)?\s*\n([\s\S]*?)\n\s*```\s*$/.exec(t);
  return m ? m[1] : t;
}

export function ensureDir(p: string) {
  mkdirSync(dirname(p), { recursive: true });
}
