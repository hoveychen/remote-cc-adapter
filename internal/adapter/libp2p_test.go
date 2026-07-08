package adapter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/hoveychen/remote-cc-adapter/internal/executor"
	"github.com/hoveychen/remote-cc-adapter/internal/protocol"
	"github.com/hoveychen/remote-cc-adapter/internal/routing"
	"github.com/hoveychen/remote-cc-adapter/internal/transport"
)

// TestAdapterOverLibp2p wires the adapter and executor over a real libp2p
// connection (two loopback hosts) and drives a remote fs op end to end, proving
// the IO-RPC pipeline is transport-agnostic and runs over libp2p.
func TestAdapterOverLibp2p(t *testing.T) {
	remoteDir := t.TempDir()
	body := []byte("served-over-libp2p")
	if err := os.WriteFile(filepath.Join(remoteDir, "f.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	// Executor on a libp2p host.
	execHost, err := transport.NewLibp2pHost(transport.HostConfig{ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatal(err)
	}
	execLn := transport.ListenLibp2p(execHost)
	defer execLn.Close()
	go executor.New(execLn, testLogger{t}).Serve()

	// Adapter dials the executor peer over libp2p.
	brainHost, err := transport.NewLibp2pHost(transport.HostConfig{ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatal(err)
	}
	dialer := transport.NewLibp2pDialer(brainHost, peer.AddrInfo{ID: execHost.ID(), Addrs: execHost.Addrs()})
	defer dialer.Close()

	route := routing.New(routing.ModeRemoteAllowlist, []string{remoteDir}, nil)

	// Interceptor-facing side: use an in-memory pipe as the interceptor conn.
	sess := &session{
		adapter: &Adapter{dialer: dialer, route: route, logger: testLogger{t}},
		localFS: executor.NewFSService(testLogger{t}),
		handles: make(map[uint64]handleRef),
		next:    1,
	}
	defer sess.close()

	// A remote-routed stat must be served by the executor over libp2p.
	remotePath := filepath.Join(remoteDir, "f.txt")
	resp := sess.handle(&protocol.Request{Op: protocol.OpStat, Path: remotePath})
	if resp.Err != 0 || resp.Size != int64(len(body)) {
		t.Fatalf("remote stat over libp2p: err=%d size=%d want size=%d", resp.Err, resp.Size, len(body))
	}

	// A local-routed stat must be served on the brain host (not over libp2p).
	localDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(localDir, "l.txt"), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	lresp := sess.handle(&protocol.Request{Op: protocol.OpStat, Path: filepath.Join(localDir, "l.txt")})
	if lresp.Err != 0 || lresp.Size != 5 {
		t.Fatalf("local stat: err=%d size=%d", lresp.Err, lresp.Size)
	}
}
