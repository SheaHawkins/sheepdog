import { readFileSync } from "node:fs";
import { stripTypeScriptTypes } from "node:module";
import { basename, resolve } from "node:path";
import { parseSelector } from "./selector.ts";

export interface Meta {
  name: string;
  description?: string;
  phases?: { title: string; detail?: string }[];
  /** role → selector */
  agents?: Record<string, string>;
}

export interface Script {
  path: string;
  meta: Meta;
  /** Plain JS function body (types stripped, `export` removed). */
  body: string;
}

/** Names injected into every workflow script. Order matters: it's the parameter list. */
export const GLOBALS = ["agent", "parallel", "pipeline", "phase", "schema", "step", "waitFor", "sleep", "sh", "args"];

const META_RE = /export\s+const\s+meta\s*(?::[^=]+)?=\s*/;

export function loadScript(path: string): Script {
  const abs = resolve(path);
  return parseScript(readFileSync(abs, "utf8"), abs);
}

export function parseScript(source: string, path = "<inline>"): Script {
  if (/^\s*import\s[^(]/m.test(source)) {
    throw new Error(`${path}: import statements aren't supported in workflow scripts yet`);
  }
  const m = META_RE.exec(source);
  if (!m) throw new Error(`${path}: missing \`export const meta = { name, ... }\``);
  const litStart = m.index + m[0].length;
  if (source[litStart] !== "{") throw new Error(`${path}: meta must be an object literal`);
  const litEnd = matchBrace(source, litStart);
  const literal = source.slice(litStart, litEnd + 1);

  let meta: Meta;
  try {
    meta = new Function(`"use strict"; return (${literal});`)() as Meta;
  } catch (e) {
    throw new Error(`${path}: meta must be a plain literal (no variables or calls): ${(e as Error).message}`);
  }
  validateMeta(meta, path);

  // Keep `meta` visible to the body as a local const; drop the `export`.
  const rest = source.slice(0, m.index) + "const meta = " + source.slice(litStart);
  if (/^\s*export\s/m.test(rest)) throw new Error(`${path}: only \`meta\` may be exported`);

  const PRE = "async function __sheepdog__() {\n";
  const POST = "\n}";
  let stripped: string;
  try {
    stripped = stripTypeScriptTypes(PRE + rest + POST, { mode: "strip" });
  } catch (e) {
    throw new Error(`${path}: ${(e as Error).message}`);
  }
  const body = stripped.slice(PRE.length, stripped.length - POST.length);
  return { path, meta, body };
}

export type WorkflowFn = (...globals: unknown[]) => Promise<unknown>;

export function compileScript(script: Script): WorkflowFn {
  const AsyncFunction = Object.getPrototypeOf(async () => {}).constructor as new (...a: string[]) => WorkflowFn;
  try {
    return new AsyncFunction(...GLOBALS, `"use strict";\n${script.body}\n//# sourceURL=${script.path}`);
  } catch (e) {
    throw new Error(`${script.path}: ${(e as Error).message}`);
  }
}

function validateMeta(meta: Meta, path: string): void {
  if (!meta || typeof meta !== "object") throw new Error(`${path}: meta must be an object`);
  if (typeof meta.name !== "string" || !meta.name) throw new Error(`${path}: meta.name is required`);
  if (meta.agents !== undefined) {
    if (typeof meta.agents !== "object" || meta.agents === null) throw new Error(`${path}: meta.agents must map role → selector`);
    for (const [role, sel] of Object.entries(meta.agents)) {
      try {
        parseSelector(sel, { allowSpace: false });
      } catch (e) {
        throw new Error(`${path}: meta.agents.${role}: ${(e as Error).message}`);
      }
    }
  }
}

/** Index of the brace matching the one at `start`, skipping strings, templates and comments. */
function matchBrace(src: string, start: number): number {
  let depth = 0;
  for (let i = start; i < src.length; i++) {
    const c = src[i];
    if (c === "/" && src[i + 1] === "/") {
      i = src.indexOf("\n", i);
      if (i < 0) break;
      continue;
    }
    if (c === "/" && src[i + 1] === "*") {
      i = src.indexOf("*/", i + 2) + 1;
      if (i <= 0) break;
      continue;
    }
    if (c === '"' || c === "'" || c === "`") {
      for (i++; i < src.length && src[i] !== c; i++) if (src[i] === "\\") i++;
      continue;
    }
    if (c === "{") depth++;
    else if (c === "}" && --depth === 0) return i;
  }
  throw new Error("unterminated meta literal");
}

export function workflowNameFromPath(p: string): string {
  return basename(p).replace(/\.(ts|js|mts|mjs)$/, "");
}
