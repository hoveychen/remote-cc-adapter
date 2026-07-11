// Command rca is the single-binary CLI for remote-cc-adapter. One executable
// carries every role; the first argument picks it:
//
//	rca serve                — remote side: run the executor and print a pairing code
//	rca <command> [args...]  — local side: run <command> locally with its filesystem
//	                           and subprocess ops routed to the paired remote (--code)
//	rca _spawn-proxy ...     — internal: stand-in for a routed subprocess
//	                           (exec'd by the native interceptor, never by hand)
//	rca _fuse ...            — internal: Linux lazy-slice FUSE daemon
//	rca _nsrun ...           — internal: Linux private-mount-namespace launcher
//
// Example:
//
//	remote$ rca serve
//	  pairing code: rca1.CAESIL...
//	local$  rca claude --code rca1.CAESIL...
//
// Anything that is not a known subcommand is treated as the command to run
// (run mode), so `rca claude -p "hi" --code ...` works without a `run` keyword.
package main

import (
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

func main() { os.Exit(dispatch(os.Args[1:])) }

func dispatch(args []string) int {
	if len(args) == 0 {
		usage(os.Stderr)
		return 2
	}
	switch args[0] {
	case "serve":
		return cmdServe(args[1:])
	case "relay":
		return cmdRelay(args[1:])
	case "_spawn-proxy":
		return cmdSpawnProxy(args[1:])
	case "_fuse":
		return cmdFuse(args[1:])
	case "_nsrun":
		return cmdNsRun(args[1:])
	case "help", "-h", "--help":
		usage(os.Stdout)
		return 0
	case "version", "--version":
		fmt.Println(versionString())
		return 0
	}
	return cmdRun(args)
}

func usage(w io.Writer) {
	fmt.Fprint(w, `rca — run a local CLI against a remote filesystem/sandbox

Usage:
  rca serve [flags]                 remote side: executor + pairing code
  rca relay [flags]                 run a circuit-relay v2 relay for NAT traversal
  rca <command> [args...] [flags]   local side: run <command> routed to the remote
  rca version                       print version

Run-mode flags (extracted from anywhere on the command line; everything
after a literal -- is passed to <command> verbatim):
  --code <pairing-code>   connect to a "rca serve" remote (from its output)
  --peer <multiaddr>      connect by raw libp2p multiaddr instead of a code
  --sock <path>           connect to a co-located executor unix socket
  --workdir <dir>         working directory for <command> (default: cwd)
  --remote-prefix <path>  path prefix routed remote (repeatable; default: workdir)
  --local-prefix <path>   path prefix kept local under --default-remote (repeatable)
  --default-remote        route everything remote except --local-prefix paths
  --profile <engine>      engine whose config home is pinned local under
                          --default-remote (claude|codex; default: auto-detect
                          from <command>)
  --resign / --resign=false
                          macOS: run an ad-hoc re-signed copy of <command> so the
                          interceptor dylib can load (default: true)
  --print-cmd             print the assembled launch command and exit
  --serve-fs-only         serve fs-RPC + exec bridge only; do not spawn <command>

Advanced run-mode flags: --adapter-sock, --spawn-sentinel, --dylib,
--supervisor, --spawn-proxy.

Serve flags: --listen, --sock, --hole-punch, --relays, --announce
(env RCA_SERVE_ANNOUNCE; advertise a public addr for direct dial behind
1:1 NAT with an opened port).
Relay flags: --listen (default /ip4/0.0.0.0/tcp/8080/ws), --announce
(env RCA_RELAY_ANNOUNCE); identity via env RCA_RELAY_KEY.
`)
}

// version is stamped by release builds via -ldflags "-X main.version=v1.2.3"
// (see scripts/build-release.sh); source builds fall back to module build info.
var version string

func versionString() string {
	if version != "" {
		return "rca " + version
	}
	v := "rca (devel)"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		v = "rca " + bi.Main.Version
	}
	return v
}
