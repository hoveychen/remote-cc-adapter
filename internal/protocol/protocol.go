// Package protocol defines the binary IO-RPC wire format spoken across three
// hops of remote-cc-adapter:
//
//	native interceptor (C)  <-->  adapter (Go)  <-->  executor sidecar (Go)
//
// The same frame format is used on every hop so the adapter can relay a frame
// from the interceptor straight to the executor without re-encoding when a
// path routes remote (see internal/adapter).
//
// Framing: each message is a single length-prefixed frame:
//
//	[uint32 big-endian body length][body bytes]
//
// A request body is: [uint8 op][op-specific fields].
// A response body is: [int32 big-endian errno][op-specific fields].
// errno 0 means success; a negative value is a POSIX errno (e.g. -2 == ENOENT),
// matching what the C interceptor returns to its caller.
//
// Primitive encodings inside a body:
//
//	string : [uint32 len][len bytes]
//	bytes  : [uint32 len][len bytes]
//	u32    : 4 bytes big-endian
//	u64/i64: 8 bytes big-endian
package protocol

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
)

// Op identifies an IO-RPC operation.
type Op uint8

const (
	OpStat      Op = 1 // stat/lstat a path
	OpOpen      Op = 2 // open a path, returns an opaque handle
	OpPread     Op = 3 // positional read from a handle
	OpWriteFile Op = 4 // write a whole file body (interceptor buffers, flushes on close)
	OpClose     Op = 5 // release a handle
	OpReaddir   Op = 6 // list directory entries
)

// String renders an Op for logs.
func (o Op) String() string {
	switch o {
	case OpStat:
		return "STAT"
	case OpOpen:
		return "OPEN"
	case OpPread:
		return "PREAD"
	case OpWriteFile:
		return "WRITEFILE"
	case OpClose:
		return "CLOSE"
	case OpReaddir:
		return "READDIR"
	default:
		return fmt.Sprintf("OP(%d)", uint8(o))
	}
}

// MaxFrame caps a single frame body to guard against corrupt length prefixes.
const MaxFrame = 64 << 20 // 64 MiB

// Request is a decoded IO-RPC request. Only the fields relevant to Op are set.
type Request struct {
	Op     Op
	Path   string // OpStat, OpOpen, OpWriteFile, OpReaddir
	Flags  uint32 // OpOpen: POSIX open flags
	Handle uint64 // OpPread, OpClose
	Off    int64  // OpPread
	Len    uint32 // OpPread: requested length
	Data   []byte // OpWriteFile: file body
}

// Response is a decoded IO-RPC response. errno 0 means success.
type Response struct {
	Err    int32    // 0 ok, else negative POSIX errno
	Mode   uint32   // OpStat/OpOpen: st_mode
	Size   int64    // OpStat/OpOpen: file size
	Handle uint64   // OpOpen: opaque handle
	Data   []byte   // OpPread: bytes read
	Names  []string // OpReaddir: entry names
	Types  []uint8  // OpReaddir: per-entry POSIX d_type (parallel to Names)
}

// POSIX d_type values carried per entry in a READDIR response (BSD/Linux
// dirent.h). Directory type is load-bearing: the native interceptor synthesises
// dirents for the intercepted process, and tools like ripgrep use d_type to
// decide whether to recurse — a directory mislabelled DTReg is never descended.
const (
	DTUnknown uint8 = 0
	DTDir     uint8 = 4
	DTReg     uint8 = 8
	DTLnk     uint8 = 10
)

// IsDir reports whether Mode denotes a directory (S_IFDIR == 0040000).
func (r *Response) IsDir() bool { return r.Mode&0o170000 == 0o040000 }

// --- frame IO -------------------------------------------------------------

func writeFrame(w io.Writer, body []byte) error {
	if len(body) > MaxFrame {
		return fmt.Errorf("protocol: frame too large: %d", len(body))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(body)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return nil, fmt.Errorf("protocol: frame too large: %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	return body, nil
}

// --- body builder / parser -------------------------------------------------

type builder struct{ b []byte }

func (e *builder) u8(v uint8)   { e.b = append(e.b, v) }
func (e *builder) u32(v uint32) { e.b = binary.BigEndian.AppendUint32(e.b, v) }
func (e *builder) u64(v uint64) { e.b = binary.BigEndian.AppendUint64(e.b, v) }
func (e *builder) i32(v int32)  { e.u32(uint32(v)) }
func (e *builder) i64(v int64)  { e.u64(uint64(v)) }
func (e *builder) bytes(v []byte) {
	e.u32(uint32(len(v)))
	e.b = append(e.b, v...)
}
func (e *builder) str(v string) {
	e.u32(uint32(len(v)))
	e.b = append(e.b, v...)
}

type parser struct {
	b   []byte
	err error
}

func (p *parser) fail(msg string) {
	if p.err == nil {
		p.err = fmt.Errorf("protocol: %s", msg)
	}
}
func (p *parser) u8() uint8 {
	if p.err != nil || len(p.b) < 1 {
		p.fail("short u8")
		return 0
	}
	v := p.b[0]
	p.b = p.b[1:]
	return v
}
func (p *parser) u32() uint32 {
	if p.err != nil || len(p.b) < 4 {
		p.fail("short u32")
		return 0
	}
	v := binary.BigEndian.Uint32(p.b)
	p.b = p.b[4:]
	return v
}
func (p *parser) u64() uint64 {
	if p.err != nil || len(p.b) < 8 {
		p.fail("short u64")
		return 0
	}
	v := binary.BigEndian.Uint64(p.b)
	p.b = p.b[8:]
	return v
}
func (p *parser) i32() int32 { return int32(p.u32()) }
func (p *parser) i64() int64 { return int64(p.u64()) }
func (p *parser) bytes() []byte {
	n := p.u32()
	if p.err != nil || uint32(len(p.b)) < n {
		p.fail("short bytes")
		return nil
	}
	v := p.b[:n:n]
	p.b = p.b[n:]
	return v
}
func (p *parser) str() string { return string(p.bytes()) }

// --- request codec ---------------------------------------------------------

// WriteRequest encodes and frames req onto w.
func WriteRequest(w io.Writer, req *Request) error {
	var e builder
	e.u8(uint8(req.Op))
	switch req.Op {
	case OpStat, OpReaddir:
		e.str(req.Path)
	case OpOpen:
		e.u32(req.Flags)
		e.str(req.Path)
	case OpPread:
		e.u64(req.Handle)
		e.i64(req.Off)
		e.u32(req.Len)
	case OpWriteFile:
		e.str(req.Path)
		e.bytes(req.Data)
	case OpClose:
		e.u64(req.Handle)
	default:
		return fmt.Errorf("protocol: unknown request op %d", req.Op)
	}
	return writeFrame(w, e.b)
}

// ReadRequest reads and decodes one framed request from r.
func ReadRequest(r io.Reader) (*Request, error) {
	body, err := readFrame(r)
	if err != nil {
		return nil, err
	}
	p := parser{b: body}
	req := &Request{Op: Op(p.u8())}
	switch req.Op {
	case OpStat, OpReaddir:
		req.Path = p.str()
	case OpOpen:
		req.Flags = p.u32()
		req.Path = p.str()
	case OpPread:
		req.Handle = p.u64()
		req.Off = p.i64()
		req.Len = p.u32()
	case OpWriteFile:
		req.Path = p.str()
		req.Data = p.bytes()
	case OpClose:
		req.Handle = p.u64()
	default:
		return nil, fmt.Errorf("protocol: unknown request op %d", req.Op)
	}
	if p.err != nil {
		return nil, p.err
	}
	return req, nil
}

// --- response codec --------------------------------------------------------

// WriteResponse encodes and frames resp onto w. op must match the request the
// response answers, since the field layout is op-specific.
func WriteResponse(w io.Writer, op Op, resp *Response) error {
	var e builder
	e.i32(resp.Err)
	if resp.Err == 0 {
		switch op {
		case OpStat, OpOpen:
			e.u32(resp.Mode)
			e.i64(resp.Size)
			if op == OpOpen {
				e.u64(resp.Handle)
			}
		case OpPread:
			e.bytes(resp.Data)
		case OpReaddir:
			e.u32(uint32(len(resp.Names)))
			for i, n := range resp.Names {
				e.str(n)
				var t uint8
				if i < len(resp.Types) {
					t = resp.Types[i]
				}
				e.u8(t)
			}
		case OpWriteFile, OpClose:
			// errno only
		}
	}
	return writeFrame(w, e.b)
}

// ReadResponse reads and decodes one framed response from r for the given op.
func ReadResponse(r io.Reader, op Op) (*Response, error) {
	body, err := readFrame(r)
	if err != nil {
		return nil, err
	}
	p := parser{b: body}
	resp := &Response{Err: p.i32()}
	if resp.Err == 0 {
		switch op {
		case OpStat, OpOpen:
			resp.Mode = p.u32()
			resp.Size = p.i64()
			if op == OpOpen {
				resp.Handle = p.u64()
			}
		case OpPread:
			resp.Data = p.bytes()
		case OpReaddir:
			n := p.u32()
			resp.Names = make([]string, 0, n)
			resp.Types = make([]uint8, 0, n)
			for i := uint32(0); i < n && p.err == nil; i++ {
				resp.Names = append(resp.Names, p.str())
				resp.Types = append(resp.Types, p.u8())
			}
		case OpWriteFile, OpClose:
		}
	}
	if p.err != nil {
		return nil, p.err
	}
	return resp, nil
}

// Conn wraps a duplex byte stream with buffered framing for one RPC peer.
type Conn struct {
	rw io.ReadWriteCloser
	br *bufio.Reader
	bw *bufio.Writer
}

// NewConn wraps rw with buffered readers/writers.
func NewConn(rw io.ReadWriteCloser) *Conn {
	return &Conn{rw: rw, br: bufio.NewReader(rw), bw: bufio.NewWriter(rw)}
}

// SendRequest writes req and flushes.
func (c *Conn) SendRequest(req *Request) error {
	if err := WriteRequest(c.bw, req); err != nil {
		return err
	}
	return c.bw.Flush()
}

// ReadRequest reads one request from the peer.
func (c *Conn) ReadRequest() (*Request, error) { return ReadRequest(c.br) }

// SendResponse writes resp for op and flushes.
func (c *Conn) SendResponse(op Op, resp *Response) error {
	if err := WriteResponse(c.bw, op, resp); err != nil {
		return err
	}
	return c.bw.Flush()
}

// ReadResponse reads one response for op from the peer.
func (c *Conn) ReadResponse(op Op) (*Response, error) { return ReadResponse(c.br, op) }

// Close closes the underlying stream.
func (c *Conn) Close() error { return c.rw.Close() }
