package main

// rca relay — a standalone circuit-relay v2 relay. It runs a libp2p host that
// lets NAT'd peers (a `rca serve` behind a firewall and a local `rca <command>`
// dialer) rendezvous: the serve reserves a slot here, the dialer reaches it
// through the relay, and DCUtR then tries to upgrade to a direct connection,
// falling back to relaying the traffic if the hole-punch fails.
//
// It listens on WebSocket by default (/ip4/0.0.0.0/tcp/8080/ws) so it can sit
// behind an HTTPS reverse proxy (e.g. a PaaS that only exposes :8080 as HTTP
// and terminates TLS itself). Point serve at it with:
//
//	rca serve --relays <RCA_RELAY_ANNOUNCE>/p2p/<relayID>
//
// The relay identity must be STABLE across restarts (the PeerID is baked into
// the serve --relays flag and the pairing code). Provide it via RCA_RELAY_KEY
// (base64 std of a marshalled libp2p private key); if unset, a fresh key is
// generated and its RCA_RELAY_KEY value is logged so the operator can pin it.

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hoveychen/remote-cc-adapter/internal/transport"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
)

func cmdRelay(args []string) int {
	fs := flag.NewFlagSet("rca relay", flag.ExitOnError)
	listen := fs.String("listen", "/ip4/0.0.0.0/tcp/8080/ws", "comma-separated libp2p listen multiaddrs")
	announce := fs.String("announce", os.Getenv("RCA_RELAY_ANNOUNCE"),
		"comma-separated public multiaddrs to advertise (e.g. /dns4/host/tcp/443/tls/ws); env RCA_RELAY_ANNOUNCE")
	_ = fs.Parse(args)

	logger := log.New(os.Stderr, "rca relay ", log.LstdFlags|log.Lmsgprefix)

	priv, err := relayIdentity(logger)
	if err != nil {
		logger.Printf("identity: %v", err)
		return 1
	}

	h, err := transport.NewLibp2pHost(transport.HostConfig{
		ListenAddrs:   splitCSV(*listen),
		PrivKey:       priv,
		AnnounceAddrs: splitCSV(*announce),
	})
	if err != nil {
		logger.Printf("libp2p host: %v", err)
		return 1
	}
	defer h.Close()

	// Mount the circuit-relay v2 hop handler directly. libp2p.EnableRelayService
	// only starts the service when the host believes it is publicly reachable;
	// behind an HTTPS proxy that heuristic fails, so we start the relay
	// unconditionally here.
	r, err := relay.New(h)
	if err != nil {
		logger.Printf("relay service: %v", err)
		return 1
	}
	defer r.Close()

	relayID := h.ID().String()
	logger.Printf("relay peer id: %s", relayID)
	for _, a := range h.Addrs() {
		logger.Printf("  reachable at: %s/p2p/%s", a, relayID)
	}
	if *announce == "" {
		logger.Printf("note: no --announce/RCA_RELAY_ANNOUNCE set; behind an HTTPS proxy set it to the public /dns4/<host>/tcp/443/tls/ws so serve can dial in")
	}
	logger.Printf("serve should use: rca serve --relays <public-addr>/p2p/%s", relayID)

	// Run until signalled; the relay service handles peers in the background.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	<-sigc
	logger.Printf("shutting down")
	return 0
}

// relayIdentity returns the relay's stable private key. It reads a base64-std
// marshalled key from RCA_RELAY_KEY; if that is unset it generates a fresh
// Ed25519 key and logs the value to set for a stable identity next time.
func relayIdentity(logger *log.Logger) (crypto.PrivKey, error) {
	if enc := os.Getenv("RCA_RELAY_KEY"); enc != "" {
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			return nil, err
		}
		return crypto.UnmarshalPrivateKey(raw)
	}
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, err
	}
	raw, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, err
	}
	logger.Printf("no RCA_RELAY_KEY set — generated an ephemeral identity; to pin it across restarts set:")
	logger.Printf("  RCA_RELAY_KEY=%s", base64.StdEncoding.EncodeToString(raw))
	return priv, nil
}
