# Spike 1: TypeScript through esbuild and goja

Question: can the binary run a graph file through embedded esbuild and goja, with `await` and a blocking Go function behind `agent()`?

Answer: yes. The code is in `internal/script`, and the tests run the example graph from the spec unchanged apart from defining its two schemas. The "blocking `agent()`" fallback in the risk table is not needed.

## What was built

- `Compile(path)`: strips types and bundles relative imports with esbuild's Go API, returning one JavaScript file with an inline source map.
- `Meta(program)`: evaluates `meta` without running the graph, which is what `graph add` needs.
- `Run(ctx, program, args, host)`: runs the script and returns what it returns. `Host.Agent` is an ordinary blocking Go function, the shape a `herdr agent prompt --wait` call will have.

## Findings

**The graph language is not valid as a single ES module, so the compiler wraps it.** A graph file mixes `export const meta`, top-level `await` and top-level `return`. esbuild rejects that combination in every output format: top-level `return` is illegal in a module, and top-level `await` is illegal outside one. `Compile` therefore splits the file into a header (imports and `meta`) and a body, and wraps the body in `export default async function`. Two rules follow, and the spec should state them:

- Imports and `export const meta` must come first. A file that does not start with `meta` (after any imports) is rejected with a clear error.
- `meta` is the only export. Another `export` is an error.

The header is found by a small scanner that handles comments, strings, template literals and bracket nesting, but nothing else. It only reads the header; esbuild parses everything after it.

**goja handles `async`/`await` well.** `agent()` returns a goja promise and runs the blocking Go call in a goroutine. A settle loop on the JS goroutine resolves each promise as its call finishes, so goja is only ever touched from one goroutine. Because `agent()` returns at once, `parallel()` is just `Promise.all` and runs calls concurrently: three 50ms validator calls take 50ms, not 150ms. The tests check this, and pass under `-race`.

**Errors point at the user's file.** The wrapper opens on the same line as the body, so line numbers survive, and goja reads esbuild's inline source map. A `throw` on line 11 is reported as `errors.ts:11:8`. A file that ends mid-expression would otherwise blame the wrapper's closing brace past the end of the file, so that case is reported as `unexpected end of file`.

**TypeScript drops unused imports.** An unused `import x from 'lodash'` compiles cleanly, because esbuild removes it before resolving. Only imports a graph actually uses hit the relative-imports-only check. This is harmless, but worth knowing.

**Determinism is enforced by removing the clock and randomness.** `Math.random`, `Date.now` and `new Date()` with no arguments throw, with a message that says why. `new Date(value)` still works.

**Small decisions made along the way:**

- `agent()` results reach the script through `JSON.parse`, so they are real arrays and objects with `.map` and `.filter`, not wrapped Go values.
- A script awaiting something that can never finish (no `agent()` in flight) fails with an error instead of hanging.
- `maxCalls` is enforced in the runtime, and a call over the cap rejects.
- With no inputs, `args` is `{}` rather than `null`.

**The binary stays portable.** With esbuild and goja linked in, a small harness cross-compiles with `CGO_ENABLED=0` for macOS arm64, Linux amd64 and Windows amd64, at 21–22 MB unstripped. That is a quick check, not spike 4: the binaries were built for all three, but only run on macOS.

## Not covered

- `graph()` (calling another graph) and journaling or replay. Replay will sit behind `Host.Agent`; spike 3 covers it.
- Stopping a run: goja's `Interrupt` and context cancellation are wired in the obvious places, but not tested.
- `sheepdog graph types`, which writes `sheepdog.d.ts`.
