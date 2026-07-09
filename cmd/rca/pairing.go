package main

import (
	"log"

	"github.com/libp2p/go-libp2p/core/host"
)

// printPairingCode tells the operator how to point the local side at this
// executor. P1 skeleton: raw PeerID + multiaddrs; the compact pairing code
// replaces this in P2.
func printPairingCode(h host.Host, logger *log.Logger) {
	suffix := "/p2p/" + h.ID().String()
	for _, a := range h.Addrs() {
		logger.Printf("dial with: --peer %s%s", a, suffix)
	}
}
