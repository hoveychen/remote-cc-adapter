# remote-cc-adapter

Run the [Claude Code](https://claude.com/claude-code) CLI locally, but make its
**tool calls execute on another machine** — file reads/writes and subprocesses
land in a remote sandbox — while Claude itself (the reasoning loop, tool
schemas, transcript) stays byte-for-byte native. The model cannot perceive the
split: there are no `mcp__` tool prefixes and no custom schemas, because the
tool *implementations* are untouched. Only the syscalls beneath them
(`open`/`read`/`posix_spawn`/…) are redirected.

> **Status: early implementation, POC-validated design.** The core interception
> mechanisms were validated end-to-end in a proof of concept on both macOS and
> Linux (see [`docs/design.md`](docs/design.md) §4). This repository promotes
> that POC into a structured, buildable codebase. The full Go pipeline
> (routing → relay → executor) is covered by tests; the native↔real-`claude`
> injection path and the cross-machine transport are the next milestones. See
> [Status & roadmap](#status--roadmap) for exactly what is and isn't wired.

## How it works

```
        ┌───────────────────────── brain host ─────────────────────────┐
        │                                                               │
        │   claude (re-signed copy)          remote-cc-adapter (Go)     │
        │  ┌──────────────────────┐         ┌───────────────────────┐   │
        │  │ native interceptor   │  fs     │ IO-RPC server          │   │
        │  │  macOS: interpose    │ IO-RPC  │  routing table:        │   │
        │  │  Linux: seccomp      │────────▶│   local  → brain FS    │   │
        │  │ (open/read/stat/…)   │  unix   │   remote → executor ───┼───┼──┐
        │  └──────────┬───────────┘  socket └───────────────────────┘   │  │
        │             │ posix_spawn (routed)                             │  │
        │             ▼                                                  │  │ transport
        │      rcc-spawn-proxy ─────────────────────────────────────────┼──┤ (unix socket now,
        │                                                               │  │  go-libp2p next)
        └───────────────────────────────────────────────────────────────┘  │
                                                                            ▼
                                        ┌──────────── remote sandbox ────────────┐
                                        │  rcc-executor (Go)                      │
                                        │   fs ops (open/pread slice/write/…)     │
                                        │   subprocess exec (stream stdout/stderr,│
                                        │     forward signals, real exit code)    │
                                        └─────────────────────────────────────────┘
```

1. The **adapter** spawns `claude` with a native interceptor injected and serves
   a filesystem IO-RPC socket.
2. The **interceptor** (macOS interpose dylib / Linux seccomp supervisor) catches
   the process's `open`/`read`/`stat`/`readdir` calls. Routed paths are forwarded
   to the adapter; everything else falls through to the real local syscall so the
   CLI boots, reads its credentials, and writes `~/.claude` locally.
3. The adapter consults its **routing table** and either serves the op on the
   brain host's filesystem or relays it to the remote **executor**.
4. Subprocesses (`Bash`, ripgrep for `Grep`/`Glob`, `git status`, …) are
   rewritten to run on the executor via **`rcc-spawn-proxy`**, so metadata-heavy
   traversals happen remotely and only the result crosses the wire.

Why syscall-level interception rather than MCP replacement tools or a
`can_use_tool` rewrite? Because those leak into what the model sees (tool names,
schemas) or only cover `Bash`. Redirecting the syscalls beneath the native tools
covers *all* tools with zero distribution shift. The full rationale — and the
three rejected designs — is in [`docs/design.md`](docs/design.md) §2.

## Repository layout

| Path | What |
|---|---|
| [`cmd/remote-cc-adapter`](cmd/remote-cc-adapter) | Brain-side host: spawns `claude` injected, serves IO-RPC, routes ops. |
| [`cmd/rcc-executor`](cmd/rcc-executor) | Remote sidecar: runs fs ops + subprocesses on the sandbox host. |
| [`cmd/rcc-spawn-proxy`](cmd/rcc-spawn-proxy) | Stands in for a routed subprocess; streams it from the executor. |
| [`internal/protocol`](internal/protocol) | Binary fs IO-RPC wire format (shared C↔Go↔Go). |
| [`internal/execproto`](internal/execproto) | Streaming subprocess protocol (proxy↔executor). |
| [`internal/routing`](internal/routing) | Path routing table (remote-allowlist / default-remote). |
| [`internal/transport`](internal/transport) | Brain↔executor link: Unix socket (now), go-libp2p (stub). |
| [`internal/executor`](internal/executor) | fs + subprocess services and stream multiplexing. |
| [`internal/adapter`](internal/adapter) | IO-RPC server, routing relay, claude launch/injection. |
| [`native/macos`](native/macos) | DYLD interpose dylib. |
| [`native/linux`](native/linux) | seccomp-user-notify supervisor. |
| [`docs/design.md`](docs/design.md) | Full design + POC results. |

## Build

```sh
make            # Go binaries into ./bin + the host platform's interceptor
make go         # just the Go binaries
make native     # just the native interceptor for this platform
make test       # go test ./...
```

Requirements: Go 1.24+, a C compiler. On Linux, building the supervisor needs
kernel headers; running it needs `CAP_SYS_ADMIN` (or a user-namespace setup).

## Run (local end-to-end, executor co-located)

This wires everything over a local Unix socket — the transport used when the
brain and sandbox are the same host. Cross-machine runs await the go-libp2p
transport (roadmap below).

```sh
make

# 1. Start the executor (this is the "remote" side; here it's local).
./bin/rcc-executor -sock /tmp/rcc-exec.sock &

# 2. Launch claude through the adapter. On macOS, --resign prepares an ad-hoc
#    re-signed copy so the dylib can load (your real claude is untouched).
./bin/remote-cc-adapter \
  --claude "$(command -v claude)" \
  --resign \
  --dylib ./native/macos/rcc_interpose.dylib \
  --spawn-proxy ./bin/rcc-spawn-proxy \
  --executor-sock /tmp/rcc-exec.sock \
  --remote-prefix "$PWD" \
  -- -p "list the files in this directory"
```

On Linux, drop `--resign`/`--dylib` and pass
`--supervisor ./native/linux/rcc_seccomp` instead.

`--print-cmd` prints the assembled launch command and injected environment
without spawning anything — handy for inspecting exactly what would run.

## Routing

Two policies (`internal/routing`):

- **remote-allowlist** (default): everything is local except `--remote-prefix`
  paths. This is the surgical routing the end-to-end POC validated — `claude`
  boots and reads its own config locally, only your work paths route remote.
- **default-remote** (`--default-remote`, design target): everything is remote
  except `--local-prefix` paths (e.g. `~/.claude`, CLI internals). More
  aggressive; enable deliberately.

`~/.claude` (global `CLAUDE.md`, skills, memory, settings) must always resolve
locally — the adapter never routes a user's global config to the sandbox.

## Status & roadmap

**Verified in this repo (tests + local builds + CI):**

- Binary IO-RPC and subprocess protocols round-trip (`internal/protocol`,
  `internal/execproto`).
- Routing table for both policies (`internal/routing`).
- Adapter ↔ executor end-to-end over real Unix sockets: `stat`/`open`/`pread`
  slice/`write`/`readdir` route to the correct side with handle namespacing
  (`internal/adapter` integration test).
- Executor subprocess streaming: stdout/stderr split, exit-code fidelity
  (`internal/executor` exec test).
- Both native interceptors compile clean (macOS dylib, Linux seccomp), verified
  on macOS and in a Linux container.
- **Real `claude` injection, end-to-end on macOS** (`scripts/e2e-local.sh`): a
  re-signed claude with the dylib injected has both Read and Bash redirected to
  the executor. Read of a routed file shows OPEN+PREAD in the executor log and
  returns the file's content; Bash run with claude's cwd under the remote prefix
  executes on the executor (observable via the `RCC_EXECUTOR=1` marker) — routed
  **naturally by cwd, no sentinel**. claude's own credential/self spawns
  (`security`, the claude binary) stay local via the interceptor's local-binary
  allowlist.

- **Remote Grep/Glob, correct and via one remote ripgrep** (real claude): claude
  runs Grep/Glob by re-exec'ing itself as its embedded ripgrep (it enters that
  mode when `argv[0]`'s basename is `rg`). Recursive Glob over a routed project
  finds nested files (dirent types carry `DTDir` so ripgrep recurses), and the
  rg engine is routed to the executor as a single native subprocess with
  `argv[0]` preserved — so its directory walk runs on the executor's real
  filesystem in one pass instead of as a per-syscall fs-interpose metadata storm
  (design doc §4.1 pt5).

**Subprocess routing (macOS)** decides remote-vs-local per spawn, highest
precedence first: rg-mode self-invocation under a remote cwd → local-binary
allowlist → sentinel in argv → target under a remote prefix → working directory
under a remote prefix → local. This lets a subprocess of a remote-routed project
run where the project lives while claude's own tooling stays local. Routed
children run with the injection environment stripped (no re-injection), and with
`argv[0]` preserved so argv[0]-sensitive binaries behave correctly.

**Next milestones:**

- **Natural routing of bare relative fs opens.** Files opened by absolute path
  (what claude's Read/Write tools do) route correctly; `open("rel")` /
  `openat(AT_FDCWD, "rel")` are left local because routing every relative open
  against the cwd destabilises claude's boot. Needs a safer cwd-scoped policy
  (design doc §4.3).

- Wire the **go-libp2p** transport (DCUtR hole-punching + circuit-relay
  fallback, Noise/TLS, PeerID == public key) so brain and sandbox can be on
  different machines — currently a stub (`internal/transport/libp2p.go`, design
  doc §3.3).
- Linux **lazy slicing**: today the supervisor fetches whole routed files into a
  memfd because seccomp only traps `openat`; slicing needs trapping
  `read`/`lseek` too or a FUSE backing store (design doc §4.1.3, §4.3).
- `run_in_background` detach-poll semantics for backgrounded `Bash` (design doc
  §4.3).

See [`docs/design.md`](docs/design.md) for the complete design and the POC
evidence behind every claim above.

## License

MIT — see [LICENSE](LICENSE).
