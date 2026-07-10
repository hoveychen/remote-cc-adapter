package executor

import (
	"errors"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/hoveychen/remote-cc-adapter/internal/protocol"
)

// FSService serves filesystem IO-RPC requests against the executor host's real
// filesystem. Handles are opaque uint64 tokens the interceptor echoes back on
// pread/close; the executor maps them to real *os.File values.
type FSService struct {
	mu     sync.Mutex
	next   uint64
	open   map[uint64]*os.File
	logger Logger
}

// NewFSService builds a service. It is used both by the executor (serving
// remote-routed ops) and by the adapter (serving local-routed ops on the brain
// host).
func NewFSService(logger Logger) *FSService {
	return &FSService{next: 1, open: make(map[uint64]*os.File), logger: logger}
}

// Handle dispatches one request and returns its response. It never returns a
// Go error: filesystem failures are reported in Response.Err as negative POSIX
// errnos, matching what the native interceptor hands back to its caller.
func (s *FSService) Handle(req *protocol.Request) *protocol.Response {
	switch req.Op {
	case protocol.OpStat:
		return s.stat(req)
	case protocol.OpOpen:
		return s.openFile(req)
	case protocol.OpPread:
		return s.pread(req)
	case protocol.OpWriteFile:
		return s.writeFile(req)
	case protocol.OpClose:
		return s.close(req)
	case protocol.OpReaddir:
		return s.readdir(req)
	case protocol.OpPwrite:
		return s.pwrite(req)
	case protocol.OpCreate:
		return s.create(req)
	case protocol.OpUnlink:
		return s.unlink(req)
	case protocol.OpMkdir:
		return s.mkdir(req)
	case protocol.OpRename:
		return s.rename(req)
	case protocol.OpSetattr:
		return s.setattr(req)
	default:
		return &protocol.Response{Err: -int32(syscall.EINVAL)}
	}
}

func (s *FSService) stat(req *protocol.Request) *protocol.Response {
	fi, err := os.Stat(req.Path)
	if err != nil {
		return &protocol.Response{Err: toErrno(err)}
	}
	s.logf("STAT %s size=%d dir=%v", req.Path, fi.Size(), fi.IsDir())
	return &protocol.Response{Mode: fileMode(fi), Size: fi.Size(), Mtime: fi.ModTime().UnixNano()}
}

func (s *FSService) openFile(req *protocol.Request) *protocol.Response {
	return s.openWithMode(req, int(req.Flags), 0o644)
}

// create opens req.Path with an explicit permission mode. Unlike OpOpen it
// always implies O_CREAT, so the FUSE Create path never races an existing file.
func (s *FSService) create(req *protocol.Request) *protocol.Response {
	return s.openWithMode(req, int(req.Flags)|os.O_CREATE, os.FileMode(req.Mode).Perm())
}

func (s *FSService) openWithMode(req *protocol.Request, flags int, perm os.FileMode) *protocol.Response {
	f, err := os.OpenFile(req.Path, flags, perm)
	if err != nil {
		return &protocol.Response{Err: toErrno(err)}
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return &protocol.Response{Err: toErrno(err)}
	}
	s.mu.Lock()
	h := s.next
	s.next++
	s.open[h] = f
	s.mu.Unlock()
	s.logf("%s %s -> handle=%d size=%d", req.Op, req.Path, h, fi.Size())
	return &protocol.Response{Mode: fileMode(fi), Size: fi.Size(), Handle: h, Mtime: fi.ModTime().UnixNano()}
}

func (s *FSService) pwrite(req *protocol.Request) *protocol.Response {
	s.mu.Lock()
	f := s.open[req.Handle]
	s.mu.Unlock()
	if f == nil {
		return &protocol.Response{Err: -int32(syscall.EBADF)}
	}
	n, err := f.WriteAt(req.Data, req.Off)
	if err != nil {
		return &protocol.Response{Err: toErrno(err)}
	}
	s.logf("PWRITE handle=%d off=%d -> %d", req.Handle, req.Off, n)
	return &protocol.Response{Size: int64(n)}
}

// unlink removes a file, or a directory when Flags carries UnlinkRmdir. The two
// use distinct syscalls so the caller gets POSIX's distinct errnos (EISDIR /
// ENOTDIR) rather than os.Remove's unified behaviour.
func (s *FSService) unlink(req *protocol.Request) *protocol.Response {
	var err error
	if req.Flags&protocol.UnlinkRmdir != 0 {
		err = syscall.Rmdir(req.Path)
	} else {
		err = syscall.Unlink(req.Path)
	}
	if err != nil {
		return &protocol.Response{Err: toErrno(err)}
	}
	s.logf("UNLINK %s rmdir=%v", req.Path, req.Flags&protocol.UnlinkRmdir != 0)
	return &protocol.Response{}
}

func (s *FSService) mkdir(req *protocol.Request) *protocol.Response {
	if err := os.Mkdir(req.Path, os.FileMode(req.Mode).Perm()); err != nil {
		return &protocol.Response{Err: toErrno(err)}
	}
	s.logf("MKDIR %s", req.Path)
	return &protocol.Response{}
}

func (s *FSService) rename(req *protocol.Request) *protocol.Response {
	if err := os.Rename(req.Path, req.Path2); err != nil {
		return &protocol.Response{Err: toErrno(err)}
	}
	s.logf("RENAME %s -> %s", req.Path, req.Path2)
	return &protocol.Response{}
}

func (s *FSService) setattr(req *protocol.Request) *protocol.Response {
	if req.Mask&protocol.SetMode != 0 {
		if err := os.Chmod(req.Path, os.FileMode(req.Mode).Perm()); err != nil {
			return &protocol.Response{Err: toErrno(err)}
		}
	}
	if req.Mask&protocol.SetSize != 0 {
		if err := os.Truncate(req.Path, req.Size); err != nil {
			return &protocol.Response{Err: toErrno(err)}
		}
	}
	// Chtimes needs both stamps; a zero time leaves that stamp untouched.
	if req.Mask&(protocol.SetAtime|protocol.SetMtime) != 0 {
		var at, mt time.Time
		if req.Mask&protocol.SetAtime != 0 {
			at = time.Unix(0, req.Atime)
		}
		if req.Mask&protocol.SetMtime != 0 {
			mt = time.Unix(0, req.Mtime)
		}
		if err := os.Chtimes(req.Path, at, mt); err != nil {
			return &protocol.Response{Err: toErrno(err)}
		}
	}
	s.logf("SETATTR %s mask=%#x", req.Path, req.Mask)
	return &protocol.Response{}
}

func (s *FSService) pread(req *protocol.Request) *protocol.Response {
	s.mu.Lock()
	f := s.open[req.Handle]
	s.mu.Unlock()
	if f == nil {
		return &protocol.Response{Err: -int32(syscall.EBADF)}
	}
	buf := make([]byte, req.Len)
	n, err := f.ReadAt(buf, req.Off)
	if err != nil && !errors.Is(err, io.EOF) {
		return &protocol.Response{Err: toErrno(err)}
	}
	s.logf("PREAD handle=%d off=%d len=%d -> %d", req.Handle, req.Off, req.Len, n)
	return &protocol.Response{Data: buf[:n]}
}

func (s *FSService) writeFile(req *protocol.Request) *protocol.Response {
	if err := os.WriteFile(req.Path, req.Data, 0o644); err != nil {
		return &protocol.Response{Err: toErrno(err)}
	}
	s.logf("WRITEFILE %s bytes=%d", req.Path, len(req.Data))
	return &protocol.Response{}
}

func (s *FSService) close(req *protocol.Request) *protocol.Response {
	s.mu.Lock()
	f := s.open[req.Handle]
	delete(s.open, req.Handle)
	s.mu.Unlock()
	if f == nil {
		return &protocol.Response{Err: -int32(syscall.EBADF)}
	}
	if err := f.Close(); err != nil {
		return &protocol.Response{Err: toErrno(err)}
	}
	s.logf("CLOSE handle=%d", req.Handle)
	return &protocol.Response{}
}

func (s *FSService) readdir(req *protocol.Request) *protocol.Response {
	ents, err := os.ReadDir(req.Path)
	if err != nil {
		return &protocol.Response{Err: toErrno(err)}
	}
	names := make([]string, len(ents))
	types := make([]uint8, len(ents))
	for i, e := range ents {
		names[i] = e.Name()
		types[i] = direntType(e)
	}
	s.logf("READDIR %s -> %d entries", req.Path, len(names))
	return &protocol.Response{Names: names, Types: types}
}

// direntType maps a directory entry to its POSIX d_type. Directory type must be
// accurate: the interceptor forwards it to tools (ripgrep) that use it to decide
// whether to recurse.
func direntType(e os.DirEntry) uint8 {
	switch {
	case e.IsDir():
		return protocol.DTDir
	case e.Type()&os.ModeSymlink != 0:
		return protocol.DTLnk
	case e.Type().IsRegular():
		return protocol.DTReg
	default:
		return protocol.DTUnknown
	}
}

// CloseAll releases any handles left open when a stream ends.
func (s *FSService) CloseAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, f := range s.open {
		f.Close()
		delete(s.open, h)
	}
}

func (s *FSService) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf("[fs] "+format, args...)
	}
}

// fileMode maps an os.FileInfo to a POSIX st_mode value the interceptor expects.
func fileMode(fi os.FileInfo) uint32 {
	mode := uint32(fi.Mode().Perm())
	if fi.IsDir() {
		mode |= 0o040000 // S_IFDIR
	} else {
		mode |= 0o100000 // S_IFREG
	}
	return mode
}

// toErrno converts a Go filesystem error to a negative POSIX errno.
func toErrno(err error) int32 {
	if err == nil {
		return 0
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return -int32(errno)
	}
	switch {
	case os.IsNotExist(err):
		return -int32(syscall.ENOENT)
	case os.IsPermission(err):
		return -int32(syscall.EACCES)
	default:
		return -int32(syscall.EIO)
	}
}
