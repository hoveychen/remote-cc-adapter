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
	)
	var remotePrefixes, localPrefixes stringList
	flag.Var(&remotePrefixes, "remote-prefix", "path prefix routed to the remote executor (repeatable)")
	flag.Var(&localPrefixes, "local-prefix", "path prefix kept local under -default-remote (repeatable)")
	flag.Parse()

	if *claudePath == "" || *spawnProxy == "" {
		log.Fatal("remote-cc-adapter: --claude and --spawn-proxy are required")
	}
	if (*executorSock == "") == (*peerAddr == "") {
		log.Fatal("remote-cc-adapter: exactly one of --executor-sock or --peer is required")
	}

	logger := log.New(os.Stderr, "adapter ", log.LstdFlags|log.Lmsgprefix)

	mode := routing.ModeRemoteAllowlist
	if *localMode {
		mode = routing.ModeLocalAllowlist
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

	cfg := &adapter.LaunchConfig{
		ClaudePath:     claudeToRun,
		Args:           flag.Args(),
		WorkDir:        *workDir,
		AdapterSock:    *adapterSock,
		ExecutorSock:   *executorSock,
		SpawnProxyPath: *spawnProxy,
		RemotePrefixes: route.RemotePrefixes(),
		SpawnSentinel:  *sentinel,
		DylibPath:      *dylibPath,
		SupervisorPath: *supervisor,
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
	go func() {
		if err := ad.Serve(); err != nil {
			logger.Printf("io-rpc server stopped: %v", err)
		}
	}()

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
	err = cmd.Wait()
	os.Exit(exitCode(err))
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
