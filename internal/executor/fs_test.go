package executor

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hoveychen/remote-cc-adapter/internal/protocol"
)

func TestFSServiceReadSlice(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.dat")
	content := bytes.Repeat([]byte("ABCDEFGH"), 4096) // 32 KiB
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	fs := NewFSService(nil)

	// stat
	st := fs.Handle(&protocol.Request{Op: protocol.OpStat, Path: path})
	if st.Err != 0 || st.Size != int64(len(content)) || st.IsDir() {
		t.Fatalf("stat: err=%d size=%d dir=%v", st.Err, st.Size, st.IsDir())
	}

	// open
	op := fs.Handle(&protocol.Request{Op: protocol.OpOpen, Path: path, Flags: uint32(os.O_RDONLY)})
	if op.Err != 0 || op.Handle == 0 {
		t.Fatalf("open: err=%d handle=%d", op.Err, op.Handle)
	}

	// pread a slice from the middle — the whole file must not be transferred
	const off, n = 5000, 25
	pr := fs.Handle(&protocol.Request{Op: protocol.OpPread, Handle: op.Handle, Off: off, Len: n})
	if pr.Err != 0 {
		t.Fatalf("pread: err=%d", pr.Err)
	}
	if !bytes.Equal(pr.Data, content[off:off+n]) {
		t.Fatalf("pread slice mismatch: got %q want %q", pr.Data, content[off:off+n])
	}

	// close
	if c := fs.Handle(&protocol.Request{Op: protocol.OpClose, Handle: op.Handle}); c.Err != 0 {
		t.Fatalf("close: err=%d", c.Err)
	}
}

func TestFSServiceStatMissing(t *testing.T) {
	fs := NewFSService(nil)
	r := fs.Handle(&protocol.Request{Op: protocol.OpStat, Path: "/no/such/path/xyz"})
	if r.Err != -int32(syscall.ENOENT) {
		t.Fatalf("expected ENOENT, got %d", r.Err)
	}
}

func TestFSServiceWriteAndReaddir(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSService(nil)

	target := filepath.Join(dir, "out.txt")
	body := []byte("written via RPC")
	if w := fs.Handle(&protocol.Request{Op: protocol.OpWriteFile, Path: target, Data: body}); w.Err != 0 {
		t.Fatalf("writefile: err=%d", w.Err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("readback: err=%v got=%q", err, got)
	}

	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	rd := fs.Handle(&protocol.Request{Op: protocol.OpReaddir, Path: dir})
	if rd.Err != 0 || len(rd.Names) != 2 {
		t.Fatalf("readdir: err=%d names=%v", rd.Err, rd.Names)
	}
	// Directory entries must report DTDir so ripgrep recurses into them.
	byName := map[string]uint8{}
	for i, n := range rd.Names {
		byName[n] = rd.Types[i]
	}
	if byName["out.txt"] != protocol.DTReg {
		t.Errorf("out.txt type = %d, want DTReg", byName["out.txt"])
	}
	if byName["subdir"] != protocol.DTDir {
		t.Errorf("subdir type = %d, want DTDir", byName["subdir"])
	}
}

// The FUSE mount mutates remote files with real POSIX ops rather than the
// whole-file OpWriteFile the macOS interposer uses.
func TestFSServiceCreateWriteAndSetattr(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSService(nil)
	target := filepath.Join(dir, "new.txt")

	cr := fs.Handle(&protocol.Request{Op: protocol.OpCreate, Path: target,
		Flags: uint32(os.O_RDWR), Mode: 0o640})
	if cr.Err != 0 || cr.Handle == 0 {
		t.Fatalf("create: err=%d handle=%d", cr.Err, cr.Handle)
	}
	if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o640 {
		t.Fatalf("create mode: err=%v mode=%v", err, fi.Mode())
	}

	// Two disjoint positional writes; the hole between them reads back as zeros.
	if w := fs.Handle(&protocol.Request{Op: protocol.OpPwrite, Handle: cr.Handle, Off: 0, Data: []byte("head")}); w.Err != 0 || w.Size != 4 {
		t.Fatalf("pwrite head: err=%d n=%d", w.Err, w.Size)
	}
	if w := fs.Handle(&protocol.Request{Op: protocol.OpPwrite, Handle: cr.Handle, Off: 8, Data: []byte("tail")}); w.Err != 0 || w.Size != 4 {
		t.Fatalf("pwrite tail: err=%d n=%d", w.Err, w.Size)
	}
	if c := fs.Handle(&protocol.Request{Op: protocol.OpClose, Handle: cr.Handle}); c.Err != 0 {
		t.Fatalf("close: err=%d", c.Err)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, []byte("head\x00\x00\x00\x00tail")) {
		t.Fatalf("readback: err=%v got=%q", err, got)
	}

	// Setattr applies exactly the masked fields.
	sa := fs.Handle(&protocol.Request{Op: protocol.OpSetattr, Path: target,
		Mask: protocol.SetSize | protocol.SetMode, Size: 4, Mode: 0o600})
	if sa.Err != 0 {
		t.Fatalf("setattr: err=%d", sa.Err)
	}
	fi, err := os.Stat(target)
	if err != nil || fi.Size() != 4 || fi.Mode().Perm() != 0o600 {
		t.Fatalf("after setattr: err=%v size=%d mode=%v", err, fi.Size(), fi.Mode().Perm())
	}

	// Stat reports the mtime the FUSE layer needs to answer getattr.
	st := fs.Handle(&protocol.Request{Op: protocol.OpStat, Path: target})
	if st.Err != 0 || st.Mtime != fi.ModTime().UnixNano() {
		t.Fatalf("stat mtime: err=%d got=%d want=%d", st.Err, st.Mtime, fi.ModTime().UnixNano())
	}
}

func TestFSServiceMkdirRenameUnlink(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSService(nil)
	sub := filepath.Join(dir, "sub")

	if r := fs.Handle(&protocol.Request{Op: protocol.OpMkdir, Path: sub, Mode: 0o755}); r.Err != 0 {
		t.Fatalf("mkdir: err=%d", r.Err)
	}
	src := filepath.Join(sub, "a.txt")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "b.txt")
	if r := fs.Handle(&protocol.Request{Op: protocol.OpRename, Path: src, Path2: dst}); r.Err != 0 {
		t.Fatalf("rename: err=%d", r.Err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Fatalf("renamed file missing: %v", err)
	}

	// unlink and rmdir are distinct syscalls, and must report POSIX's distinct
	// errnos when applied to the wrong kind of entry.
	if r := fs.Handle(&protocol.Request{Op: protocol.OpUnlink, Path: sub}); r.Err != -int32(syscall.EISDIR) &&
		r.Err != -int32(syscall.EPERM) { // Linux returns EISDIR, macOS EPERM
		t.Fatalf("unlink(dir): err=%d, want EISDIR/EPERM", r.Err)
	}
	if r := fs.Handle(&protocol.Request{Op: protocol.OpUnlink, Path: dst, Flags: protocol.UnlinkRmdir}); r.Err != -int32(syscall.ENOTDIR) {
		t.Fatalf("rmdir(file): err=%d, want ENOTDIR", r.Err)
	}

	if r := fs.Handle(&protocol.Request{Op: protocol.OpUnlink, Path: sub, Flags: protocol.UnlinkRmdir}); r.Err != 0 {
		t.Fatalf("rmdir: err=%d", r.Err)
	}
	if r := fs.Handle(&protocol.Request{Op: protocol.OpUnlink, Path: dst}); r.Err != 0 {
		t.Fatalf("unlink: err=%d", r.Err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("file survived unlink: %v", err)
	}
}
