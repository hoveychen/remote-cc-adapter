//go:build linux

// Package linuxfuse backs the Linux interceptor's view of remote-routed paths
// with a FUSE filesystem, so routed reads are served one slice at a time from
// the adapter instead of fetching whole files up front (design doc §4.1.3 /
// §4.3).
//
// Two mount shapes share the same nodes:
//
//   - MountRouted(dir, remote) makes <dir> *be* the remote directory <remote>.
//     Mounted at the routed path itself, inside the target's private mount
//     namespace, it gives every syscall — openat, stat, statx, getdents64,
//     getcwd — one consistent view. This is the shape run mode uses.
//
//   - Mount(dir) exposes a flat namespace of hex(path) entries for callers that
//     resolve a routed absolute path by hand.
//
// Reads fetch exactly the requested [offset, len) slice over the fs IO-RPC
// protocol (internal/protocol); writes go out as real POSIX mutations.
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

func mountOpts(name string, directMount bool) *fs.Options {
	return &fs.Options{MountOptions: fuse.MountOptions{
		FsName: "rcc-vfs",
		Name:   name,
		// A private mount namespace has no fusermount3 helper contract to honour
		// and may not even have /dev/fuse reachable through it; mount(2) directly.
		DirectMount: directMount,
	}}
}

// Mount mounts the hex-entry filesystem at dir. Callers Wait/Unmount via the
// server.
func Mount(dir string, client *Client) (*fuse.Server, error) {
	return fs.Mount(dir, &rootNode{client: client}, mountOpts("rcc", false))
}

// MountRouted mounts the remote directory `remote` at `dir`, so paths under dir
// resolve to the executor's filesystem. Pass dir == remote to make the routed
// absolute path mean the same thing to every syscall.
func MountRouted(dir, remote string, client *Client, directMount bool) (*fuse.Server, error) {
	return fs.Mount(dir, &dirNode{client: client, path: remote}, mountOpts("rcc-routed", directMount))
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

// fillAttr copies a stat response into a FUSE attribute block. Mode carries both
// the file-type bits and the permission bits exactly as the executor reported
// them, so a routed directory is never mistaken for a regular file.
func fillAttr(a *fuse.Attr, resp *protocol.Response) {
	a.Mode = resp.Mode
	a.Size = uint64(resp.Size)
	if resp.Mtime > 0 {
		sec, nsec := resp.Mtime/1e9, resp.Mtime%1e9
		a.Mtime, a.Mtimensec = uint64(sec), uint32(nsec)
		a.Atime, a.Atimensec = uint64(sec), uint32(nsec)
		a.Ctime, a.Ctimensec = uint64(sec), uint32(nsec)
	}
}

// rootNode resolves <hex(path)> entries to file/dir nodes backed by the adapter.
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
// st_mode. Typing a directory S_IFREG makes getdents64 on it fail ENOTDIR.
func newNode(ctx context.Context, parent *fs.Inode, c *Client, path string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	resp := c.call(&protocol.Request{Op: protocol.OpStat, Path: path})
	if resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}
	fillAttr(&out.Attr, resp)
	if resp.IsDir() {
		child := &dirNode{client: c, path: path}
		return parent.NewInode(ctx, child, fs.StableAttr{Mode: syscall.S_IFDIR}), 0
	}
	child := &fileNode{client: c, path: path}
	return parent.NewInode(ctx, child, fs.StableAttr{Mode: syscall.S_IFREG}), 0
}

// getattr refreshes one node's attributes straight from the executor. Nothing is
// cached in the node: a routed file's size changes under our feet whenever the
// remote side writes to it, and a stale size truncates the target's reads.
func getattr(c *Client, path string, out *fuse.AttrOut) syscall.Errno {
	resp := c.call(&protocol.Request{Op: protocol.OpStat, Path: path})
	if resp.Err != 0 {
		return syscall.Errno(-resp.Err)
	}
	fillAttr(&out.Attr, resp)
	return 0
}

// setattr applies the requested subset of mode/size/times, then reports the
// resulting attributes.
func setattr(c *Client, path string, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	req := &protocol.Request{Op: protocol.OpSetattr, Path: path}
	if m, ok := in.GetMode(); ok {
		req.Mask |= protocol.SetMode
		req.Mode = m
	}
	if sz, ok := in.GetSize(); ok {
		req.Mask |= protocol.SetSize
		req.Size = int64(sz)
	}
	if t, ok := in.GetATime(); ok {
		req.Mask |= protocol.SetAtime
		req.Atime = t.UnixNano()
	}
	if t, ok := in.GetMTime(); ok {
		req.Mask |= protocol.SetMtime
		req.Mtime = t.UnixNano()
	}
	if req.Mask != 0 {
		if resp := c.call(req); resp.Err != 0 {
			return syscall.Errno(-resp.Err)
		}
	}
	return getattr(c, path, out)
}

// dirNode is one routed directory. It is also the root node of a MountRouted
// filesystem, which is why the whole directory surface lives here.
type dirNode struct {
	fs.Inode
	client *Client
	path   string
}

var (
	_ = fs.NodeGetattrer((*dirNode)(nil))
	_ = fs.NodeSetattrer((*dirNode)(nil))
	_ = fs.NodeReaddirer((*dirNode)(nil))
	_ = fs.NodeLookuper((*dirNode)(nil))
	_ = fs.NodeCreater((*dirNode)(nil))
	_ = fs.NodeMkdirer((*dirNode)(nil))
	_ = fs.NodeUnlinker((*dirNode)(nil))
	_ = fs.NodeRmdirer((*dirNode)(nil))
	_ = fs.NodeRenamer((*dirNode)(nil))
)

func (d *dirNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	return getattr(d.client, d.path, out)
}

func (d *dirNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return setattr(d.client, d.path, in, out)
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

func (d *dirNode) Create(ctx context.Context, name string, flags, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	path := pathpkg.Join(d.path, name)
	resp := d.client.call(&protocol.Request{Op: protocol.OpCreate, Path: path, Flags: flags, Mode: mode})
	if resp.Err != 0 {
		return nil, nil, 0, syscall.Errno(-resp.Err)
	}
	fillAttr(&out.Attr, resp)
	child := &fileNode{client: d.client, path: path}
	inode := d.NewInode(ctx, child, fs.StableAttr{Mode: syscall.S_IFREG})
	return inode, &fileHandle{client: d.client, handle: resp.Handle}, 0, 0
}

func (d *dirNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	path := pathpkg.Join(d.path, name)
	if resp := d.client.call(&protocol.Request{Op: protocol.OpMkdir, Path: path, Mode: mode}); resp.Err != 0 {
		return nil, syscall.Errno(-resp.Err)
	}
	return newNode(ctx, &d.Inode, d.client, path, out)
}

func (d *dirNode) Unlink(ctx context.Context, name string) syscall.Errno {
	return d.remove(name, 0)
}

func (d *dirNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	return d.remove(name, protocol.UnlinkRmdir)
}

func (d *dirNode) remove(name string, flags uint32) syscall.Errno {
	resp := d.client.call(&protocol.Request{Op: protocol.OpUnlink, Path: pathpkg.Join(d.path, name), Flags: flags})
	return syscall.Errno(-resp.Err)
}

// Rename only moves within this filesystem. RENAME_EXCHANGE / RENAME_NOREPLACE
// have no OpRename equivalent, so they are refused rather than silently demoted
// to a plain rename that would clobber the destination.
func (d *dirNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	if flags != 0 {
		return syscall.EINVAL
	}
	np, ok := newParent.(*dirNode)
	if !ok {
		return syscall.EXDEV
	}
	resp := d.client.call(&protocol.Request{
		Op:    protocol.OpRename,
		Path:  pathpkg.Join(d.path, name),
		Path2: pathpkg.Join(np.path, newName),
	})
	return syscall.Errno(-resp.Err)
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

// fileNode is one routed file; reads fetch slices lazily, writes go straight out.
type fileNode struct {
	fs.Inode
	client *Client
	path   string
}

var (
	_ = fs.NodeGetattrer((*fileNode)(nil))
	_ = fs.NodeSetattrer((*fileNode)(nil))
	_ = fs.NodeOpener((*fileNode)(nil))
	_ = fs.NodeReader((*fileNode)(nil))
	_ = fs.NodeWriter((*fileNode)(nil))
	_ = fs.NodeFsyncer((*fileNode)(nil))
)

func (f *fileNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	return getattr(f.client, f.path, out)
}

func (f *fileNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	return setattr(f.client, f.path, in, out)
}

// Open forwards the caller's access mode. Hardcoding O_RDONLY here is what used
// to make every routed file read-only no matter how the target opened it.
// FOPEN_KEEP_CACHE is only safe for read-only handles: the page cache would
// otherwise serve stale bytes after a write lands on the executor.
func (f *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	resp := f.client.call(&protocol.Request{Op: protocol.OpOpen, Flags: flags, Path: f.path})
	if resp.Err != 0 {
		return nil, 0, syscall.Errno(-resp.Err)
	}
	var fuseFlags uint32
	if flags&uint32(syscall.O_ACCMODE) == uint32(syscall.O_RDONLY) {
		fuseFlags = fuse.FOPEN_KEEP_CACHE
	}
	return &fileHandle{client: f.client, handle: resp.Handle}, fuseFlags, 0
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

func (f *fileNode) Write(ctx context.Context, fh fs.FileHandle, data []byte, off int64) (uint32, syscall.Errno) {
	h, ok := fh.(*fileHandle)
	if !ok {
		return 0, syscall.EBADF
	}
	resp := f.client.call(&protocol.Request{Op: protocol.OpPwrite, Handle: h.handle, Off: off, Data: data})
	if resp.Err != 0 {
		return 0, syscall.Errno(-resp.Err)
	}
	return uint32(resp.Size), 0
}

// Fsync is a no-op: every Write is already a synchronous round trip to the
// executor, which issued a real pwrite(2) before answering.
func (f *fileNode) Fsync(ctx context.Context, fh fs.FileHandle, flags uint32) syscall.Errno {
	return 0
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
