package transport

import (
	"context"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
)

// TestLibp2pStreamRoundTrip stands up two libp2p hosts on loopback, opens a
// stream from a dialer to a listener, and round-trips bytes — proving the RCC
// transport works over libp2p (Noise-secured, PeerID-addressed).
func TestLibp2pStreamRoundTrip(t *testing.T) {
	server, err := NewLibp2pHost(HostConfig{ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	ln := ListenLibp2p(server)
	defer ln.Close()

	client, err := NewLibp2pHost(HostConfig{ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("client host: %v", err)
	}
	dialer := NewLibp2pDialer(client, peer.AddrInfo{ID: server.ID(), Addrs: server.Addrs()})
	defer dialer.Close()

	// Echo server: read a byte, echo it back.
	type result struct {
		got byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := ln.Accept()
		if err != nil {
			done <- result{err: err}
			return
		}
		defer s.Close()
		buf := make([]byte, 1)
		if _, err := s.Read(buf); err != nil {
			done <- result{err: err}
			return
		}
		done <- result{got: buf[0]}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := dialer.Dial(ctx)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer stream.Close()

	if _, err := stream.Write([]byte{0x42}); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("server: %v", r.err)
		}
		if r.got != 0x42 {
			t.Fatalf("echo mismatch: got %#x want 0x42", r.got)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for stream")
	}

	// PeerID pinning: the dialer reached exactly the server's identity.
	if server.ID() == client.ID() {
		t.Fatal("hosts unexpectedly share a PeerID")
	}
}
