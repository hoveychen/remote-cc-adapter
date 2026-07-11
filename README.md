# remote-cc-adapter (`rca`)

Run a CLI — typically [Claude Code](https://claude.com/claude-code) — locally,
but make its **tool calls execute on another machine**: file reads/writes and
subprocesses land in a remote sandbox, while the process itself (and for
claude, the reasoning loop, tool schemas, transcript) stays byte-for-byte
native on your machine. The model cannot perceive the split: there are no
`mcp__` tool prefixes and no custom schemas, because the tool *implementations*
are untouched. Only the syscalls beneath them (`open`/`read`/`posix_spawn`/…)
are redirected.

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
and every `Bash` command it runs happens on the remote.

## Install

Grab the single binary from
[Releases](https://github.com/hoveychen/remote-cc-adapter/releases) — one
archive per platform, interceptor included:

```sh
# macOS (Apple silicon)
curl -fsSL https://github.com/hoveychen/remote-cc-adapter/releases/latest/download/rca_darwin_arm64.tar.gz | tar xz
# macOS (Intel)
curl -fsSL https://github.com/hoveychen/remote-cc-adapter/releases/latest/download/rca_darwin_amd64.tar.gz | tar xz
# Linux (x86_64)
curl -fsSL https://github.com/hoveychen/remote-cc-adapter/releases/latest/download/rca_linux_amd64.tar.gz | tar xz
# Linux (arm64)
curl -fsSL https://github.com/hoveychen/remote-cc-adapter/releases/latest/download/rca_linux_arm64.tar.gz | tar xz

sudo install -m 755 rca /usr/local/bin/rca   # or anywhere on your PATH
rca version
```

`checksums.txt` on each release carries the sha256 of every archive.

Or build from source (Go 1.25+, a C compiler):

```sh
make          # native interceptor for this platform, then rca (embeds it) into ./bin
```

## Usage

### 1. Pair the two machines

On the **remote** machine (where the project files live and commands should
execute):

```sh
rca serve
```

It prints a pairing code — a self-contained `rca1.…` string packing its libp2p
identity and addresses. There is no rendezvous server; copy the code once.

### 2. Run your CLI locally against the remote

On the **local** machine:

```sh
cd ~/work/project          # the directory that lives on the remote
rca claude --code rca1.…                  # interactive TUI
rca claude --code rca1.… -p "fix the failing test"   # non-interactive
```

Any user-installed CLI works, not just claude — `rca <command> [args…]` runs
`<command>` locally with its filesystem and subprocesses transparently routed.
(On macOS, Apple system binaries like `/bin/sh` cannot be injected — the OS
kills copies of trust-cached binaries — so target user-installed tools.)

Useful flags (they may appear anywhere on the line; everything after a literal
`--` goes to the command verbatim):

| Flag | Meaning |
|---|---|
| `--code <rca1.…>` | connect to a `rca serve` remote (from its output) |
| `--remote-prefix <path>` | path prefix routed remote; **default: the working directory** |
| `--workdir <dir>` | working directory for the command (default: cwd) |
| `--default-remote` | route *everything* remote except `--local-prefix` paths |
| `--resign=false` | macOS: skip running an ad-hoc re-signed copy of the target |
| `--sock <path>` | co-located mode: dial an executor unix socket instead of libp2p |
| `--peer <multiaddr>` | dial a raw libp2p multiaddr instead of a pairing code |
| `--print-cmd` | print the assembled launch command + env, spawn nothing |

Defaults are chosen so the common case needs zero flags: the project you `cd`
into routes remote; claude's own config and credentials (`~/.claude`,
`~/.claude.json`) always stay local, even under `--default-remote`.

### Co-located testing (one host)

```sh
rca serve --sock /tmp/rcc-exec.sock &
rca --sock /tmp/rcc-exec.sock claude -p "list the files here"
```

### NAT traversal with a relay

The pairing code packs the remote's dialable addresses, so when the remote is
directly reachable (public IP, LAN, or an SSH tunnel you set up yourself) the
local side connects straight to it. When the remote sits behind NAT and has no
reachable address, run a **circuit-relay** on a host both sides *can* reach and
point the remote at it:

```sh
# On a host with a public IP — a RAW TCP/UDP port (see the note below on why it
# must NOT sit behind an HTTP reverse proxy):
relay$ RCA_RELAY_KEY=… rca relay \
         --listen   /ip4/0.0.0.0/tcp/4001,/ip4/0.0.0.0/udp/4001/quic-v1 \
         --announce /ip4/<public-ip>/tcp/4001,/ip4/<public-ip>/udp/4001/quic-v1
  relay peer id: 12D3Koo…   # pin RCA_RELAY_KEY so the PeerID survives restarts

# On the NAT'd remote — reserve a slot on the relay; the pairing code then
# carries a relayed /p2p-circuit address in addition to the direct ones:
remote$ rca serve --relays /ip4/<public-ip>/tcp/4001/p2p/12D3Koo…

# On the local machine — nothing changes; --code just works:
local$  rca claude --code rca1.…
```

The local side dials the remote through the relay, then libp2p's DCUtR
hole-punching **upgrades to a direct connection**: over the relayed connection
each side learns its own public address from the relay's `identify` observation,
then both simultaneously dial to punch through their NATs. Once the direct
connection is up the relay carries nothing (verified end-to-end: two hosts on
different networks punched a direct TCP connection in ~1 s). If the NATs can't
be punched — e.g. a symmetric NAT that remaps the port per destination — it
keeps relaying.

**The relay must expose a raw TCP/UDP port, not sit behind an HTTP reverse
proxy.** DCUtR depends on the relay observing each endpoint's *real* public
address and reporting it back via `identify`. An HTTPS proxy (Traefik, nginx, a
PaaS that terminates TLS and forwards to `:8080`) makes the relay observe only
the proxy's *internal* address; that address is private, gets filtered out, and
the hole punch never initiates — so traffic relays forever. A WebSocket relay
behind such a proxy still works as a *relay* (traffic flows), it just can never
upgrade to a direct connection. For the direct upgrade give the relay its own
public port, as above.

The repo-root `Dockerfile` builds a minimal relay image (`ENTRYPOINT ["rca",
"relay"]`). Run it with host networking so both the TCP and QUIC ports are
published, and pin a stable key:

```sh
docker run -d --restart unless-stopped --name rca-relay --network host \
  -e RCA_RELAY_KEY=<stable-key> <relay-image> \
  --listen   /ip4/0.0.0.0/tcp/4001,/ip4/0.0.0.0/udp/4001/quic-v1 \
  --announce /ip4/<public-ip>/tcp/4001,/ip4/<public-ip>/udp/4001/quic-v1
```

For an HTTP-only PaaS that can only ever *relay* (no direct upgrade), keep the
WebSocket form instead: `rca relay` on `:8080` with
`RCA_RELAY_ANNOUNCE=/dns4/<host>/tcp/443/tls/ws` behind the proxy.

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
   injected — a DYLD interpose dylib on macOS, a seccomp-user-notify supervisor
   on Linux. Both are embedded in the `rca` binary and extracted to the user
   cache dir at runtime.
2. The **interceptor** catches the process's `open`/`read`/`stat`/`readdir`
   syscalls. Routed paths are forwarded to the adapter; everything else falls
   through to the real local syscall, so the CLI boots, reads its credentials,
   and writes `~/.claude` locally.
3. The adapter's **routing table** either serves the op on the local filesystem
   or relays it to the remote **executor** (`rca serve`). Large files move as
   on-demand slices, never materialised whole (on Linux via a lazy FUSE mount,
   `rca _fuse`).
4. **Subprocesses** (`Bash`, ripgrep for `Grep`/`Glob`, `git`, …) spawned under
   a remote-routed directory are rewritten to `rca _spawn-proxy`, which streams
   them from the executor — stdout/stderr, signals, and exit codes are relayed,
   and `argv[0]` is preserved so argv[0]-sensitive binaries (claude's embedded
   ripgrep) behave correctly. Credential/self spawns (keychain `security`,
   claude itself, `pbcopy`, `tmux`) always stay local.
5. The **transport** is go-libp2p: Noise-encrypted, addressed by PeerID
   (== public key), with DCUtR hole-punching for NAT traversal. The pairing
   code packs the PeerID and dialable addresses — trust is pinned to the key,
   not the network path.

Why syscall-level interception rather than MCP replacement tools or a
`can_use_tool` rewrite? Because those leak into what the model sees (tool
names, schemas) or only cover `Bash`. Redirecting the syscalls beneath the
native tools covers *all* tools with zero distribution shift. The full
rationale, the three rejected designs, and the POC evidence are in
[`docs/design.md`](docs/design.md).

## Development

```sh
make            # native interceptor + rca into ./bin
make test       # go test ./...
scripts/e2e-paircode.sh   # full pipeline e2e, no claude needed (also runs in CI)
scripts/e2e-local.sh      # real-claude e2e (macOS, logged-in claude required)
scripts/build-release.sh  # release archives for this host OS into ./dist
```

Repository layout, verified-status details, and the roadmap live in
[`docs/design.md`](docs/design.md); the per-component map is in the directory
READMEs ([`native/`](native/README.md), [`cmd/rca/embedded/`](cmd/rca/embedded/README.md)).

Linux run mode routes both file access and subprocess execution, by two separate
mechanisms.

Files: `rca _nsrun` puts the target in a private mount namespace and mounts each
remote-routed directory there, as a FUSE filesystem, at the same absolute path it
has on the remote. The kernel then gives every syscall — `openat`, `stat`,
`statx`, `getdents64`, `getcwd` — one consistent view of that directory, and
reads are fetched as on-demand slices rather than whole files.

Subprocesses: the supervisor (`native/linux/rcc_seccomp.c`) traps
`execve`/`execveat` as `SECCOMP_RET_TRACE` and is also the ptracer, so each
`execve` stops at syscall entry with `PTRACE_EVENT_SECCOMP` and — for paths that
route remote — its argv is rewritten to `rca _spawn-proxy …` so the subprocess
streams to the remote executor. LD_PRELOAD cannot intercept Bun's raw
`clone+execve`, which is why ptrace is required.

macOS routes both via DYLD interposition, of the file syscalls and of
`posix_spawn`.

Known limits: NAT traversal is enabled but not yet field-tested across real
networks; relative-path opens (`open("rel")`) stay local by design (claude's
tools use absolute paths). Linux subprocess routing rewrites plain `execve`
only; `execveat` runs locally (Bun uses `execve`). Real-claude end-to-end
verification of Linux subprocess routing is pending a logged-in claude on the
Linux test host — the mechanism itself is validated in isolation and across
hosts (file routing + the launcher→supervisor→proxy→bridge→executor chain).

## License

MIT — see [LICENSE](LICENSE).
