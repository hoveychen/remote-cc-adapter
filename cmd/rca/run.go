package main

// rca <command> [args...] — run mode, the local side. Spawns <command> with the
// native interceptor injected, serves the fs IO-RPC socket, and routes each
// intercepted filesystem/subprocess op to the local host or the paired remote
// executor per the routing table.
//
// rca-owned flags (see ownedFlags) are extracted from anywhere on the command
// line so `rca claude --code xxx` and `rca --code xxx claude -p "hi"` both
// work; everything after a literal `--` is passed to <command> verbatim.

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hoveychen/remote-adapter/internal/adapter"
	"github.com/hoveychen/remote-adapter/internal/paircode"
	"github.com/hoveychen/remote-adapter/internal/routing"
	"github.com/hoveychen/remote-adapter/internal/transport"
)

// runOpts is the parsed run-mode command line.
type runOpts struct {
	command string   // the target binary (looked up in PATH)
	args    []string // its arguments, verbatim

	code string // pairing code from `rca serve`
	peer string // raw libp2p peer multiaddr (alternative to code)
	sock string // co-located executor unix socket (alternative to code)

	workdir        string
	remotePrefixes []string
	localPrefixes  []string
	defaultRemote  bool
	profile        string // engine profile for --default-remote local pins ("" = auto-detect from command)

	resign      bool // macOS: run an ad-hoc re-signed copy (default true)
	printCmd    bool
	serveFSOnly bool

	adapterSock string
	sentinel    string
	dylib       string
	supervisor  string
	spawnProxy  string
}

// flag kinds for the run-mode extractor.
const (
	kindBool = iota
	kindString
	kindStringList
)

// ownedFlags are the run-mode flags rca claims for itself. Anything else on the
// command line belongs to <command>. Names chosen to not collide with claude's.
var ownedFlags = map[string]int{
	"code":           kindString,
	"peer":           kindString,
	"sock":           kindString,
	"workdir":        kindString,
	"remote-prefix":  kindStringList,
	"local-prefix":   kindStringList,
	"default-remote": kindBool,
	"profile":        kindString,
	"resign":         kindBool,
	"print-cmd":      kindBool,
	"serve-fs-only":  kindBool,
	"adapter-sock":   kindString,
	"spawn-sentinel": kindString,
	"dylib":          kindString,
	"supervisor":     kindString,
	"spawn-proxy":    kindString,
}

// parseRunArgs extracts rca-owned flags from args; the first non-flag token is
// the command, the rest are its arguments. A literal "--" ends extraction:
// everything after it goes to the command verbatim.
func parseRunArgs(args []string) (*runOpts, error) {
	o := &runOpts{
		resign:      runtime.GOOS == "darwin",
		adapterSock: defaultAdapterSock(),
		sentinel:    "RCC_REMOTE_MARK",
	}
	set := func(name, val string) error {
		switch name {
		case "code":
			o.code = val
		case "peer":
			o.peer = val
		case "sock":
			o.sock = val
		case "workdir":
			o.workdir = val
		case "profile":
			o.profile = val
		case "remote-prefix":
			o.remotePrefixes = append(o.remotePrefixes, val)
		case "local-prefix":
			o.localPrefixes = append(o.localPrefixes, val)
		case "adapter-sock":
			o.adapterSock = val
		case "spawn-sentinel":
			o.sentinel = val
		case "dylib":
			o.dylib = val
		case "supervisor":
			o.supervisor = val
		case "spawn-proxy":
			o.spawnProxy = val
		}
		return nil
	}
	setBool := func(name string, v bool) {
		switch name {
		case "default-remote":
			o.defaultRemote = v
		case "resign":
			o.resign = v
		case "print-cmd":
			o.printCmd = v
		case "serve-fs-only":
			o.serveFSOnly = v
		}
	}

	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			rest := args[i+1:]
			if o.command == "" && len(rest) > 0 {
				o.command, rest = rest[0], rest[1:]
			}
			o.args = append(o.args, rest...)
			break
		}
		if strings.HasPrefix(tok, "-") && tok != "-" {
			name := strings.TrimLeft(tok, "-")
			val := ""
			hasVal := false
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				name, val, hasVal = name[:eq], name[eq+1:], true
			}
			if kind, owned := ownedFlags[name]; owned {
				switch kind {
				case kindBool:
					b := true
					if hasVal {
						var err error
						if b, err = strconv.ParseBool(val); err != nil {
							return nil, fmt.Errorf("rca: bad value for --%s: %q", name, val)
						}
					}
					setBool(name, b)
				default:
					if !hasVal {
						i++
						if i >= len(args) {
							return nil, fmt.Errorf("rca: --%s needs a value", name)
						}
						val = args[i]
					}
					if err := set(name, val); err != nil {
						return nil, err
					}
				}
				continue
			}
			// Not ours. Before the command it's a typo; after, it's the
			// command's flag.
			if o.command == "" {
				return nil, fmt.Errorf("rca: unknown flag %q before the command (use `rca help`, or put it after the command name)", tok)
			}
			o.args = append(o.args, tok)
			continue
		}
		if o.command == "" {
			o.command = tok
		} else {
			o.args = append(o.args, tok)
		}
	}

	if o.command == "" && !o.serveFSOnly {
		return nil, fmt.Errorf("rca: no command given (usage: rca <command> [args...] --code <pairing-code>)")
	}
	if n := btoi(o.code != "") + btoi(o.peer != "") + btoi(o.sock != ""); n != 1 {
		return nil, fmt.Errorf("rca: exactly one of --code, --peer or --sock is required (got %d)", n)
	}
	return o, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func cmdRun(args []string) int {
	o, err := parseRunArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	logger := log.New(os.Stderr, "rca ", log.LstdFlags|log.Lmsgprefix)

	mode := routing.ModeRemoteAllowlist
	if o.defaultRemote {
		mode = routing.ModeLocalAllowlist
		// Under default-remote, keep the engine's own config home (and any global
		// config file) local, even if the operator forgot to pass --local-prefix
		// for them; otherwise the engine's credential/session reads route remote
		// and it cannot boot. The engine is picked from --profile, or auto-detected
		// from the target command's basename (claude/codex).
		profile := o.profile
		if profile == "" {
			profile = detectProfile(o.command)
		}
		if defs := profileLocalPrefixes(profile); len(defs) > 0 {
			logger.Printf("default-remote: profile %q pinning local prefixes %s", profile, strings.Join(defs, ", "))
			o.localPrefixes = append(defs, o.localPrefixes...)
		} else {
			logger.Printf("default-remote: no engine profile matched command %q — pass --profile or --local-prefix to keep engine state local", o.command)
		}
	} else if len(o.remotePrefixes) == 0 {
		// Natural default: the directory you run rca in is the project that
		// lives on the remote.
		wd := o.workdir
		if wd == "" {
			if wd, err = os.Getwd(); err != nil {
				logger.Printf("getwd: %v", err)
				return 1
			}
		}
		o.remotePrefixes = []string{wd}
		logger.Printf("routing remote prefix: %s (default: workdir)", wd)
	}
	// Resolve symlinks in prefixes (e.g. macOS /tmp -> /private/tmp) so they
	// match the canonical paths the target actually opens after cwd resolution.
	route := routing.New(mode, resolvePrefixes(o.remotePrefixes), resolvePrefixes(o.localPrefixes))

	// Resolve the target binary and (macOS) prepare a re-signed copy so the
	// interpose dylib can load. The original binary is never touched.
	var target string
	if !o.serveFSOnly {
		if target, err = exec.LookPath(o.command); err != nil {
			logger.Printf("command %q: %v", o.command, err)
			return 127
		}
		if runtime.GOOS == "darwin" && o.resign {
			// Apple platform binaries (e.g. /bin/sh) live in the system trust
			// cache; macOS SIGKILLs any copy of them — re-signed or not — so
			// they can never be intercepted. Fail with a real explanation
			// instead of a silent exit 137. (Verified 2026-07: `cp /bin/sh
			// /tmp/x && /tmp/x` dies with SIGKILL on Darwin 25.)
			if isMacOSPlatformBinary(target) {
				logger.Printf("%s is an Apple platform binary; macOS kills copies of these outside the system trust cache, so it cannot be intercepted. Target a user-installed binary instead.", target)
				return 1
			}
			dest := filepath.Join(filepath.Dir(o.adapterSock), "rcc-"+filepath.Base(target)+"-copy")
			realTarget := target
			if target, err = adapter.PrepareMacOSCopy(realTarget, dest); err != nil {
				logger.Printf("prepare re-signed copy: %v", err)
				return 1
			}
			logger.Printf("re-signed copy at %s", target)

			// Some engines (codex) spawn a sibling helper binary they resolve
			// relative to their own executable dir (e.g. codex-code-mode-host,
			// which runs the shell tool). Since the target now runs from an
			// isolated copy dir, that sibling is absent there — copy+re-sign each
			// declared helper next to the copy so the engine finds it and the
			// interceptor keeps it local (see spawn_is_local_bin, profile.go).
			profileName := o.profile
			if profileName == "" {
				profileName = detectProfile(o.command)
			}
			for _, h := range profileSpawnHelpers(profileName) {
				src := filepath.Join(filepath.Dir(realTarget), h)
				if _, statErr := os.Stat(src); statErr != nil {
					logger.Printf("engine helper %q not found next to %s (%v) — skipping", h, realTarget, statErr)
					continue
				}
				hdest := filepath.Join(filepath.Dir(dest), h)
				if _, err = adapter.PrepareMacOSCopy(src, hdest); err != nil {
					logger.Printf("prepare re-signed helper %q: %v", h, err)
					return 1
				}
				logger.Printf("re-signed engine helper at %s", hdest)
			}
		}
	}

	// Default the spawn proxy to this very binary: the interceptor execs
	// `rca _spawn-proxy <exec-path> <argv...>` for routed subprocesses.
	if o.spawnProxy == "" {
		if o.spawnProxy, err = os.Executable(); err != nil {
			logger.Printf("locate self for spawn proxy: %v", err)
			return 1
		}
	}

	// Default the native interceptor to the embedded artifact (put there by
	// `make native`), extracted to the user cache dir.
	if !o.serveFSOnly {
		name := nativeArtifactName(runtime.GOOS)
		switch {
		case runtime.GOOS == "darwin" && o.dylib == "":
			if o.dylib, err = extractEmbeddedNative(name); err != nil {
				logger.Print(err)
				return 1
			}
			if o.dylib == "" {
				logger.Printf("no embedded interceptor in this build — build rca with `make`, or pass --dylib")
				return 1
			}
		case runtime.GOOS == "linux" && o.supervisor == "":
			if o.supervisor, err = extractEmbeddedNative(name); err != nil {
				logger.Print(err)
				return 1
			}
			if o.supervisor == "" {
				logger.Printf("no embedded interceptor in this build — build rca with `make`, or pass --supervisor")
				return 1
			}
		}
	}

	// The spawn proxy always connects to the adapter's local exec-bridge socket,
	// which forwards to the executor over the shared transport (unix or libp2p).
	// This is what lets subprocesses route cross-machine without the proxy
	// speaking libp2p itself.
	bridgeSock := o.adapterSock + ".exec"

	// Build the executor-facing transport: pairing code / raw peer multiaddr
	// (libp2p) or a co-located unix socket.
	var dialer transport.Dialer
	switch {
	case o.code != "" || o.peer != "":
		h, err := transport.NewLibp2pHost(transport.HostConfig{
			ListenAddrs:        []string{"/ip4/0.0.0.0/tcp/0"},
			EnableHolePunching: true,
		})
		if err != nil {
			logger.Printf("libp2p host: %v", err)
			return 1
		}
		if o.code != "" {
			info, err := paircode.Decode(o.code)
			if err != nil {
				logger.Print(err)
				return 2
			}
			dialer = transport.NewLibp2pDialer(h, info)
			logger.Printf("executor transport: pairing code -> peer %s (%d addrs)", info.ID, len(info.Addrs))
		} else {
			d, err := transport.DialLibp2pPeer(h, o.peer)
			if err != nil {
				logger.Printf("peer: %v", err)
				return 2
			}
			dialer = d
			logger.Printf("executor transport: libp2p peer %s", o.peer)
		}
	default:
		dialer = transport.NewUnixDialer(o.sock)
	}

	// Start the interceptor-facing IO-RPC server.
	ln, err := transport.ListenUnix(o.adapterSock)
	if err != nil {
		logger.Printf("listen %s: %v", o.adapterSock, err)
		return 1
	}
	defer ln.Close()
	ad := adapter.New(ln, dialer, route, logger)

	// Start the exec bridge: proxy connections on bridgeSock forward to the
	// executor over the same transport.
	bridgeLn, err := transport.ListenUnix(bridgeSock)
	if err != nil {
		logger.Printf("listen exec bridge %s: %v", bridgeSock, err)
		return 1
	}
	defer bridgeLn.Close()
	bridge := adapter.NewExecBridge(bridgeLn, dialer, logger)
	go func() {
		if err := bridge.Serve(); err != nil {
			logger.Printf("exec bridge stopped: %v", err)
		}
	}()

	go func() {
		if err := ad.Serve(); err != nil {
			logger.Printf("io-rpc server stopped: %v", err)
		}
	}()

	// serve-fs-only: run the brain-side services and block (target/interceptor
	// launched separately, e.g. the Linux FUSE client or an external harness).
	if o.serveFSOnly {
		logger.Printf("serving fs-RPC on %s and exec bridge on %s (no command spawn)", o.adapterSock, bridgeSock)
		sigc := make(chan os.Signal, 2)
		signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
		<-sigc
		return 0
	}

	// Cross-OS deployment: the agent launches on THIS host and its environment
	// block reports this OS, but its routed subprocesses execute on the executor.
	// When the two differ (e.g. macOS agent, Linux executor), prepend an
	// engine-specific system-prompt hint so the agent writes commands for the
	// executor's platform instead of the local one — otherwise it emits
	// wrong-dialect commands (BSD sed/ls on GNU/Linux) that fail repeatedly. The
	// probe is best-effort: on any failure (short timeout, or an old executor
	// that predates OpServerInfo) we skip the hint rather than block the launch.
	{
		// A relayed executor can need more than one attempt: the first dial may
		// spend its whole deadline walking the pairing code's unreachable
		// private addrs into backoff before the circuit address wins (observed
		// 2026-07-19, mac → own-api-ko via relay: a 10s one-shot probe timed
		// out while the session's first real fs op connected moments later).
		// Retry once with the backoff already primed — cross-OS is exactly the
		// remote case relays serve, so this is where the hint matters most. An
		// old executor that predates OpServerInfo fails fast, so the retry
		// costs nothing there.
		var (
			execOS, execArch string
			qerr             error
		)
		for attempt := 1; attempt <= 2; attempt++ {
			qctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			execOS, execArch, qerr = adapter.QueryServerInfo(qctx, dialer)
			cancel()
			if qerr == nil {
				break
			}
			logger.Printf("executor OS probe attempt %d failed: %v", attempt, qerr)
		}
		switch {
		case qerr != nil:
			logger.Printf("executor OS probe gave up — skipping cross-OS hint")
		case execOS == runtime.GOOS:
			// Same-OS deployment (incl. co-located): nothing to reconcile.
		default:
			profileName := o.profile
			if profileName == "" {
				profileName = detectProfile(o.command)
			}
			// Two independent cross-OS adjustments, both prepended to the engine's
			// args: (1) a system-prompt hint so the agent writes commands for the
			// executor's OS; (2) engine args needed to FUNCTION cross-OS, e.g.
			// disabling codex's macOS sandbox whose /usr/bin/sandbox-exec wrapper
			// 127s on a Linux executor.
			hint := osHintArgs(profileName, runtime.GOOS, execOS, execArch)
			extra := profileCrossOSExtraArgs(profileName)
			inject := append(append([]string(nil), hint...), extra...)
			if len(inject) > 0 {
				o.args = append(inject, o.args...)
				logger.Printf("cross-OS: agent on %s, executor on %s/%s — for %q injected OS hint=%t extra=%v", runtime.GOOS, execOS, execArch, profileName, len(hint) > 0, extra)
			} else {
				logger.Printf("cross-OS: agent on %s, executor on %s/%s — profile %q has no cross-OS handling; the agent may emit %s-wrong commands or its local sandbox may fail on the executor. Add a note to the engine's instructions (e.g. AGENTS.md) telling it commands run on %s.", runtime.GOOS, execOS, execArch, profileName, runtime.GOOS, execOS)
			}
		}
	}

	// Build and spawn the intercepted target process.
	cfg := &adapter.LaunchConfig{
		TargetPath:     target,
		Args:           o.args,
		WorkDir:        o.workdir,
		AdapterSock:    o.adapterSock,
		ExecutorSock:   bridgeSock,
		SpawnProxyPath: o.spawnProxy,
		RemotePrefixes: route.RemotePrefixes(),
		SpawnSentinel:  o.sentinel,
		DylibPath:      o.dylib,
		SupervisorPath: o.supervisor,
	}
	cmd, err := cfg.BuildCommand()
	if err != nil {
		logger.Print(err)
		return 1
	}

	// On Linux the target runs inside a private mount namespace where each remote
	// prefix is mounted at its own absolute path, so every syscall — not just the
	// intercepted ones — sees the remote directory. macOS uses DYLD interposition
	// and needs none of this.
	workdir := o.workdir
	if workdir == "" {
		if workdir, err = os.Getwd(); err != nil {
			logger.Printf("getwd: %v", err)
			return 1
		}
	}
	if cmd, err = wrapMountNamespace(cmd, o.adapterSock, workdir, route.RemotePrefixes(), logger); err != nil {
		logger.Printf("mount namespace: %v", err)
		return 1
	}

	if o.printCmd {
		fmt.Println(strings.Join(cmd.Args, " "))
		for _, kv := range injectedEnv(cfg) {
			fmt.Println("env:", kv)
		}
		return 0
	}

	// Forward termination signals to the target.
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	if err := cmd.Start(); err != nil {
		logger.Printf("start %s: %v", o.command, err)
		return 1
	}
	go func() {
		for s := range sigc {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(s)
			}
		}
	}()
	return exitCode(cmd.Wait())
}

// isMacOSPlatformBinary reports whether path is an Apple platform binary
// (codesign prints "Platform identifier=" for those). Platform binaries are
// validated against the static trust cache, so copies of them are SIGKILLed
// and can never carry an interpose dylib.
func isMacOSPlatformBinary(path string) bool {
	out, err := exec.Command("codesign", "-d", "--verbose=2", path).CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "Platform identifier=")
}

func defaultAdapterSock() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("rcc-adapter-%d.sock", os.Getpid()))
}

// resolvePrefixes resolves symlinks in each prefix (best effort). A prefix that
// cannot be resolved (e.g. does not exist yet) is passed through unchanged.
func resolvePrefixes(in []string) []string {
	out := make([]string, len(in))
	for i, p := range in {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			out[i] = r
		} else {
			out[i] = p
		}
	}
	return out
}

// injectedEnv mirrors the env LaunchConfig.BuildCommand sets, for --print-cmd
// display. Keep it in sync with launch.go so the printed command is accurate.
func injectedEnv(cfg *adapter.LaunchConfig) []string {
	env := []string{
		adapter.EnvAdapterSock + "=" + cfg.AdapterSock,
		adapter.EnvExecutorSock + "=" + cfg.ExecutorSock,
		adapter.EnvSpawnProxy + "=" + cfg.SpawnProxyPath,
		adapter.EnvRemotePrefix + "=" + strings.Join(cfg.RemotePrefixes, ":"),
		adapter.EnvSpawnSentinel + "=" + cfg.SpawnSentinel,
		adapter.EnvTargetPath + "=" + cfg.TargetPath,
	}
	if runtime.GOOS == "darwin" {
		env = append(env, adapter.EnvDylib+"="+cfg.DylibPath)
	}
	return env
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	return 1
}
