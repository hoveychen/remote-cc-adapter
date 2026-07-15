# Embedded native artifacts

`make native` copies the platform interceptor here before `make go` builds
`rca`, so the single binary carries it:

- macOS: `rcc_interpose.dylib` (DYLD interpose dylib, from `native/macos`)
- Linux: `rcc_seccomp` (seccomp-user-notify supervisor, from `native/linux`)

`make rg` additionally stages a static ripgrep for the target GOOS/GOARCH here:

- `rg` (from the ripgrep GitHub release, checksum-verified in the Makefile)

The executor extracts and runs it when a routed `rg` spawn cannot be resolved
locally — cross-OS, claude re-execs its own host-OS binary with argv[0]=rg,
which cannot run on a different-OS executor (see `internal/executor/exec.go`).

At runtime `rca` extracts each artifact to the user cache dir. A plain
`go build ./cmd/rca` without `make native`/`make rg` still compiles; run mode
then needs `--dylib` / `--supervisor`, and cross-OS `rg` spawns fall back to the
executor's own ripgrep if present.

Everything in this directory except this file is gitignored.
