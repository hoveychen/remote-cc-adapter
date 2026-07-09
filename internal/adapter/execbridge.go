package adapter

import (
	"context"
	"io"
	"sync"

	"github.com/hoveychen/remote-cc-adapter/internal/transport"
)

// ExecBridge lets the subprocess proxy reach the executor over whatever
// transport the adapter uses (unix socket or libp2p), without the proxy needing
// to speak libp2p itself.
//
// The proxy (`rca _spawn-proxy`, exec'd by the interceptor for a routed
// subprocess) connects to the bridge's local unix socket and speaks execproto.
// The bridge does not parse that protocol — it opens one transport stream to the
// executor and splices bytes both ways. The executor's stream-kind prefix (the
// 'X' the proxy writes first) flows through unchanged, so the executor dispatches
// it to its subprocess service exactly as for a co-located direct connection.
type ExecBridge struct {
	ln     transport.Listener // proxy-facing local unix socket
	dialer transport.Dialer   // executor-facing transport (unix or libp2p)
	logger Logger
}

// NewExecBridge builds a bridge serving proxy connections on ln and forwarding
// them to the executor via dialer.
func NewExecBridge(ln transport.Listener, dialer transport.Dialer, logger Logger) *ExecBridge {
	return &ExecBridge{ln: ln, dialer: dialer, logger: logger}
}

// Serve accepts proxy connections until the listener closes.
func (b *ExecBridge) Serve() error {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return err
		}
		go b.handle(conn)
	}
}

func (b *ExecBridge) handle(conn io.ReadWriteCloser) {
	defer conn.Close()
	stream, err := b.dialer.Dial(context.Background())
	if err != nil {
		if b.logger != nil {
			b.logger.Printf("[exec-bridge] dial executor: %v", err)
		}
		return
	}
	defer stream.Close()
	splice(conn, stream)
}

// splice copies bytes both ways until either side closes, then returns.
func splice(a, b io.ReadWriteCloser) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		// Unblock the other direction: closing surfaces EOF to the a->b copy.
		if c, ok := a.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		} else {
			_ = a.Close()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		if c, ok := b.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		} else {
			_ = b.Close()
		}
	}()
	wg.Wait()
}
