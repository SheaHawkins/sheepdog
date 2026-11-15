# Spike 4: build and run the binary on macOS, Linux and Windows

Question: does sheepdog build as one static binary for each OS, and does that binary run a graph on each?

Status: **in progress.** The build question is settled. The run question is settled on macOS, covered by CI on Linux and Windows against a fake herdr, and still open for real agents on Linux and Windows.

## What was built

- `cmd/sheepdog`, the first binary. It has only what the spike needs:
  - `sheepdog [--session NAME] graph run FILE.ts ["instruction"] [-i KEY=VALUE]...`
  - `sheepdog [--session NAME] run resume RUN_ID`
  - `sheepdog version`

  The supervisor runs in the foreground and prints the result as JSON. Agents get a new herdr workspace rather than the run's own tab. `graph run` takes a file path because there is no graph registry yet.
- `internal/herdr/fakeherdr`, a stand-in `herdr` binary. It answers the commands `herdr.CLI` sends and keeps its state in a JSON file. Its agents finish every prompt at once and write the result file the prompt names.
- `TestSmoke` builds both binaries and puts the fake first on `PATH`. It then runs a graph with a plain-text call, a schema call, `parallel()` across two labels and a relative import. After that it resumes the finished run, and finally resumes the run again after deleting the journal entries for the parallel calls.
- `.github/workflows/ci.yml`:
  - unit tests on ubuntu, macos and windows runners (`-race` everywhere except Windows)
  - cross-builds of all six targets, uploaded as a workflow artifact
  - a check that herdr runs headless on each OS: it starts a server, opens a pane and runs a command in it

## Findings

**Cross-compiling is a non-issue.** Everything is pure Go: esbuild, goja, the schema validator and `x/sys/windows`. With `CGO_ENABLED=0` all six targets build from one machine, and `go vet` is clean for each. Process liveness for the run lock is the only OS-specific code (`kill -0` on Unix, `OpenProcess` on Windows).

**The binary works end to end on macOS with real herdr and claude.** `examples/hello.ts`, with one plain-text call and one schema call, finished in 16s. `run resume` on the finished run replayed it in 0.03s without sending a prompt.

**Windows path handling was already broken in a test.** The node tests located the result path with a regex that expected it to start with `/`, so they would have failed on Windows. Product code builds result paths with `filepath`, so it was not affected. CI on Windows is what catches this kind of bug.

**Smaller observations:**

- Plain-text results keep the agent's trailing newline (`"PELICAN\n"`). Trimming it is probably right but is left for later.
- `run resume` opens a new herdr workspace even when every call replays from the journal. The real CLI should open it only when a live call needs an agent.
- herdr's installers always fetch the latest release, so the CI herdr check is not pinned.

## Still to do

- [ ] CI green on all three runners.
- [ ] Real agents on Windows: the spike 2 and 3 herdr tests. See below.
- [ ] Real agents on Linux, in a container or on a machine.

Risks for real agents on Windows:

- agents copying `C:\…\result.json` paths with backslashes intact
- the delivery check finding a wrapped Windows path in the pane
- `run.json` renames and lock removal failing while another process has the file open
- herdr's agent status detection and restore behaviour under ConPTY

## Running it on Windows

In PowerShell:

1. Install Go 1.26, then herdr with `powershell -ExecutionPolicy Bypass -c "irm https://herdr.dev/install.ps1 | iex"`. Install claude and log in.
2. Clone the repository and run `go test ./...`. Leave out `-race`, because it needs a C compiler.
3. In a second terminal, run `herdr --session sheepdog-spike` to watch the agents.
4. Make a scratch folder, for example `C:\scratch\proj`. Start `claude` there once and accept its folder trust prompt, then quit.
5. From the repository:

   ```powershell
   $env:SHEEPDOG_HERDR_SESSION = "sheepdog-spike"
   $env:SHEEPDOG_HERDR_CWD     = "C:\scratch\proj"
   go test ./internal/node -run HerdrEndToEnd -v -timeout 10m
   go test ./internal/run  -run KillAndResume -v -timeout 10m
   ```

6. Try the binary itself:

   ```powershell
   go build -o sheepdog.exe ./cmd/sheepdog
   cd C:\scratch\proj
   path\to\sheepdog.exe --session sheepdog-spike graph run path\to\sheepdog\examples\hello.ts PELICAN
   ```

Leave out the restart test (`SHEEPDOG_HERDR_RESTART`) for now, because it stops the whole herdr server.
