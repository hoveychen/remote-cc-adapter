// Package executor is the remote sidecar (design doc §3.1 component 3). It runs
// inside the sandbox and executes, on its own host, the filesystem and
// subprocess operations that the brain-side adapter forwards to it.
//
// A single transport listener multiplexes two stream kinds, distinguished by a
// one-byte prefix the caller writes immediately after connecting:
//
//	StreamKindFS   ('F') : protocol frames (internal/protocol) — fs IO-RPC.
//	StreamKindExec ('X') : execproto frames — one streamed subprocess.
package executor

import (
	"io"

	"github.com/hoveychen/remote-adapter/internal/protocol"
	"github.com/hoveychen/remote-adapter/internal/transport"
)

// Stream-kind prefix bytes written by the caller right after connecting.
const (
	StreamKindFS   byte = 'F'
	StreamKindExec byte = 'X'
)

// Logger is the minimal logging surface the executor needs.
type Logger interface {
	Printf(format string, args ...any)
}

// Executor accepts transport streams and serves fs / exec requests.
type Executor struct {
	ln     transport.Listener
	logger Logger
	// embeddedRg is a bundled ripgrep this host runs when a routed rg spawn
	// references a binary absent here (cross-OS, claude re-execs its own host-OS
	// binary with argv[0]=rg). Empty disables the fallback. Set via SetEmbeddedRg.
	embeddedRg string
}

// New builds an executor serving on ln.
func New(ln transport.Listener, logger Logger) *Executor {
	return &Executor{ln: ln, logger: logger}
}

// SetEmbeddedRg configures a bundled ripgrep the executor runs when a routed
// rg spawn (argv[0] basename "rg") references a binary path that does not exist
// on this host and this host has no rg of its own. Empty leaves the fallback off.
func (e *Executor) SetEmbeddedRg(path string) { e.embeddedRg = path }

// Serve accepts streams until the listener is closed. Each stream is handled in
// its own goroutine.
func (e *Executor) Serve() error {
	e.logf("executor listening on %s", e.ln.Addr())
	for {
		stream, err := e.ln.Accept()
		if err != nil {
			return err
		}
		go e.handle(stream)
	}
}

func (e *Executor) handle(stream io.ReadWriteCloser) {
	var kind [1]byte
	if _, err := io.ReadFull(stream, kind[:]); err != nil {
		stream.Close()
		return
	}
	switch kind[0] {
	case StreamKindFS:
		e.serveFS(stream)
	case StreamKindExec:
		e.serveExec(stream)
	default:
		e.logf("unknown stream kind %q", kind[0])
		stream.Close()
	}
}

// serveFS serves fs IO-RPC frames on a single stream until it closes. Each
// stream gets its own handle table so handles never leak across connections.
func (e *Executor) serveFS(stream io.ReadWriteCloser) {
	defer stream.Close()
	fs := NewFSService(e.logger)
	defer fs.CloseAll()
	conn := protocol.NewConn(stream)
	for {
		req, err := conn.ReadRequest()
		if err != nil {
			return
		}
		resp := fs.Handle(req)
		if err := conn.SendResponse(req.Op, resp); err != nil {
			return
		}
	}
}

func (e *Executor) logf(format string, args ...any) {
	if e.logger != nil {
		e.logger.Printf(format, args...)
	}
}
