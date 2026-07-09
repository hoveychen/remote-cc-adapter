package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/hoveychen/remote-cc-adapter/internal/paircode"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// printPairingCode prints the copy-pasteable code the local side passes as
// --code. The code packs the host's PeerID plus its dialable listen addrs
// (loopback is dropped unless it is all we have, so co-located testing still
// works). For each relay this serve successfully reserved on, it appends the
// relayed "/p2p-circuit" address so a NAT'd local side can reach this serve
// through the relay. Falls back to raw --peer multiaddrs if encoding fails.
func printPairingCode(h host.Host, relays []string, reserved []peer.ID, logger *log.Logger) {
	addrs := h.Addrs()
	var dialable []multiaddr.Multiaddr
	for _, a := range addrs {
		if !manet.IsIPLoopback(a) {
			dialable = append(dialable, a)
		}
	}
	if len(dialable) == 0 {
		dialable = addrs
	}
	// Deterministically add the relayed address for each reserved relay, built
	// from the relay's public multiaddr the operator supplied (behind an HTTPS
	// proxy the relay cannot observe its own public /wss addr, so we cannot rely
	// on it being advertised — synthesize it here).
	dialable = appendCircuitAddrs(dialable, relays, reserved, logger)
	code, err := paircode.Encode(peer.AddrInfo{ID: h.ID(), Addrs: dialable})
	if err != nil {
		logger.Printf("pairing code: %v; dial with --peer instead:", err)
		suffix := "/p2p/" + h.ID().String()
		for _, a := range addrs {
			logger.Printf("  --peer %s%s", a, suffix)
		}
		return
	}
	fmt.Fprintf(os.Stdout, "pairing code:\n\n  %s\n\nrun on the local machine:\n\n  rca <command> --code %s\n\n", code, code)
}

// appendCircuitAddrs synthesizes the relayed "/p2p-circuit" multiaddr for each
// relay this serve actually reserved on (its public p2p multiaddr + /p2p-circuit)
// and appends any not already present, so the pairing code carries a dialable
// relayed path. Relays with no live reservation are skipped — a circuit addr
// for them would be a dead end.
func appendCircuitAddrs(addrs []multiaddr.Multiaddr, relays []string, reserved []peer.ID, logger *log.Logger) []multiaddr.Multiaddr {
	if len(reserved) == 0 {
		logger.Printf("warning: no relay reservation succeeded; pairing code carries no relayed address")
		return addrs
	}
	reservedSet := make(map[peer.ID]bool, len(reserved))
	for _, id := range reserved {
		reservedSet[id] = true
	}
	circuit, err := multiaddr.NewMultiaddr("/p2p-circuit")
	if err != nil {
		return addrs
	}
	have := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		have[a.String()] = true
	}
	for _, r := range relays {
		if r = strings.TrimSpace(r); r == "" {
			continue
		}
		rma, err := multiaddr.NewMultiaddr(r)
		if err != nil {
			logger.Printf("warning: bad relay multiaddr %q: %v", r, err)
			continue
		}
		info, err := peer.AddrInfoFromP2pAddr(rma)
		if err != nil || !reservedSet[info.ID] {
			continue // no reservation on this relay
		}
		relayed := rma.Encapsulate(circuit)
		if s := relayed.String(); !have[s] {
			addrs = append(addrs, relayed)
			have[s] = true
		}
	}
	return addrs
}
