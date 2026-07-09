package main

// rca serve — the remote side. Runs the executor that serves filesystem ops and
// subprocesses forwarded by the local `rca <command>` session. Two transports: a
// local Unix socket (--sock) for when both sides are co-located, and go-libp2p
// (default; Noise-secured, PeerID-addressed) for crossing machines. In libp2p
// mode it prints a pairing code that the local side passes as --code.

import (
	"context"
	"flag"
	"log"
	"os"
	"strings"

	"github.com/hoveychen/remote-cc-adapter/internal/executor"
	"github.com/hoveychen/remote-cc-adapter/internal/transport"
	"github.com/libp2p/go-libp2p/core/peer"
)

func cmdServe(args []string) int {
	fs := flag.NewFlagSet("rca serve", flag.ExitOnError)
	sock := fs.String("sock", "", "unix socket path to listen on (co-located mode; disables libp2p)")
	listen := fs.String("listen", "/ip4/0.0.0.0/tcp/0", "comma-separated libp2p listen multiaddrs")
	holePunch := fs.Bool("hole-punch", true, "enable DCUtR hole-punching in libp2p mode")
	relays := fs.String("relays", "", "comma-separated circuit-relay peer multiaddrs (fallback)")
	_ = fs.Parse(args)

	logger := log.New(os.Stderr, "rca serve ", log.LstdFlags|log.Lmsgprefix)

	var ln transport.Listener
	if *sock != "" {
		var err error
		if ln, err = transport.ListenUnix(*sock); err != nil {
			logger.Printf("listen: %v", err)
			return 1
		}
	} else {
		relayList := splitCSV(*relays)
		h, err := transport.NewLibp2pHost(transport.HostConfig{
			ListenAddrs:              splitCSV(*listen),
			EnableHolePunching:       *holePunch,
			ForceReachabilityPrivate: len(relayList) > 0,
		})
		if err != nil {
			logger.Printf("libp2p host: %v", err)
			return 1
		}
		var reserved []peer.ID
		if len(relayList) > 0 {
			reserved = transport.ReserveRelays(context.Background(), h, relayList, logger)
		}
		ln = transport.ListenLibp2p(h)
		printPairingCode(h, relayList, reserved, logger)
	}
	defer ln.Close()

	exe := executor.New(ln, logger)
	logger.Printf("serving on %s", ln.Addr())
	if err := exe.Serve(); err != nil {
		logger.Printf("serve: %v", err)
		return 1
	}
	return 0
}

func splitCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
