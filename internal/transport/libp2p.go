package transport

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/multiformats/go-multiaddr"
)

// RCCProtocol is the libp2p protocol ID for remote-cc-adapter streams. fs IO-RPC
// and subprocess streams share it as separate streams on one connection (design
// doc §3.3); the executor's stream-kind prefix byte still distinguishes them.
const RCCProtocol protocol.ID = "/rcc/1.0.0"

// HostConfig configures a libp2p host.
type HostConfig struct {
	// ListenAddrs are multiaddrs to listen on, e.g. "/ip4/0.0.0.0/tcp/0".
	ListenAddrs []string
	// PrivKey is the host identity. If nil, a fresh Ed25519 key is generated
	// (the PeerID is that key's hash — trust is pinned by exchanging PeerIDs).
	PrivKey crypto.PrivKey
	// EnableHolePunching turns on DCUtR direct-connection upgrade for NAT
	// traversal (both the NAT'd serve and the local dialer set it).
	EnableHolePunching bool
	// ForceReachabilityPrivate marks the host as behind NAT so it acts as the
	// DCUtR initiator; set it on a serve that reserves on a relay. Explicit
	// reservations are made with ReserveRelays, not AutoRelay.
	ForceReachabilityPrivate bool
	// AnnounceAddrs, if set, are extra multiaddrs appended to what the host
	// advertises (e.g. the public "/dns4/host/tcp/443/tls/ws" a relay is
	// reachable at behind an HTTPS proxy that the host itself cannot observe).
	AnnounceAddrs []string
}

// NewLibp2pHost builds a libp2p host with Noise/TLS security (PeerID == public
// key). Defaults already include TCP+QUIC transports and yamux muxing.
func NewLibp2pHost(cfg HostConfig) (host.Host, error) {
	priv := cfg.PrivKey
	if priv == nil {
		var err error
		priv, _, err = crypto.GenerateEd25519Key(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("transport: generate key: %w", err)
		}
	}
	opts := []libp2p.Option{libp2p.Identity(priv)}
	if len(cfg.ListenAddrs) > 0 {
		opts = append(opts, libp2p.ListenAddrStrings(cfg.ListenAddrs...))
	}
	if cfg.EnableHolePunching {
		opts = append(opts, libp2p.EnableHolePunching())
	}
	if cfg.ForceReachabilityPrivate {
		opts = append(opts, libp2p.ForceReachabilityPrivate())
	}
	if announce := parseMultiaddrs(cfg.AnnounceAddrs); len(announce) > 0 {
		opts = append(opts, libp2p.AddrsFactory(func(addrs []multiaddr.Multiaddr) []multiaddr.Multiaddr {
			return append(announce, addrs...)
		}))
	}
	return libp2p.New(opts...)
}

// parseMultiaddrs best-effort parses plain multiaddr strings (no /p2p/ suffix
// required), dropping any that fail to parse.
func parseMultiaddrs(addrs []string) []multiaddr.Multiaddr {
	var out []multiaddr.Multiaddr
	for _, a := range addrs {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		if ma, err := multiaddr.NewMultiaddr(a); err == nil {
			out = append(out, ma)
		}
	}
	return out
}

// parseAddrInfos best-effort parses p2p multiaddrs into peer.AddrInfo.
func parseAddrInfos(addrs []string) []peer.AddrInfo {
	var out []peer.AddrInfo
	for _, a := range addrs {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		ma, err := multiaddr.NewMultiaddr(a)
		if err != nil {
			continue
		}
		if info, err := peer.AddrInfoFromP2pAddr(ma); err == nil {
			out = append(out, *info)
		}
	}
	return out
}

// Libp2pListener accepts inbound RCC streams on a host.
type Libp2pListener struct {
	h        host.Host
	incoming chan network.Stream
	closed   chan struct{}
}

// ListenLibp2p registers the RCC stream handler on h and returns a Listener.
func ListenLibp2p(h host.Host) *Libp2pListener {
	l := &Libp2pListener{
		h:        h,
		incoming: make(chan network.Stream, 32),
		closed:   make(chan struct{}),
	}
	h.SetStreamHandler(RCCProtocol, func(s network.Stream) {
		select {
		case l.incoming <- s:
		case <-l.closed:
			_ = s.Reset()
		}
	})
	return l
}

// Accept returns the next inbound stream.
func (l *Libp2pListener) Accept() (Stream, error) {
	select {
	case s := <-l.incoming:
		return s, nil
	case <-l.closed:
		return nil, errors.New("transport: libp2p listener closed")
	}
}

// Addr returns the host's PeerID and its full p2p multiaddrs (what a dialer
// connects to).
func (l *Libp2pListener) Addr() string {
	parts := []string{l.h.ID().String()}
	suffix := "/p2p/" + l.h.ID().String()
	for _, a := range l.h.Addrs() {
		parts = append(parts, a.String()+suffix)
	}
	return strings.Join(parts, " ")
}

// Close stops accepting and closes the host.
func (l *Libp2pListener) Close() error {
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	l.h.RemoveStreamHandler(RCCProtocol)
	return l.h.Close()
}

// Libp2pDialer opens RCC streams to a peer over libp2p.
type Libp2pDialer struct {
	h    host.Host
	peer peer.AddrInfo
}

// NewLibp2pDialer builds a dialer for peer info (PeerID + its multiaddrs).
func NewLibp2pDialer(h host.Host, info peer.AddrInfo) *Libp2pDialer {
	return &Libp2pDialer{h: h, peer: info}
}

// DialLibp2pPeer parses a "/ip4/.../tcp/.../p2p/<peerid>" multiaddr into a dialer.
func DialLibp2pPeer(h host.Host, p2pAddr string) (*Libp2pDialer, error) {
	ma, err := multiaddr.NewMultiaddr(strings.TrimSpace(p2pAddr))
	if err != nil {
		return nil, fmt.Errorf("transport: bad peer multiaddr: %w", err)
	}
	info, err := peer.AddrInfoFromP2pAddr(ma)
	if err != nil {
		return nil, fmt.Errorf("transport: bad peer addr info: %w", err)
	}
	return &Libp2pDialer{h: h, peer: *info}, nil
}

// Dial connects to the peer (if not already connected) and opens a new stream.
func (d *Libp2pDialer) Dial(ctx context.Context) (Stream, error) {
	if len(d.peer.Addrs) > 0 {
		if err := d.h.Connect(ctx, d.peer); err != nil {
			return nil, fmt.Errorf("transport: connect peer: %w", err)
		}
	}
	// Allow the stream to ride a limited (circuit-relay) connection. DCUtR still
	// tries to upgrade to a direct connection first; without this, opening a
	// stream over a relay-only peer blocks until the hole-punch succeeds and
	// fails if it can't — defeating the relay fallback.
	ctx = network.WithAllowLimitedConn(ctx, "rcc")
	s, err := d.h.NewStream(ctx, d.peer.ID, RCCProtocol)
	if err != nil {
		return nil, fmt.Errorf("transport: open stream: %w", err)
	}
	return s, nil
}

// Close closes the underlying host.
func (d *Libp2pDialer) Close() error { return d.h.Close() }
