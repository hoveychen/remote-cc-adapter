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
