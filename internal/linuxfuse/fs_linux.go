//go:build linux

// Package linuxfuse backs the Linux interceptor's injected file descriptors
// with a FUSE filesystem so routed reads are served one slice at a time from
// the adapter, instead of fetching the whole file into a memfd up front (design
// doc §4.1.3 / §4.3).
//
// Flow: the seccomp supervisor traps openat of a routed path P, opens
// <mount>/<hex(P)> on this FUSE mount, and injects that fd into the target.
// The kernel then routes the target's read/lseek on that fd to this filesystem,
// whose Read fetches exactly the requested [offset, len) slice from the adapter
// over the fs IO-RPC protocol (internal/protocol). Only files actually opened
// through the mount incur FUSE callbacks — unlike trapping read/lseek in seccomp
// process-wide, this does not tax the target's other file I/O.
package linuxfuse

import (
	"context"
	"encoding/hex"
	pathpkg "path"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/hoveychen/remote-cc-adapter/internal/protocol"
	"github.com/hoveychen/remote-cc-adapter/internal/transport"
)

// Client issues fs IO-RPC calls to the adapter over a serialized connection,
// reconnecting on error.
type Client struct {
	dialer transport.Dialer
	mu     sync.Mutex
	conn   *protocol.Conn
}

// NewClient builds a Client dialing the adapter via dialer (typically a
// unix-socket dialer to RCC_ADAPTER_SOCK).
func NewClient(dialer transport.Dialer) *Client { return &Client{dialer: dialer} }

func (c *Client) call(req *protocol.Request) *protocol.Response {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		stream, err := c.dialer.Dial(context.Background())
		if err != nil {
			return &protocol.Response{Err: -int32(syscall.EIO)}
		}
		// The FUSE daemon is the Linux equivalent of the macOS interpose dylib:
		// it connects to the adapter's interceptor-facing socket and speaks the
		// raw protocol (no stream-kind byte; that prefix is only for the
		// adapter<->executor transport).
		c.conn = protocol.NewConn(stream)
	}
	if err := c.conn.SendRequest(req); err != nil {
		c.reset()
		return &protocol.Response{Err: -int32(syscall.EIO)}
	}
	resp, err := c.conn.ReadResponse(req.Op)
	if err != nil {
		c.reset()
		return &protocol.Response{Err: -int32(syscall.EIO)}
	}
	return resp
}

func (c *Client) reset() {
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}
}

// Mount mounts the FUSE filesystem at dir. Callers Wait/Unmount via the server.
func Mount(dir string, client *Client) (*fuse.Server, error) {
	root := &rootNode{client: client}
	return fs.Mount(dir, root, &fs.Options{
		MountOptions: fuse.MountOptions{FsName: "rcc-vfs", Name: "rcc"},
	})
}

// EncodePath maps a real path to a FUSE entry name (hex avoids '/' in names).
func EncodePath(p string) string { return hex.EncodeToString([]byte(p)) }

func decodePath(name string) (string, bool) {
	b, err := hex.DecodeString(name)
	if err != nil {
		return "", false
	}
	return string(b), true
}

// rootNode resolves <hex(path)> entries to fileNodes backed by the adapter.
type rootNode struct {
	fs.Inode
	client *Client
}

var _ = fs.NodeLookuper((*rootNode)(nil))

func (r *rootNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	path, ok := decodePath(name)
	if !ok {
		return nil, syscall.ENOENT
	}
	return newNode(ctx, &r.Inode, r.client, path, out)
}

// newNode stats path and materialises the matching node. A routed path may be a
// directory — a remote cwd is the common case — so the node type must follow
// st_mode. Typing a directory S_IFREG makes the supervisor inject a regular-file
// fd for its openat, and the caller's getdents64 then fails ENOTDIR.
func newNode(ctx context.Context, parent *fs.Inode, c *Client, path string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	resp := c.call(&protocol.Request{Op: protocol.OpStat, Path: path})
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}
	if resp.IsDir() {
		out.Attr.Mode = fuse.S_IFDIR | 0o755
		child := &dirNode{client: c, path: path}
		return parent.NewInode(ctx, child, fs.StableAttr{Mode: syscall.S_IFDIR}), 0
	}
	out.Attr.Mode = fuse.S_IFREG | 0o644
	out.Attr.Size = uint64(resp.Size)
	child := &fileNode{client: c, path: path, size: resp.Size}
	return parent.NewInode(ctx, child, fs.StableAttr{Mode: syscall.S_IFREG}), 0
}

// dirNode is one routed directory; Readdir forwards to the executor's OpReaddir.
type dirNode struct {
	fs.Inode
	client *Client
	path   string
}

var (
	_ = fs.NodeGetattrer((*dirNode)(nil))
	_ = fs.NodeReaddirer((*dirNode)(nil))
	_ = fs.NodeLookuper((*dirNode)(nil))
)

func (d *dirNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFDIR | 0o755
	return 0
}

func (d *dirNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	resp := d.client.call(&protocol.Request{Op: protocol.OpReaddir, Path: d.path})
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}
	ents := make([]fuse.DirEntry, 0, len(resp.Names))
	for i, n := range resp.Names {
		var t uint8 = protocol.DTUnknown
		if i < len(resp.Types) {
			t = resp.Types[i]
		}
		ents = append(ents, fuse.DirEntry{Name: n, Mode: direntMode(t)})
	}
	return fs.NewListDirStream(ents), 0
}

func (d *dirNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return newNode(ctx, &d.Inode, d.client, pathpkg.Join(d.path, name), out)
}

// direntMode maps a POSIX d_type to the file-type bits fuse.DirEntry expects.
// Directories must be exact: ripgrep will not descend an entry typed DT_REG.
func direntMode(t uint8) uint32 {
	switch t {
	case protocol.DTDir:
		return syscall.S_IFDIR
	case protocol.DTLnk:
		return syscall.S_IFLNK
	case protocol.DTReg:
		return syscall.S_IFREG
	default:
		return 0
	}
}

// fileNode is one routed file; reads fetch slices lazily.
type fileNode struct {
	fs.Inode
	client *Client
	path   string
	size   int64
}

var (
	_ = fs.NodeGetattrer((*fileNode)(nil))
	_ = fs.NodeOpener((*fileNode)(nil))
	_ = fs.NodeReader((*fileNode)(nil))
)

func (f *fileNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = fuse.S_IFREG | 0o644
	out.Size = uint64(f.size)
	return 0
}

func (f *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	resp := f.client.call(&protocol.Request{Op: protocol.OpOpen, Flags: uint32(syscall.O_RDONLY), Path: f.path})
	if resp.Err != 0 {
		return nil, 0, syscall.Errno(-resp.Err)
	}
	return &fileHandle{client: f.client, handle: resp.Handle}, fuse.FOPEN_KEEP_CACHE, 0
}

func (f *fileNode) Read(ctx context.Context, fh fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	h, ok := fh.(*fileHandle)
	if !ok {
		return nil, syscall.EBADF
	}
	resp := f.client.call(&protocol.Request{Op: protocol.OpPread, Handle: h.handle, Off: off, Len: uint32(len(dest))})
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}
	return fuse.ReadResultData(resp.Data), 0
}

type fileHandle struct {
	client *Client
	handle uint64
}

var (
	_ = fs.FileReleaser((*fileHandle)(nil))
)

func (h *fileHandle) Release(ctx context.Context) syscall.Errno {
	h.client.call(&protocol.Request{Op: protocol.OpClose, Handle: h.handle})
	return 0
}
