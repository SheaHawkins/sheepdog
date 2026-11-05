/**
 * Result schemas. `schema(shape)` turns a compact shape into JSON Schema:
 *   "string" | "number" | "integer" | "boolean" | "null" | "any"   (suffix "?" = optional)
 *   [shape]                                                          array of shape
 *   { key: shape, "key?": shape }                                    object ("?" on the key = optional)
 * Anywhere a schema is accepted, a raw JSON Schema object works too.
 */

export type JSONSchema = { [k: string]: unknown };

const SCALARS = ["string", "number", "integer", "boolean", "null"];

export function schema(shape: unknown): JSONSchema {
  return compact(shape, "$");
}

function compact(shape: unknown, path: string): JSONSchema {
  if (typeof shape === "string") {
    const t = shape.endsWith("?") ? shape.slice(0, -1) : shape;
    if (t === "any") return {};
    if (!SCALARS.includes(t)) throw new Error(`schema ${path}: unknown type "${shape}"`);
    return { type: t };
  }
  if (Array.isArray(shape)) {
    if (shape.length !== 1) throw new Error(`schema ${path}: array shapes take exactly one element shape`);
    return { type: "array", items: compact(shape[0], `${path}[]`) };
  }
  if (shape && typeof shape === "object") {
    const properties: Record<string, JSONSchema> = {};
    const required: string[] = [];
    for (const [rawKey, sub] of Object.entries(shape)) {
      const optKey = rawKey.endsWith("?");
      const key = optKey ? rawKey.slice(0, -1) : rawKey;
      const optType = typeof sub === "string" && sub.endsWith("?");
      properties[key] = compact(sub, `${path}.${key}`);
      if (!optKey && !optType) required.push(key);
    }
    return { type: "object", properties, required };
  }
  throw new Error(`schema ${path}: invalid shape ${JSON.stringify(shape)}`);
}

function typeOf(v: unknown): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  return typeof v;
}

function matchesType(t: string, v: unknown): boolean {
  switch (t) {
    case "integer":
      return Number.isInteger(v);
    case "number":
      return typeof v === "number" && Number.isFinite(v);
    case "object":
      return typeOf(v) === "object";
    default:
      return typeOf(v) === t;
  }
}

/** Validates the subset of JSON Schema sheepdog uses. Returns human-readable errors (empty = valid). */
export function validate(s: JSONSchema, v: unknown, path = "$"): string[] {
  const errs: string[] = [];
  if (s.type !== undefined) {
    const types = Array.isArray(s.type) ? (s.type as string[]) : [s.type as string];
    if (!types.some((t) => matchesType(t, v))) {
      return [`${path}: expected ${types.join(" | ")}, got ${typeOf(v)}`];
    }
  }
  if (Array.isArray(s.enum) && !s.enum.some((e) => JSON.stringify(e) === JSON.stringify(v))) {
    errs.push(`${path}: must be one of ${s.enum.map((e) => JSON.stringify(e)).join(", ")}`);
  }
  if ("const" in s && JSON.stringify(s.const) !== JSON.stringify(v)) {
    errs.push(`${path}: must equal ${JSON.stringify(s.const)}`);
  }
  if (Array.isArray(s.anyOf) && !s.anyOf.some((sub) => validate(sub as JSONSchema, v, path).length === 0)) {
    errs.push(`${path}: doesn't match any allowed shape`);
  }
  if (typeOf(v) === "object") {
    const o = v as Record<string, unknown>;
    const props = (s.properties ?? {}) as Record<string, JSONSchema>;
    for (const k of (s.required ?? []) as string[]) {
      if (!(k in o)) errs.push(`${path}.${k}: required`);
    }
    for (const [k, sub] of Object.entries(props)) {
      if (k in o) errs.push(...validate(sub, o[k], `${path}.${k}`));
    }
    if (s.additionalProperties === false) {
      for (const k of Object.keys(o)) if (!(k in props)) errs.push(`${path}.${k}: not allowed`);
    }
  }
  if (Array.isArray(v) && s.items) {
    v.forEach((item, i) => errs.push(...validate(s.items as JSONSchema, item, `${path}[${i}]`)));
  }
  return errs;
}

/** Builds a minimal value that satisfies a schema (used by the simulate backend). */
export function sampleOf(s: JSONSchema, hint = "x"): unknown {
  if (Array.isArray(s.enum)) return s.enum[0];
  if ("const" in s) return s.const;
  const t = Array.isArray(s.type) ? s.type[0] : s.type;
  switch (t) {
    case "string":
      return `sample ${hint}`;
    case "number":
    case "integer":
      return 1;
    case "boolean":
      return true;
    case "null":
      return null;
    case "array":
      return [sampleOf((s.items ?? {}) as JSONSchema, hint), sampleOf((s.items ?? {}) as JSONSchema, hint + "2")];
    case "object": {
      const out: Record<string, unknown> = {};
      for (const [k, sub] of Object.entries((s.properties ?? {}) as Record<string, JSONSchema>)) {
        out[k] = sampleOf(sub, k);
      }
      return out;
    }
    default:
      return null;
  }
}
