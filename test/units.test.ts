import assert from "node:assert/strict";
import { test } from "node:test";
import { schema, validate } from "../src/schema.ts";
import { parseScript } from "../src/script.ts";
import { parseSelector, resolveSelector } from "../src/selector.ts";
import { parseDuration, stableStringify } from "../src/util.ts";
import { Scheduler } from "../src/scheduler.ts";
import { shellQuote } from "../src/runner.ts";

test("parseSelector: kinds, models, effort, fallbacks", () => {
  assert.deepEqual(parseSelector("codex"), [{ space: undefined, kind: "codex", model: undefined, effort: undefined }]);
  assert.deepEqual(parseSelector("claude:opus@high | codex:gpt-5.4"), [
    { space: undefined, kind: "claude", model: "opus", effort: "high" },
    { space: undefined, kind: "codex", model: "gpt-5.4", effort: undefined },
  ]);
  assert.equal(parseSelector("opencode:anthropic/claude-sonnet")[0].model, "anthropic/claude-sonnet");
  assert.equal(parseSelector("*@low")[0].kind, "*");
});

test("parseSelector: spaces only when allowed", () => {
  assert.throws(() => parseSelector("box2/codex"), /can't choose spaces/);
  assert.equal(parseSelector("box2/codex", { allowSpace: true })[0].space, "box2");
  assert.equal(parseSelector("box2/opencode:anthropic/x", { allowSpace: true })[0].model, "anthropic/x");
});

test("parseSelector: errors", () => {
  assert.throws(() => parseSelector(""), /empty/);
  assert.throws(() => parseSelector("codex | "), /invalid/);
  assert.throws(() => parseSelector("codex@turbo"), /unknown effort/);
  assert.throws(() => parseSelector("*:model"), /can't take a model/);
});

test("resolveSelector: first available wins, * uses preference", async () => {
  const avail = new Set(["local/codex", "box/claude"]);
  const ctx = { defaultSpace: "local", isAvailable: async (s: string, k: string) => avail.has(`${s}/${k}`) };
  assert.equal((await resolveSelector(parseSelector("claude | codex"), ctx)).kind, "codex");
  assert.deepEqual(await resolveSelector(parseSelector("*"), ctx), { space: "local", kind: "codex", model: undefined, effort: undefined });
  assert.equal((await resolveSelector(parseSelector("box/claude", { allowSpace: true }), ctx)).space, "box");
  await assert.rejects(resolveSelector(parseSelector("gemini"), ctx), /no available agent \(tried local\/gemini\)/);
});

test("schema: compact shapes and validation", () => {
  const s = schema({ findings: [{ file: "string", line: "integer", "note?": "string" }], ok: "boolean?" });
  assert.deepEqual(validate(s, { findings: [{ file: "a", line: 3 }] }), []);
  assert.deepEqual(validate(s, { findings: [{ file: "a", line: 3.5 }] }), ["$.findings[0].line: expected integer, got number"]);
  assert.deepEqual(validate(s, {}), ["$.findings: required"]);
  assert.deepEqual(validate(s, { findings: "x" }), ["$.findings: expected array, got string"]);
  assert.throws(() => schema({ a: "str" }), /unknown type/);
});

test("parseScript: meta, types stripped, top-level return", () => {
  const s = parseScript(`
// comment with { brace
export const meta = { name: "t", agents: { r: "codex | claude" }, note: "}" };
const n: number = 2;
return n * 2;`);
  assert.equal(s.meta.name, "t");
  assert.equal(s.meta.agents?.r, "codex | claude");
  assert.match(s.body, /return n \* 2/);
  assert.doesNotMatch(s.body, /: number/);
});

test("parseScript: rejects imports, extra exports, bad meta", () => {
  assert.throws(() => parseScript(`import x from "y";\nexport const meta = { name: "t" };`), /import/);
  assert.throws(() => parseScript(`export const meta = { name: "t" };\nexport const y = 1;`), /only `meta`/);
  assert.throws(() => parseScript(`const x = 1;`), /missing/);
  assert.throws(() => parseScript(`export const meta = { name: x };`), /plain literal/);
  assert.throws(() => parseScript(`export const meta = { name: "t", agents: { r: "box/codex" } };`), /meta.agents.r/);
});

test("durations and stable hashing", () => {
  assert.equal(parseDuration("30s"), 30_000);
  assert.equal(parseDuration("1.5h"), 5_400_000);
  assert.throws(() => parseDuration("soon"));
  assert.equal(stableStringify({ b: 1, a: [1, { d: 2, c: undefined }] }), '{"a":[1,{"d":2}],"b":1}');
});

test("shellQuote", () => {
  assert.equal(shellQuote("it's"), `'it'\\''s'`);
  assert.equal(shellQuote(["a b", "c"]), `'a b' 'c'`);
});

test("Scheduler: concurrency and spawn cap", async () => {
  const s = new Scheduler({ concurrency: 1, maxAgents: 2 });
  const r1 = await s.acquire("local", true);
  let second = false;
  const p = s.acquire("local", true).then((r) => {
    second = true;
    return r;
  });
  await new Promise((r) => setImmediate(r));
  assert.equal(second, false);
  r1();
  (await p)();
  await assert.rejects(s.acquire("local", true), /at most 2 agents/);
  (await s.acquire("local", false))(); // reattach doesn't count
});
