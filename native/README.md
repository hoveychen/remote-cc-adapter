# Native interceptors

The native interceptor is the brain-side interception載体 injected into the
`claude` process. Its job is identical on both platforms — turn the intercepted
`open`/`read`/`stat`/`readdir`/subprocess calls for *routed* paths into fs
IO-RPC to the adapter — but the injection mechanism differs, so there are two
implementations.

| Platform | Mechanism | Source | Why not the other |
|---|---|---|---|
| macOS | `DYLD_INSERT_LIBRARIES` interpose dylib | [`macos/rcc_interpose.c`](macos/rcc_interpose.c) | Bun on macOS goes through libSystem, so interpose cleanly catches its I/O (design doc §4.1.3). |
| Linux | `seccomp-user-notify` supervisor | [`linux/rcc_seccomp.c`](linux/rcc_seccomp.c) | Bun on Linux issues **raw** syscalls that bypass glibc, so `LD_PRELOAD` misses its file I/O; seccomp traps at the syscall boundary and catches everything (design doc §4.1.3). |

Both were ported from the validated POC (`/tmp/rcc-poc`, design doc §4–§5). The
material change from the POC is that per-op data no longer comes from a local
store on disk — it is fetched from the adapter via the binary IO-RPC protocol
([`internal/protocol`](../internal/protocol)), so the adapter's routing table
decides local-vs-remote.

## Building

```sh
make -C native/macos    # -> native/macos/rcc_interpose.dylib
make -C native/linux     # -> native/linux/rcc_seccomp
```

or `make native` from the repo root (builds the host platform's interceptor).

## Injection contract

The adapter (see [`internal/adapter/launch.go`](../internal/adapter/launch.go))
sets these environment variables on the `claude` process, and both interceptors
read them:

| Variable | Meaning |
|---|---|
| `RCC_ADAPTER_SOCK` | Unix socket the interceptor dials for fs IO-RPC. |
| `RCC_REMOTE_PREFIXES` | `:`-joined path prefixes to forward (remote-allowlist). |
| `RCC_SPAWN_PROXY` | Path to the `rca` binary; routed spawns become `rca _spawn-proxy <exec-path> <argv...>` (macOS subprocess forwarding). |
| `RCC_SPAWN_SENTINEL` | Env marker that forces a subprocess remote (macOS). |

Handle lifecycle: the interceptor keeps a file's `open`/`read`/`close` sequence
on a single connection to the adapter, so the adapter's per-connection handle
table (and the executor's per-stream one) stay valid across the sequence.

## Platform differences worth knowing

- **macOS `$NOCANCEL` variants are load-bearing.** libSystem routes ~73% of file
  opens through `open$NOCANCEL`/`openat$NOCANCEL`, and Bun uses
  `pread$NOCANCEL`/`close$NOCANCEL`. Missing any of them silently corrupts reads
  or drops writes (design doc §4.1 point 2, §4.1.2). All variants are hooked.
- **macOS requires a re-signed claude.** `DYLD_INSERT_LIBRARIES` is ignored for a
  hardened-runtime binary, so the adapter runs an ad-hoc re-signed *copy*
  (`--resign`); the original install is never touched (design doc §4.2).
- **Linux does file I/O with a mount, not with seccomp.** The supervisor traps
  only `execve`/`execveat`. Routed directories are FUSE mounts that `rca _nsrun`
  places at their own absolute paths inside the target's private mount namespace,
  so the kernel answers `openat`, `stat`, `statx`, `getdents64` and `getcwd` from
  one filesystem and reads arrive as on-demand slices (design doc §4.1.3 step 2,
  §4.3). Verified by `scripts/linux-fuse-test.sh`.

  It used to trap `openat` as a seccomp USER_NOTIF and inject a FUSE-backed fd.
  That routed `openat` and nothing else, so a target saw its own working directory
  through two filesystems at once — `fstatat(dirfd, name)` answered from the
  remote while `stat(fullpath)` returned ENOENT from the local disk — and Bun
  wedged at startup cross-checking the two. Trapping the whole stat family instead
  would have cost ~5.6k extra user-space round trips per claude run, with no
  kernel caching.
- **Linux needs FUSE, and CAP_SYS_ADMIN or a user namespace.** Mounting needs
  `/dev/fuse` and `CAP_SYS_ADMIN`; unprivileged callers get the capability from a
  user namespace `rca _nsrun` creates for them (identity uid mapping, ambient
  `CAP_SYS_ADMIN`, dropped again before the target is spawned). The tests use
  `--privileged --device /dev/fuse` (design doc §4.1.3).
