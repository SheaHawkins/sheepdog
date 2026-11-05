import assert from "node:assert/strict";
import { test } from "node:test";
import { parseResultContract } from "../src/prompt.ts";
import { echoResponder, fakeClock, makeEnv } from "./helpers.ts";

const META = `export const meta = { name: "wf", agents: { reviewer: "codex | claude", verifier: "claude" } };\n`;
const ECHO = `const Echo = schema({ echo: "string" });\n`;

test("agent returns a validated structured result", async () => {
  const env = makeEnv();
  const out = await env.run(META + ECHO + `return await agent("hello", { as: "reviewer", schema: Echo });`);
  assert.equal(out.status, "done", out.error);
  assert.deepEqual(out.result, { echo: "hello" });
  const [req] = env.backends.local.started;
  assert.equal(req.kind, "codex");
  const prompt = env.backends.local.agents.get(req.name)!.prompts[0];
  assert.ok(parseResultContract(prompt)?.schema, "prompt carries the schema contract");
});

test("agent without a schema returns text", async () => {
  const env = makeEnv();
  const out = await env.run(META + `return await agent("say hi");`);
  assert.equal(out.result, "answer: say hi");
});

test("resume replays finished calls and re-runs failed ones", async () => {
  let fail = true;
  const env = makeEnv({
    respond: (p, req, turn) => {
      if (p.startsWith("second") && fail) return { raw: "not json" };
      return echoResponder(p, req, turn);
    },
  });
  const src = META + ECHO + `
const a = await agent("first", { as: "reviewer", schema: Echo });
const b = await agent("second", { as: "reviewer", schema: Echo });
return [a.echo, b.echo];`;
  const first = await env.run(src);
  assert.equal(first.status, "failed");
  assert.match(first.error!, /isn't valid JSON/);
  // the bad agent got exactly one correction prompt
  const bad = env.backends.local.started.find((r) => r.label === "second")!;
  assert.equal(env.backends.local.agents.get(bad.name)!.prompts.length, 2);

  fail = false;
  const second = await env.run(src, { resume: first.runId });
  assert.equal(second.status, "done", second.error);
  assert.deepEqual(second.result, ["first", "second"]);
  const labels = env.backends.local.started.map((r) => r.label);
  assert.deepEqual(labels, ["first", "second", "second"], "first was replayed, not restarted");
  const calls = env.journal.listCalls(first.runId, 2);
  assert.equal(calls.find((c) => c.label === "first")!.data.replayed, true);
});

test("editing a script re-runs only the changed calls", async () => {
  const env = makeEnv();
  const v1 = META + ECHO + `return await parallel([() => agent("a", { schema: Echo }), () => agent("b", { schema: Echo })]);`;
  const v2 = v1.replace(`agent("b"`, `agent("b2"`);
  const r1 = await env.run(v1);
  assert.equal(r1.status, "done");
  const r2 = await env.run(v2, { resume: r1.runId });
  assert.equal(r2.status, "done");
  assert.deepEqual(env.backends.local.started.map((r) => r.label), ["a", "b", "b2"]);
});

test("identical calls are distinct occurrences", async () => {
  const env = makeEnv();
  const src = META + `return await parallel([() => agent("same"), () => agent("same")]);`;
  const r1 = await env.run(src);
  assert.deepEqual(r1.result, ["answer: same", "answer: same"]);
  assert.equal(env.backends.local.started.length, 2);
  await env.run(src, { resume: r1.runId });
  assert.equal(env.backends.local.started.length, 2, "both replayed");
});

test("one correction prompt fixes a bad result", async () => {
  const env = makeEnv({ respond: (p, req, turn) => (turn === 0 ? { raw: '{"wrong": 1}' } : { result: { echo: "fixed" } }) });
  const out = await env.run(META + ECHO + `return await agent("x", { schema: Echo });`);
  assert.deepEqual(out.result, { echo: "fixed" });
  const name = env.backends.local.started[0].name;
  assert.match(env.backends.local.agents.get(name)!.prompts[1], /doesn't match the schema: \$\.echo: required/);
});

test("roles: overrides win, fallbacks skip unavailable kinds, unknown roles fail", async () => {
  const env = makeEnv({ unavailable: ["codex"] });
  await env.run(META + `return await agent("x", { as: "reviewer" });`);
  assert.equal(env.backends.local.started.at(-1)!.kind, "claude", "codex unavailable → claude");

  await env.run(META + `return await agent("y", { as: "reviewer" });`, {
    overrides: { agents: { reviewer: "gemini:2.5-pro@high" }, spaces: {} },
  });
  const req = env.backends.local.started.at(-1)!;
  assert.deepEqual([req.kind, req.model, req.effort], ["gemini", "2.5-pro", "high"]);

  const bad = await env.run(META + `return await agent("z", { as: "nobody" });`);
  assert.match(bad.error!, /unknown agent role "nobody"/);

  const inline = await env.run(META + `return await agent("z", { use: "box/claude" });`);
  assert.match(inline.error!, /can't choose spaces/);
});

test("spaces are chosen at run time", async () => {
  const env = makeEnv({ spaces: ["local", "box"] });
  const src = META + `return await parallel([() => agent("r", { as: "reviewer" }), () => agent("v", { as: "verifier" })]);`;
  await env.run(src, { overrides: { agents: {}, spaces: { verifier: "box" } } });
  assert.deepEqual(env.backends.local.started.map((r) => r.label), ["r"]);
  assert.deepEqual(env.backends.box.started.map((r) => r.label), ["v"]);

  await env.run(src, { overrides: { agents: { reviewer: "box/claude" }, spaces: {}, defaultSpace: "box" } });
  assert.equal(env.backends.box.started.length, 3);

  await assert.rejects(env.run(src, { overrides: { agents: {}, spaces: {}, defaultSpace: "nope" } }), /unknown space/);
});

test("time blocked on a human doesn't count against the timeout", async () => {
  const env = makeEnv({ respond: () => ({ result: "ok", blockMs: 300, delayMs: 5 }) });
  const out = await env.run(META + `return await agent("x", { timeout: 150 });`);
  assert.equal(out.status, "done", out.error);
  assert.equal(out.result, "ok");
});

test("agents that run too long time out", async () => {
  const env = makeEnv({ respond: () => ({ result: "late", delayMs: 400 }) });
  const out = await env.run(META + `return await agent("x", { timeout: 50 });`);
  assert.equal(out.status, "failed");
  assert.match(out.error!, /timed out after 50ms/);
});

test("steps run once and replay on resume; sh only inside steps", async () => {
  const env = makeEnv();
  const src = META + `
const v = await step("count", () => sh\`echo \${"it's"} && echo "$PWD"\`);
await agent("then fail", { schema: { type: "number" } });
return v;`;
  const r1 = await env.run(src);
  assert.equal(r1.status, "failed");
  const step = env.journal.getCall(r1.runId, "step:count@local")!;
  assert.equal(step.status, "done");
  assert.match(String(step.result), /^it's\n/);
  // mark the stored result so a re-run would be visible
  env.journal.updateCall(r1.runId, "step:count@local", { result: "from-journal" });
  const r2 = await env.run(src.replace(`schema: { type: "number" }`, ``), { resume: r1.runId });
  assert.equal(r2.status, "done", r2.error);
  assert.equal(r2.result, "from-journal");

  const bare = await env.run(META + "return await sh`echo hi`;");
  assert.match(bare.error!, /only be used inside step\(\) or waitFor\(\)/);

  const dup = await env.run(META + `await step("a", () => 1); await step("a", () => 2);`);
  assert.match(dup.error!, /used twice/);
});

test("waitFor polls until a value and times out by its original deadline", async () => {
  const clock = fakeClock();
  const env = makeEnv();
  const ok = await env.run(
    META + `let n = 0; return await waitFor("ci", () => (++n >= 3 ? { conclusion: "success" } : undefined), { every: "30s" });`,
    { clock },
  );
  assert.deepEqual(ok.result, { conclusion: "success" });
  const call = env.journal.listCalls(ok.runId)[0];
  assert.equal(call.data.checks, 3);

  const t0 = clock.t;
  const late = await env.run(META + `return await waitFor("ci", () => undefined, { every: "1m", timeout: "5m" });`, { clock });
  assert.match(late.error!, /timed out after 5m/);
  assert.equal(clock.t - t0, 5 * 60_000);
});

test("cancel stops running agents and marks the run cancelled", async () => {
  const env = makeEnv({ respond: () => ({ result: "x", delayMs: 5_000 }) });
  const ac = new AbortController();
  const p = env.run(META + `return await parallel([() => agent("a"), () => agent("b")]);`, { signal: ac.signal });
  setTimeout(() => ac.abort(), 50);
  const out = await p;
  assert.equal(out.status, "cancelled");
  const calls = env.journal.listCalls(out.runId);
  assert.deepEqual(calls.map((c) => c.status), ["cancelled", "cancelled"]);
});

test("parallel limit/settle, pipeline, and the per-run agent cap", async () => {
  const env = makeEnv({ config: { limits: { concurrency: 6, maxAgents: 3, agentTimeoutMs: 60_000 } } as never });
  const out = await env.run(
    META + `
const settled = await parallel([() => 1, () => { throw new Error("boom"); }], { settle: true });
const piped = await pipeline([1, 2], (x) => x * 10, async (x, i) => x + i);
return { settled, piped };`,
  );
  assert.deepEqual(out.result, { settled: [{ ok: true, value: 1 }, { ok: false, error: "boom" }], piped: [10, 21] });

  const capped = await env.run(META + `return await parallel([1, 2, 3, 4].map((i) => () => agent("a" + i)), { limit: 2 });`);
  assert.equal(capped.status, "failed");
  assert.match(capped.error!, /at most 3 agents per run/);
});
