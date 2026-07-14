// Package adapter is the brain-side host (design doc §3.1 component 1). It:
//
//   - listens on a Unix socket that the native interceptor (macOS dylib / Linux
//     seccomp supervisor) connects to and speaks the fs IO-RPC protocol over;
//   - for each intercepted fs op, consults the routing table (internal/routing)
//     and either serves it locally against the brain host's filesystem or relays
//     it to the remote executor over the transport (internal/transport);
//   - spawns the claude process with the interceptor injected (see launch.go).
//
// Handle namespacing: OpOpen may resolve to either side, and each side hands
// back a side-local handle. The adapter wraps those in its own handle space so
// later OpPread/OpClose calls reach the correct side. The interceptor keeps the
// open/read/close sequence for one file on a single connection (see the native
// READMEs), so per-connection handle tables are sufficient.
package adapter

import (
	"context"
	"fmt"
	"io"
	"sync"
	"syscall"

	"github.com/hoveychen/remote-adapter/internal/executor"
	"github.com/hoveychen/remote-adapter/internal/protocol"
	"github.com/hoveychen/remote-adapter/internal/routing"
	"github.com/hoveychen/remote-adapter/internal/transport"
)

// Logger is the minimal logging surface the adapter needs.
type Logger interface {
	Printf(format string, args ...any)
}

// Adapter serves the native interceptor's fs IO-RPC and routes ops.
type Adapter struct {
	ln     transport.Listener // interceptor-facing listener (Unix socket)
	dialer transport.Dialer   // executor-facing transport
	route  *routing.Table
	logger Logger
}

// New builds an adapter. ln accepts interceptor connections; dialer reaches the
// executor; route decides local vs remote per path.
func New(ln transport.Listener, dialer transport.Dialer, route *routing.Table, logger Logger) *Adapter {
	return &Adapter{ln: ln, dialer: dialer, route: route, logger: logger}
}

// Serve accepts interceptor connections until the listener closes.
func (a *Adapter) Serve() error {
	a.logf("adapter IO-RPC listening on %s", a.ln.Addr())
	for {
		stream, err := a.ln.Accept()
		if err != nil {
			return err
		}
		go a.serveConn(stream)
	}
}

func (a *Adapter) serveConn(stream io.ReadWriteCloser) {
	s := &session{
		adapter: a,
		localFS: executor.NewFSService(a.logger),
		handles: make(map[uint64]handleRef),
		next:    1,
	}
	defer s.close()
	conn := protocol.NewConn(stream)
	for {
		req, err := conn.ReadRequest()
		if err != nil {
			return
		}
		resp := s.handle(req)
		if err := conn.SendResponse(req.Op, resp); err != nil {
			return
		}
	}
}

func (a *Adapter) logf(format string, args ...any) {
	if a.logger != nil {
		a.logger.Printf(format, args...)
	}
}

// QueryServerInfo asks the executor for its GOOS/GOARCH over a one-shot fs
// stream. It is used before launching the agent to decide whether the agent's
// perceived OS (the local host the CLI runs on) differs from where its routed
// subprocesses actually execute. An executor built before OpServerInfo hits its
// ReadRequest default and closes the stream; that surfaces here as an error and
// the caller skips the hint rather than failing the launch — so a new run
// client still talks to an old `rca serve`.
func QueryServerInfo(ctx context.Context, dialer transport.Dialer) (osName, arch string, err error) {
	stream, err := dialer.Dial(ctx)
	if err != nil {
		return "", "", fmt.Errorf("dial executor: %w", err)
	}
	defer stream.Close()
	if _, err := stream.Write([]byte{executor.StreamKindFS}); err != nil {
		return "", "", fmt.Errorf("write stream kind: %w", err)
	}
	conn := protocol.NewConn(stream)
	if err := conn.SendRequest(&protocol.Request{Op: protocol.OpServerInfo}); err != nil {
		return "", "", fmt.Errorf("send server-info: %w", err)
	}
	resp, err := conn.ReadResponse(protocol.OpServerInfo)
	if err != nil {
		return "", "", fmt.Errorf("read server-info: %w", err)
	}
	if resp.Err != 0 {
		return "", "", fmt.Errorf("executor server-info errno %d", resp.Err)
	}
	return resp.OS, resp.Arch, nil
}

// handleRef records which side an adapter handle lives on and its side-local id.
type handleRef struct {
	remote bool
	real   uint64
}

// session is per interceptor connection state.
type session struct {
	adapter *Adapter
	localFS *executor.FSService

	mu      sync.Mutex
	handles map[uint64]handleRef
	next    uint64

	remote *protocol.Conn // lazily dialed executor fs stream
}

func (s *session) handle(req *protocol.Request) *protocol.Response {
	switch req.Op {
	case protocol.OpStat, protocol.OpReaddir, protocol.OpWriteFile,
		protocol.OpUnlink, protocol.OpMkdir, protocol.OpSetattr:
		return s.forwardByPath(req)
	case protocol.OpRename:
		return s.rename(req)
	case protocol.OpOpen, protocol.OpCreate:
		return s.open(req)
	case protocol.OpPread, protocol.OpPwrite, protocol.OpClose:
		return s.byHandle(req)
	default:
		return &protocol.Response{Err: -int32(syscall.EINVAL)}
	}
}

// rename refuses to cross the routing boundary. Both endpoints must live on the
// same side; a rename that would move a file between the brain host and the
// executor is a cross-device link as far as the caller is concerned, and EXDEV
// is what makes callers fall back to copy+unlink.
func (s *session) rename(req *protocol.Request) *protocol.Response {
	if s.adapter.route.IsRemote(req.Path) != s.adapter.route.IsRemote(req.Path2) {
		return &protocol.Response{Err: -int32(syscall.EXDEV)}
	}
	return s.forwardByPath(req)
}

// forwardByPath routes path-addressed ops with no handle bookkeeping.
func (s *session) forwardByPath(req *protocol.Request) *protocol.Response {
	if s.adapter.route.IsRemote(req.Path) {
		return s.remoteRPC(req)
	}
	return s.localFS.Handle(req)
}

// open routes by path, then wraps the side-local handle in the adapter space.
func (s *session) open(req *protocol.Request) *protocol.Response {
	remote := s.adapter.route.IsRemote(req.Path)
	var resp *protocol.Response
	if remote {
		resp = s.remoteRPC(req)
	} else {
		resp = s.localFS.Handle(req)
	}
	if resp.Err != 0 {
		return resp
	}
	s.mu.Lock()
	a := s.next
	s.next++
	s.handles[a] = handleRef{remote: remote, real: resp.Handle}
	s.mu.Unlock()
	resp.Handle = a
	return resp
}

// byHandle rewrites the adapter handle to its side-local id and forwards.
func (s *session) byHandle(req *protocol.Request) *protocol.Response {
	s.mu.Lock()
	ref, ok := s.handles[req.Handle]
	if ok && req.Op == protocol.OpClose {
		delete(s.handles, req.Handle)
	}
	s.mu.Unlock()
	if !ok {
		return &protocol.Response{Err: -int32(syscall.EBADF)}
	}
	req.Handle = ref.real
	if ref.remote {
		return s.remoteRPC(req)
	}
	return s.localFS.Handle(req)
}

// remoteRPC relays one request to the executor over the (lazily dialed) fs
// stream and returns the response.
func (s *session) remoteRPC(req *protocol.Request) *protocol.Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remote == nil {
		stream, err := s.adapter.dialer.Dial(context.Background())
		if err != nil {
			s.adapter.logf("remote dial failed: %v", err)
			return &protocol.Response{Err: -int32(syscall.EIO)}
		}
		if _, err := stream.Write([]byte{executor.StreamKindFS}); err != nil {
			stream.Close()
			return &protocol.Response{Err: -int32(syscall.EIO)}
		}
		s.remote = protocol.NewConn(stream)
	}
	if err := s.remote.SendRequest(req); err != nil {
		s.adapter.logf("remote send failed: %v", err)
		return &protocol.Response{Err: -int32(syscall.EIO)}
	}
	resp, err := s.remote.ReadResponse(req.Op)
	if err != nil {
		s.adapter.logf("remote recv failed: %v", err)
		return &protocol.Response{Err: -int32(syscall.EIO)}
	}
	return resp
}

func (s *session) close() {
	s.localFS.CloseAll()
	s.mu.Lock()
	if s.remote != nil {
		s.remote.Close()
		s.remote = nil
	}
	s.mu.Unlock()
}
