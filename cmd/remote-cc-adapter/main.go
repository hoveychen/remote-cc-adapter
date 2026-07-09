// Command remote-cc-adapter is the brain-side host. It stands in front of the
// claude CLI: it starts the fs IO-RPC server, spawns claude with the native
// interceptor injected, and routes each intercepted filesystem/subprocess op to
// the local host or the remote executor per the routing table.
//
// Example (macOS, executor co-located over a Unix socket):
//
//	rcc-executor -sock /tmp/rcc-exec.sock &
//	remote-cc-adapter \
//	  --claude "$(command -v claude)" \
//	  --dylib ./native/macos/rcc_interpose.dylib \
//	  --spawn-proxy ./bin/rcc-spawn-proxy \
//	  --executor-sock /tmp/rcc-exec.sock \
//	  --remote-prefix "$PWD" \
//	  -- -p "list files here"
//
// Everything after `--` is forwarded to claude verbatim.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/hoveychen/remote-cc-adapter/internal/adapter"
	"github.com/hoveychen/remote-cc-adapter/internal/routing"
	"github.com/hoveychen/remote-cc-adapter/internal/transport"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	var (
		claudePath   = flag.String("claude", "", "path to the claude binary (required)")
		dylibPath    = flag.String("dylib", "", "macOS interpose dylib path (required on macOS)")
		supervisor   = flag.String("supervisor", "", "Linux seccomp supervisor path (required on Linux)")
		spawnProxy   = flag.String("spawn-proxy", "", "path to rcc-spawn-proxy binary (required)")
		executorSock = flag.String("executor-sock", "", "executor unix socket path (co-located; required unless --peer)")
		peerAddr     = flag.String("peer", "", "executor libp2p peer multiaddr, e.g. /ip4/HOST/tcp/PORT/p2p/PEERID (cross-machine transport)")
		adapterSock  = flag.String("adapter-sock", defaultAdapterSock(), "fs IO-RPC socket the interceptor dials")
		sentinel     = flag.String("spawn-sentinel", "RCC_REMOTE_MARK", "env marker that forces a subprocess remote")
		localMode    = flag.Bool("default-remote", false, "route remote by default (local allowlist); otherwise local by default (remote allowlist)")
		resign       = flag.Bool("resign", false, "on macOS, copy+ad-hoc-resign claude so the dylib can load, and run the copy")
		printCmd     = flag.Bool("print-cmd", false, "print the claude launch command and exit (no spawn)")
		workDir      = flag.String("workdir", "", "claude working directory (point under a remote prefix for natural cwd routing)")
		fuseMnt      = flag.String("fuse-mount", "", "Linux: rcc-fuse mount point the seccomp supervisor redirects routed opens to (required on Linux)")
		serveFSOnly  = flag.Bool("serve-fs-only", false, "start only the brain-side fs-RPC server + exec bridge (do not spawn claude); useful when the interceptor/FUSE client is launched separately")
	)
	var remotePrefixes, localPrefixes stringList
	flag.Var(&remotePrefixes, "remote-prefix", "path prefix routed to the remote executor (repeatable)")
	flag.Var(&localPrefixes, "local-prefix", "path prefix kept local under -default-remote (repeatable)")
	flag.Parse()

	if !*serveFSOnly && (*claudePath == "" || *spawnProxy == "") {
		log.Fatal("remote-cc-adapter: --claude and --spawn-proxy are required")
	}
	if (*executorSock == "") == (*peerAddr == "") {
		log.Fatal("remote-cc-adapter: exactly one of --executor-sock or --peer is required")
	}

	logger := log.New(os.Stderr, "adapter ", log.LstdFlags|log.Lmsgprefix)

	mode := routing.ModeRemoteAllowlist
	if *localMode {
		mode = routing.ModeLocalAllowlist
		// Under default-remote, always keep claude's own config home and global
		// config file local, even if the operator forgot to pass --local-prefix
		// for them; otherwise claude's credential/session reads route remote.
		if defs := defaultLocalPrefixes(); len(defs) > 0 {
			logger.Printf("default-remote: pinning local prefixes %s", strings.Join(defs, ", "))
			localPrefixes = append(defs, localPrefixes...)
		}
	}
	// Resolve symlinks in prefixes (e.g. macOS /tmp -> /private/tmp) so they
	// match the canonical paths claude actually opens after cwd resolution.
	route := routing.New(mode, resolvePrefixes(remotePrefixes), resolvePrefixes(localPrefixes))

	claudeToRun := *claudePath
	if runtime.GOOS == "darwin" && *resign {
		dest := filepath.Join(filepath.Dir(*adapterSock), "rcc-claude-copy")
		var err error
		claudeToRun, err = adapter.PrepareMacOSCopy(*claudePath, dest)
		if err != nil {
			log.Fatalf("remote-cc-adapter: prepare claude copy: %v", err)
		}
		logger.Printf("re-signed claude copy at %s", claudeToRun)
	}

	// The spawn proxy always connects to the adapter's local exec-bridge socket,
	// which forwards to the executor over the shared transport (unix or libp2p).
	// This is what lets subprocesses route cross-machine without the proxy
	// speaking libp2p itself.
	bridgeSock := *adapterSock + ".exec"

	// Build the executor-facing transport: unix socket (co-located) or libp2p.
	var dialer transport.Dialer
	if *peerAddr != "" {
		h, err := transport.NewLibp2pHost(transport.HostConfig{
			ListenAddrs:        []string{"/ip4/0.0.0.0/tcp/0"},
			EnableHolePunching: true,
		})
		if err != nil {
			log.Fatalf("remote-cc-adapter: libp2p host: %v", err)
		}
		d, err := transport.DialLibp2pPeer(h, *peerAddr)
		if err != nil {
			log.Fatalf("remote-cc-adapter: peer: %v", err)
		}
		dialer = d
		logger.Printf("executor transport: libp2p peer %s", *peerAddr)
	} else {
		dialer = transport.NewUnixDialer(*executorSock)
	}

	// Start the interceptor-facing IO-RPC server.
	ln, err := transport.ListenUnix(*adapterSock)
	if err != nil {
		log.Fatalf("remote-cc-adapter: listen %s: %v", *adapterSock, err)
	}
	defer ln.Close()
	ad := adapter.New(ln, dialer, route, logger)

	// Start the exec bridge: proxy connections on bridgeSock forward to the
	// executor over the same transport.
	bridgeLn, err := transport.ListenUnix(bridgeSock)
	if err != nil {
		log.Fatalf("remote-cc-adapter: listen exec bridge %s: %v", bridgeSock, err)
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

	// serve-fs-only: run the brain-side services and block (claude/interceptor
	// launched separately, e.g. the Linux FUSE client or an external harness).
	if *serveFSOnly {
		logger.Printf("serving fs-RPC on %s and exec bridge on %s (no claude spawn)", *adapterSock, bridgeSock)
		sigc := make(chan os.Signal, 2)
		signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
		<-sigc
		return
	}

	// Build and spawn the intercepted claude process.
	cfg := &adapter.LaunchConfig{
		ClaudePath:     claudeToRun,
		Args:           flag.Args(),
		WorkDir:        *workDir,
		AdapterSock:    *adapterSock,
		ExecutorSock:   bridgeSock,
		SpawnProxyPath: *spawnProxy,
		RemotePrefixes: route.RemotePrefixes(),
		SpawnSentinel:  *sentinel,
		DylibPath:      *dylibPath,
		SupervisorPath: *supervisor,
		FuseMnt:        *fuseMnt,
	}
	cmd, err := cfg.BuildCommand()
	if err != nil {
		log.Fatalf("remote-cc-adapter: %v", err)
	}
	if *printCmd {
		fmt.Println(strings.Join(cmd.Args, " "))
		for _, kv := range injectedEnv(cfg) {
			fmt.Println("env:", kv)
		}
		return
	}

	// Forward termination signals to claude.
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	if err := cmd.Start(); err != nil {
		log.Fatalf("remote-cc-adapter: start claude: %v", err)
	}
	go func() {
		for s := range sigc {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(s)
			}
		}
	}()
	os.Exit(exitCode(cmd.Wait()))
}

func defaultAdapterSock() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("rcc-adapter-%d.sock", os.Getpid()))
}

// defaultLocalPrefixes returns the paths that must stay on the brain host under
// -default-remote (ModeLocalAllowlist), regardless of operator config: claude's
// config home (CLAUDE_CONFIG_DIR or ~/.claude — projects, plans, credentials,
// settings) and its global config file ~/.claude.json. Without these, a
// default-remote launch would forward claude's own credential/session reads to
// the executor. Verified against claude's envUtils.getClaudeConfigHomeDir
// (CLAUDE_CONFIG_DIR ?? join(homedir(), ".claude")).
func defaultLocalPrefixes() []string {
	var out []string
	if cfg := os.Getenv("CLAUDE_CONFIG_DIR"); cfg != "" {
		out = append(out, cfg)
	} else if home := os.Getenv("HOME"); home != "" {
		out = append(out, filepath.Join(home, ".claude"))
	}
	if home := os.Getenv("HOME"); home != "" {
		out = append(out, filepath.Join(home, ".claude.json"))
	}
	return out
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

func injectedEnv(cfg *adapter.LaunchConfig) []string {
	env := []string{
		adapter.EnvAdapterSock + "=" + cfg.AdapterSock,
		adapter.EnvExecutorSock + "=" + cfg.ExecutorSock,
		adapter.EnvSpawnProxy + "=" + cfg.SpawnProxyPath,
		adapter.EnvRemotePrefix + "=" + strings.Join(cfg.RemotePrefixes, ":"),
		adapter.EnvSpawnSentinel + "=" + cfg.SpawnSentinel,
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
