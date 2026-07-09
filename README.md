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
> that POC into a structured, buildable codebase: the full Go pipeline is tested,
> real-`claude` injection is verified end-to-end on macOS (Read/Write/Bash/
> subagent/Grep/Glob), and filesystem IO-RPC runs over go-libp2p between hosts.
> Bridging the subprocess path across machines and NAT-traversal field testing
> are the next milestones. See [Status & roadmap](#status--roadmap) for exactly
> what is and isn't wired.

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
| [`cmd/rcc-fuse`](cmd/rcc-fuse) | Linux: FUSE daemon backing injected fds with lazy slices (`internal/linuxfuse`). |
| [`internal/protocol`](internal/protocol) | Binary fs IO-RPC wire format (shared C↔Go↔Go). |
| [`internal/execproto`](internal/execproto) | Streaming subprocess protocol (proxy↔executor). |
| [`internal/routing`](internal/routing) | Path routing table (remote-allowlist / default-remote). |
| [`internal/transport`](internal/transport) | Brain↔executor link: Unix socket + go-libp2p. |
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

## Run (cross-machine over libp2p)

To put the executor on a different machine, run it in libp2p mode and point the
adapter at its peer address. The link is Noise-secured and identified by PeerID
(== public key), with DCUtR hole-punching for NAT traversal.

```sh
# On the sandbox host: listen over libp2p; it prints its PeerID + multiaddrs.
./bin/rcc-executor -libp2p /ip4/0.0.0.0/tcp/4001
# -> serving on 12D3Koo... /ip4/.../tcp/4001/p2p/12D3Koo...

# On the brain host: dial that peer instead of a unix socket.
./bin/remote-cc-adapter \
  --claude "$(command -v claude)" --resign \
  --dylib ./native/macos/rcc_interpose.dylib \
  --spawn-proxy ./bin/rcc-spawn-proxy \
  --peer /ip4/<SANDBOX_IP>/tcp/4001/p2p/12D3Koo... \
  --remote-prefix "$PWD" --workdir "$PWD" \
  -- -p "read the files here"
```

Both filesystem IO-RPC and subprocesses (`Bash`, the ripgrep engine) flow over
libp2p: the spawn proxy connects to a local exec-bridge socket the adapter
serves, and the adapter splices each exec stream to the executor over the shared
libp2p connection. The proxy never speaks libp2p itself.

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

- **Filesystem IO-RPC over go-libp2p** (`internal/transport`, tested): two
  loopback libp2p hosts (Noise-secured, PeerID-addressed) carry the adapter ↔
  executor fs-RPC; a remote-routed `stat`/`open`/`pread` is served on the peer
  while local paths stay on the brain. `rcc-executor -libp2p` prints its PeerID +
  multiaddrs; `remote-cc-adapter --peer <multiaddr>` dials it. DCUtR
  hole-punching and circuit-relay fallback are enabled in the host config.
- **Subprocesses over go-libp2p** (`internal/adapter`, tested): the adapter's
  exec bridge splices the spawn proxy's local unix connection to a libp2p stream,
  so a command runs on a libp2p-remote executor end-to-end. The bridge is also
  in-path co-located and does not disturb the real-claude Bash e2e.
- **Linux lazy slicing over FUSE** (`internal/linuxfuse`, `cmd/rcc-fuse`,
  `native/linux`; verified in a privileged container by `scripts/linux-fuse-test.sh`):
  the seccomp supervisor redirects a routed `openat` to a FUSE-backed file served
  by `rcc-fuse`, which fetches each read as an on-demand slice from the adapter.
  A raw consumer reads a 25-byte slice at 5 MiB of a 10 MiB routed file through
  the full FUSE → adapter → executor chain and only **4096 bytes** cross the
  fs-RPC — no whole-file materialisation, and the target's other reads are
  untouched (design doc §4.1.3 / §4.3).

**Subprocess routing (macOS)** decides remote-vs-local per spawn, highest
precedence first: rg-mode self-invocation under a remote cwd → local-binary
allowlist → sentinel in argv → target under a remote prefix → working directory
under a remote prefix → local. This lets a subprocess of a remote-routed project
run where the project lives while claude's own tooling stays local. Routed
children run with the injection environment stripped (no re-injection), and with
`argv[0]` preserved so argv[0]-sensitive binaries behave correctly.

**Next milestones:**

- **NAT-traversal field testing.** Hole-punching + relay options are enabled but
  only loopback-tested; real cross-NAT verification needs two hosts and a relay.
- **Natural routing of bare relative fs opens.** Files opened by absolute path
  (what claude's Read/Write tools do) route correctly; `open("rel")` /
  `openat(AT_FDCWD, "rel")` are left local because routing every relative open
  against the cwd destabilises claude's boot. Needs a safer cwd-scoped policy
  (design doc §4.3).

- `run_in_background` detach-poll semantics for backgrounded `Bash` (design doc
  §4.3).

See [`docs/design.md`](docs/design.md) for the complete design and the POC
evidence behind every claim above.

## License

MIT — see [LICENSE](LICENSE).
