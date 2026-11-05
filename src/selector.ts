/**
 * Agent selectors:
 *   selector := choice ( "|" choice )*            fallbacks, tried in order
 *   choice   := [ space "/" ] ( kind [ ":" model ] [ "@" effort ] | "*" [ "@" effort ] )
 *
 * Scripts may not name a space; only run-time overrides and config can.
 */

export const EFFORTS = ["low", "medium", "high", "xhigh", "max"] as const;

/** Order used to resolve "*". */
export const DEFAULT_PREFERENCE = ["claude", "codex", "opencode", "gemini", "cursor", "amp", "pi", "grok"];

export interface Choice {
  space?: string;
  kind: string;
  model?: string;
  effort?: string;
}

export interface ResolvedAgent {
  space: string;
  kind: string;
  model?: string;
  effort?: string;
}

export class SelectorError extends Error {
  constructor(message: string) {
    super(message);
    this.name = "SelectorError";
  }
}

const CHOICE_RE = /^(?:([a-z0-9][a-z0-9_-]*)\/)?([a-z][a-z0-9_-]*|\*)(?::([^@\s]+))?(?:@([a-z]+))?$/i;

export function parseSelector(src: string, opts: { allowSpace?: boolean } = {}): Choice[] {
  if (typeof src !== "string" || !src.trim()) throw new SelectorError("empty agent selector");
  const parts = src.split("|").map((s) => s.trim());
  return parts.map((part) => {
    const m = CHOICE_RE.exec(part);
    if (!m) throw new SelectorError(`invalid selector choice "${part}" in "${src}" (expected kind[:model][@effort])`);
    const [, space, kind, model, effort] = m;
    if (space && !opts.allowSpace) {
      throw new SelectorError(
        `selector "${src}" names a space ("${space}/"). Scripts can't choose spaces; use --space or --agent role=${space}/... at run time`,
      );
    }
    if (kind === "*" && model) throw new SelectorError(`"*" can't take a model in "${src}"`);
    if (effort && !(EFFORTS as readonly string[]).includes(effort.toLowerCase())) {
      throw new SelectorError(`unknown effort "@${effort}" in "${src}" (use ${EFFORTS.join(", ")})`);
    }
    return { space, kind: kind.toLowerCase(), model, effort: effort?.toLowerCase() };
  });
}

export function formatAgent(a: ResolvedAgent): string {
  return `${a.space}/${a.kind}${a.model ? ":" + a.model : ""}${a.effort ? "@" + a.effort : ""}`;
}

export interface ResolveContext {
  /** Space used by choices that don't name one. */
  defaultSpace: string;
  isAvailable(space: string, kind: string): Promise<boolean>;
  preference?: string[];
}

export async function resolveSelector(choices: Choice[], ctx: ResolveContext): Promise<ResolvedAgent> {
  const tried: string[] = [];
  for (const c of choices) {
    const space = c.space ?? ctx.defaultSpace;
    const kinds = c.kind === "*" ? (ctx.preference ?? DEFAULT_PREFERENCE) : [c.kind];
    for (const kind of kinds) {
      if (await ctx.isAvailable(space, kind)) return { space, kind, model: c.model, effort: c.effort };
      tried.push(`${space}/${kind}`);
    }
  }
  throw new SelectorError(`no available agent (tried ${tried.join(", ")})`);
}
