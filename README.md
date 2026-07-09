# remote-cc-adapter (`rca`)

Run a CLI — typically [Claude Code](https://claude.com/claude-code) — locally,
but make its **tool calls execute on another machine**: file reads/writes and
subprocesses land in a remote sandbox, while the process itself (and for
claude, the reasoning loop, tool schemas, transcript) stays byte-for-byte
native on your machine. The model cannot perceive the split: there are no
`mcp__` tool prefixes and no custom schemas, because the tool *implementations*
are untouched. Only the syscalls beneath them (`open`/`read`/`posix_spawn`/…)
are redirected.

One Go binary, two sides:

```sh
# On the remote machine (the sandbox where files/subprocesses should live):
remote$ rca serve
pairing code:

  rca1.JgAkCAESIPuT...

# On your local machine (where claude runs and you type):
local$ cd ~/work/project
local$ rca claude --code rca1.JgAkCAESIPuT...
```

`claude` starts locally — interactive TUI and `-p` one-shots both work, since
stdin/stdout never cross the network — but everything it reads, writes, greps,
and every `Bash` command it runs happens on the remote. The link is go-libp2p:
Noise-encrypted, addressed by PeerID (== public key), with DCUtR hole-punching
for NAT traversal. The pairing code is self-contained (PeerID + addresses);
there is no rendezvous server.

> **Status: early implementation, POC-validated design.** The interception
> mechanisms were validated end-to-end on macOS and Linux (see
> [`docs/design.md`](docs/design.md) §4): real-`claude` injection with
> Read/Write/Bash/subagent/Grep/Glob redirected, filesystem IO-RPC and
> subprocess streams over go-libp2p, Linux lazy slicing over FUSE.
> NAT-traversal field testing across real networks is the next milestone. See
> [Status & roadmap](#status--roadmap).

## How it works

```
        ┌───────────────────────── local host ─────────────────────────┐
        │                                                               │
        │   claude (re-signed copy)               rca (run mode)        │
        │  ┌──────────────────────┐         ┌───────────────────────┐   │
        │  │ native interceptor   │  fs     │ IO-RPC server          │   │
        │  │  macOS: interpose    │ IO-RPC  │  routing table:        │   │
        │  │  Linux: seccomp      │────────▶│   local  → local FS    │   │
        │  │ (open/read/stat/…)   │  unix   │   remote → executor ───┼───┼──┐
        │  └──────────┬───────────┘  socket └───────────────────────┘   │  │
        │             │ posix_spawn (routed)                             │  │ go-libp2p
        │             ▼                                                  │  │ (or unix socket
        │      rca _spawn-proxy ────────────────────────────────────────┼──┤  when co-located)
        │                                                               │  │
        └───────────────────────────────────────────────────────────────┘  │
                                                                            ▼
                                        ┌──────────── remote sandbox ────────────┐
                                        │  rca serve                              │
                                        │   fs ops (open/pread slice/write/…)     │
                                        │   subprocess exec (stream stdout/stderr,│
                                        │     forward signals, real exit code)    │
                                        └─────────────────────────────────────────┘
```

1. **Run mode** (`rca <command>`) spawns the target with a native interceptor
   injected and serves a filesystem IO-RPC socket. The interceptor artifact
   (macOS interpose dylib / Linux seccomp supervisor) is embedded in the `rca`
   binary and extracted to the user cache dir at runtime.
2. The **interceptor** catches the process's `open`/`read`/`stat`/`readdir`
   calls. Routed paths are forwarded to the adapter; everything else falls
   through to the real local syscall so the CLI boots, reads its credentials,
   and writes `~/.claude` locally.
3. The adapter consults its **routing table** and either serves the op on the
   local filesystem or relays it to the remote **executor** (`rca serve`).
4. Subprocesses (`Bash`, ripgrep for `Grep`/`Glob`, `git status`, …) are
   rewritten to run on the executor via `rca _spawn-proxy`, so metadata-heavy
   traversals happen remotely and only the result crosses the wire.

Why syscall-level interception rather than MCP replacement tools or a
`can_use_tool` rewrite? Because those leak into what the model sees (tool
names, schemas) or only cover `Bash`. Redirecting the syscalls beneath the
native tools covers *all* tools with zero distribution shift. The full
rationale — and the three rejected designs — is in
[`docs/design.md`](docs/design.md) §2.

## Build

```sh
make            # native interceptor for this platform, then rca (embeds it) into ./bin
make test       # go test ./...
```

Requirements: Go 1.25+, a C compiler. On Linux, building the supervisor needs
kernel headers; running it needs `CAP_SYS_ADMIN` (or a user-namespace setup).

A plain `go build ./cmd/rca` also works but embeds no interceptor; run mode
then needs `--dylib` / `--supervisor` pointing at an external build.

## Run

### Cross-machine (pairing code)

```sh
# Remote sandbox host — prints the pairing code (PeerID + dialable addrs):
remote$ rca serve

# Local host — run any command against it:
local$ cd ~/work/project
local$ rca claude --code rca1....                 # interactive TUI
local$ rca claude --code rca1.... -p "list files" # non-interactive
```

`rca`'s own flags may appear anywhere on the line; anything else after the
command name goes to the command verbatim, and everything after a literal `--`
is never interpreted by `rca`. Defaults in run mode:

- `--remote-prefix` defaults to the working directory — the project you `cd`
  into is what lives remotely; claude's config/credentials stay local.
- On macOS, `rca` runs an ad-hoc re-signed **copy** of the target so the
  interpose dylib can load (`--resign=false` to disable; the real binary is
  never touched).
- The spawn proxy and interceptor come from the `rca` binary itself.

### Co-located (one host, unix socket)

Useful for testing the full pipeline without a second machine:

```sh
rca serve --sock /tmp/rcc-exec.sock &
rca --sock /tmp/rcc-exec.sock --remote-prefix "$PWD" claude -p "list the files here"
```

`--print-cmd` prints the assembled launch command and injected environment
without spawning anything; `--peer <multiaddr>` dials a raw libp2p address if
you'd rather not use a pairing code.

## Routing

Two policies (`internal/routing`):

- **remote-allowlist** (default): everything is local except `--remote-prefix`
  paths (default: the working directory). This is the surgical routing the
  end-to-end POC validated — `claude` boots and reads its own config locally,
  only your work paths route remote.
- **default-remote** (`--default-remote`, design target): everything is remote
  except `--local-prefix` paths. More aggressive; enable deliberately.

`~/.claude` (global `CLAUDE.md`, skills, memory, settings) and `~/.claude.json`
always resolve locally — even under `--default-remote`, `rca` pins them so a
user's global config and credentials never route to the sandbox.

**Subprocess routing (macOS)** decides remote-vs-local per spawn, highest
precedence first: rg-mode self-invocation under a remote cwd → local-binary
allowlist (keychain `security`, `pbcopy`, `tmux`, the spawn proxy, claude
itself; extend via `RCC_LOCAL_BINS`) → sentinel in argv → target under a remote
prefix → working directory under a remote prefix → local. Routed children run
with the injection environment stripped and `argv[0]` preserved so
argv[0]-sensitive binaries (claude's embedded ripgrep) behave correctly.

## Repository layout

| Path | What |
|---|---|
| [`cmd/rca`](cmd/rca) | The single binary: `serve`, run mode, `_spawn-proxy`, `_fuse`, embedded artifacts. |
| [`internal/protocol`](internal/protocol) | Binary fs IO-RPC wire format (shared C↔Go↔Go). |
| [`internal/execproto`](internal/execproto) | Streaming subprocess protocol (proxy↔executor). |
| [`internal/routing`](internal/routing) | Path routing table (remote-allowlist / default-remote). |
| [`internal/transport`](internal/transport) | Local↔executor link: Unix socket + go-libp2p. |
| [`internal/paircode`](internal/paircode) | Self-contained pairing code (PeerID + multiaddrs). |
| [`internal/executor`](internal/executor) | fs + subprocess services and stream multiplexing. |
| [`internal/adapter`](internal/adapter) | IO-RPC server, routing relay, target launch/injection. |
| [`internal/linuxfuse`](internal/linuxfuse) | Linux lazy-slice FUSE filesystem behind `rca _fuse`. |
| [`native/macos`](native/macos) | DYLD interpose dylib (embedded into `rca` by `make`). |
| [`native/linux`](native/linux) | seccomp-user-notify supervisor (embedded into `rca` by `make`). |
| [`docs/design.md`](docs/design.md) | Full design + POC results. |

## Status & roadmap

**Verified in this repo (tests + local builds + CI):**

- Binary IO-RPC and subprocess protocols round-trip (`internal/protocol`,
  `internal/execproto`).
- Routing table for both policies (`internal/routing`); pairing code
  round-trips (`internal/paircode`).
- Adapter ↔ executor end-to-end over real Unix sockets: `stat`/`open`/`pread`
  slice/`write`/`readdir` route to the correct side with handle namespacing
  (`internal/adapter` integration test).
- Executor subprocess streaming: stdout/stderr split, exit-code fidelity,
  background tasks with process-group signal forwarding (`internal/executor`).
- **Real `claude` injection, end-to-end on macOS** (`scripts/e2e-local.sh`):
  Read and Bash redirected to the executor, routed naturally by cwd with no
  sentinel; claude's credential/self spawns stay local. Subagents inherit the
  injection (`scripts/e2e-subagent.sh`). Grep/Glob run as one remote ripgrep
  with `argv[0]` preserved.
- **Filesystem IO-RPC and subprocesses over go-libp2p** (`internal/transport`,
  `internal/adapter`): Noise-secured, PeerID-addressed; the exec bridge splices
  the spawn proxy's local unix connection to a libp2p stream. DCUtR
  hole-punching and circuit-relay fallback are enabled in the host config.
- **Linux lazy slicing over FUSE** (`internal/linuxfuse`, verified in a
  privileged container by `scripts/linux-fuse-test.sh`): a routed `openat` is
  redirected to a FUSE-backed file served by `rca _fuse`; a 25-byte read at
  5 MiB of a 10 MiB file moves only 4096 bytes over the fs-RPC.

**Next milestones:**

- **NAT-traversal field testing.** Hole-punching + relay options are enabled but
  only loopback-tested; real cross-NAT verification needs two hosts and a relay.
- **Natural routing of bare relative fs opens.** Files opened by absolute path
  (what claude's Read/Write tools do) route correctly; `open("rel")` /
  `openat(AT_FDCWD, "rel")` are left local because routing every relative open
  against the cwd destabilises claude's boot. Needs a safer cwd-scoped policy
  (design doc §4.3).

See [`docs/design.md`](docs/design.md) for the complete design and the POC
evidence behind every claim above.

## License

MIT — see [LICENSE](LICENSE).
