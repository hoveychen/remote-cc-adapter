package transport

// Stdio transport: multiplex executor streams (internal/protocol) over a single
// bidirectional byte pipe using yamux. This is the ssh-friendly transport —
// `rca serve --stdio` speaks yamux on its own stdin/stdout, and the run side
// spawns e.g. `ssh host rca serve --stdio` and dials over that child's pipe.
//
// Unlike UnixDialer (a fresh connection per Dial, which breaks when forwarded
// over `ssh -L` because ssh does not faithfully propagate half-close), this
// keeps ONE long-lived pipe and opens logical streams as yamux frames — so ssh
// only ever sees a single stream of bytes it transports verbatim. Mirrors
// Libp2pDialer's "one connection, many streams via Dial" shape (libp2p muxes
// with yamux internally; here we use it directly, no libp2p host / pairing code).

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/libp2p/go-yamux/v5"
)

// pipeConn adapts a reader + writer (+ optional closer) into a net.Conn so
// yamux can run over it. The pipe has no independent deadline support, so
// deadlines are no-ops and the stdio config disables keepalive (which relies
// on SetDeadline).
type pipeConn struct {
	r io.Reader
	w io.Writer
	c io.Closer
}

func (p *pipeConn) Read(b []byte) (int, error)  { return p.r.Read(b) }
func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeConn) Close() error {
	if p.c != nil {
		return p.c.Close()
	}
	return nil
}
func (p *pipeConn) LocalAddr() net.Addr                { return stdioAddr{} }
func (p *pipeConn) RemoteAddr() net.Addr               { return stdioAddr{} }
func (p *pipeConn) SetDeadline(time.Time) error        { return nil }
func (p *pipeConn) SetReadDeadline(time.Time) error    { return nil }
func (p *pipeConn) SetWriteDeadline(time.Time) error   { return nil }

type stdioAddr struct{}

func (stdioAddr) Network() string { return "stdio" }
func (stdioAddr) String() string  { return "stdio" }

func stdioConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	// Keepalive uses SetDeadline, which is a no-op over a raw pipe; disable it.
	c.EnableKeepAlive = false
	return c
}

// StdioDialer opens executor streams by multiplexing (yamux client) over a
// single pipe. Satisfies Dialer.
type StdioDialer struct {
	sess *yamux.Session
}

// NewStdioDialer starts a yamux client session over the pipe (r = bytes from
// the executor, w = bytes to the executor; c closes the underlying pipe, may
// be nil).
func NewStdioDialer(r io.Reader, w io.Writer, c io.Closer) (*StdioDialer, error) {
	sess, err := yamux.Client(&pipeConn{r: r, w: w, c: c}, stdioConfig(), nil)
	if err != nil {
		return nil, err
	}
	return &StdioDialer{sess: sess}, nil
}

// Dial opens a new logical stream to the executor.
func (d *StdioDialer) Dial(ctx context.Context) (Stream, error) {
	return d.sess.OpenStream(ctx)
}

// Close tears down the session and the underlying pipe.
func (d *StdioDialer) Close() error { return d.sess.Close() }

// StdioListener accepts executor streams multiplexed over a single pipe —
// typically the process's own stdin/stdout under `rca serve --stdio`.
// Satisfies Listener.
type StdioListener struct {
	sess *yamux.Session
}

// NewStdioListener starts a yamux server session over the pipe (r = bytes from
// the run side, w = bytes to the run side; c may be nil).
func NewStdioListener(r io.Reader, w io.Writer, c io.Closer) (*StdioListener, error) {
	sess, err := yamux.Server(&pipeConn{r: r, w: w, c: c}, stdioConfig(), nil)
	if err != nil {
		return nil, err
	}
	return &StdioListener{sess: sess}, nil
}

// Accept blocks until the next inbound stream arrives.
func (l *StdioListener) Accept() (Stream, error) { return l.sess.AcceptStream() }

// Addr returns a human-readable address for logs.
func (l *StdioListener) Addr() string { return "stdio" }

// Close stops accepting and tears down the session.
func (l *StdioListener) Close() error { return l.sess.Close() }
