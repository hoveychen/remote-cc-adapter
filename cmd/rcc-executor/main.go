// Command rcc-executor is the remote sidecar. It runs inside the sandbox and
// executes filesystem and subprocess operations forwarded by the brain-side
// adapter. See design doc §3.1 component 3.
//
// Two transports: a local Unix socket (-sock) for when the brain and sidecar
// are co-located, and go-libp2p (-libp2p) for crossing machines (Noise-secured,
// PeerID-addressed; design doc §3.3). In libp2p mode it prints its PeerID and
// multiaddrs on startup — copy one into the adapter's --peer flag.
package main

import (
	"flag"
	"log"
	"os"
	"strings"

	"github.com/hoveychen/remote-cc-adapter/internal/executor"
	"github.com/hoveychen/remote-cc-adapter/internal/transport"
)

func main() {
	sock := flag.String("sock", "", "unix socket path to listen on (co-located mode)")
	libp2pListen := flag.String("libp2p", "", "comma-separated libp2p listen multiaddrs, e.g. /ip4/0.0.0.0/tcp/0 (cross-machine mode)")
	holePunch := flag.Bool("hole-punch", true, "enable DCUtR hole-punching in libp2p mode")
	relays := flag.String("relays", "", "comma-separated circuit-relay peer multiaddrs (fallback)")
	flag.Parse()

	if (*sock == "") == (*libp2pListen == "") {
		log.Fatal("rcc-executor: exactly one of -sock or -libp2p is required")
	}

	logger := log.New(os.Stderr, "rcc-executor ", log.LstdFlags|log.Lmsgprefix)

	var ln transport.Listener
	if *libp2pListen != "" {
		h, err := transport.NewLibp2pHost(transport.HostConfig{
			ListenAddrs:        splitCSV(*libp2pListen),
			EnableHolePunching: *holePunch,
			StaticRelays:       splitCSV(*relays),
		})
		if err != nil {
			log.Fatalf("rcc-executor: libp2p host: %v", err)
		}
		ln = transport.ListenLibp2p(h)
	} else {
		var err error
		if ln, err = transport.ListenUnix(*sock); err != nil {
			log.Fatalf("rcc-executor: listen: %v", err)
		}
	}
	defer ln.Close()

	exe := executor.New(ln, logger)
	logger.Printf("serving on %s", ln.Addr())
	if err := exe.Serve(); err != nil {
		log.Fatalf("rcc-executor: serve: %v", err)
	}
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
