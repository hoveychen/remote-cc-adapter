package protocol

import (
	"bytes"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	cases := []*Request{
		{Op: OpStat, Path: "/tmp/foo"},
		{Op: OpReaddir, Path: "/tmp/dir"},
		{Op: OpOpen, Flags: 0x0242, Path: "/work/file.txt"},
		{Op: OpPread, Handle: 42, Off: 5 << 20, Len: 4096},
		{Op: OpWriteFile, Path: "/work/out.txt", Data: []byte("hello world")},
		{Op: OpClose, Handle: 7},
		{Op: OpPwrite, Handle: 11, Off: 4096, Data: []byte("payload")},
		{Op: OpCreate, Path: "/work/new.txt", Flags: 0x241, Mode: 0o644},
		{Op: OpUnlink, Path: "/work/gone.txt"},
		{Op: OpUnlink, Path: "/work/gonedir", Flags: UnlinkRmdir},
		{Op: OpMkdir, Path: "/work/sub", Mode: 0o755},
		{Op: OpRename, Path: "/work/a", Path2: "/work/b"},
		{Op: OpSetattr, Path: "/work/f", Mask: SetMode | SetSize | SetAtime | SetMtime,
			Mode: 0o600, Size: 1234, Atime: 1_700_000_000_000_000_001, Mtime: 1_700_000_000_000_000_002},
		{Op: OpServerInfo},
	}
	for _, want := range cases {
		var buf bytes.Buffer
		if err := WriteRequest(&buf, want); err != nil {
			t.Fatalf("%s: write: %v", want.Op, err)
		}
		got, err := ReadRequest(&buf)
		if err != nil {
			t.Fatalf("%s: read: %v", want.Op, err)
		}
		if got.Op != want.Op || got.Path != want.Path || got.Path2 != want.Path2 ||
			got.Flags != want.Flags || got.Mode != want.Mode || got.Mask != want.Mask ||
			got.Handle != want.Handle || got.Off != want.Off || got.Len != want.Len ||
			got.Size != want.Size || got.Atime != want.Atime || got.Mtime != want.Mtime ||
			!bytes.Equal(got.Data, want.Data) {
			t.Errorf("%s: round-trip mismatch\n got=%+v\nwant=%+v", want.Op, got, want)
		}
		if buf.Len() != 0 {
			t.Errorf("%s: %d trailing bytes after decode", want.Op, buf.Len())
		}
	}
}

func TestResponseRoundTrip(t *testing.T) {
	cases := []struct {
		op   Op
		resp *Response
	}{
		{OpStat, &Response{Mode: 0o100644, Size: 12345, Mtime: 1_700_000_000_000_000_003}},
		{OpOpen, &Response{Mode: 0o40755, Size: 0, Handle: 99}},
		{OpCreate, &Response{Mode: 0o100644, Size: 0, Handle: 5, Mtime: 42}},
		{OpPread, &Response{Data: []byte("abcdef")}},
		{OpPwrite, &Response{Size: 7}},
		{OpReaddir, &Response{Names: []string{"a.txt", "b.txt", "sub"}, Types: []uint8{DTReg, DTReg, DTDir}}},
		{OpWriteFile, &Response{}},
		{OpClose, &Response{}},
		{OpUnlink, &Response{}},
		{OpMkdir, &Response{}},
		{OpRename, &Response{}},
		{OpSetattr, &Response{}},
		{OpStat, &Response{Err: -2}}, // ENOENT: no payload beyond errno
		{OpServerInfo, &Response{OS: "linux", Arch: "arm64"}},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := WriteResponse(&buf, c.op, c.resp); err != nil {
			t.Fatalf("%s: write: %v", c.op, err)
		}
		got, err := ReadResponse(&buf, c.op)
		if err != nil {
			t.Fatalf("%s: read: %v", c.op, err)
		}
		if got.Err != c.resp.Err || got.Mode != c.resp.Mode || got.Size != c.resp.Size ||
			got.Handle != c.resp.Handle || got.Mtime != c.resp.Mtime || !bytes.Equal(got.Data, c.resp.Data) ||
			got.OS != c.resp.OS || got.Arch != c.resp.Arch {
			t.Errorf("%s: mismatch\n got=%+v\nwant=%+v", c.op, got, c.resp)
		}
		if len(got.Names) != len(c.resp.Names) {
			t.Errorf("%s: names len %d != %d", c.op, len(got.Names), len(c.resp.Names))
		}
		for i := range c.resp.Types {
			if i < len(got.Types) && got.Types[i] != c.resp.Types[i] {
				t.Errorf("%s: type[%d]=%d want %d", c.op, i, got.Types[i], c.resp.Types[i])
			}
		}
		if buf.Len() != 0 {
			t.Errorf("%s: %d trailing bytes", c.op, buf.Len())
		}
	}
}

// A peer built before Mtime existed ends its STAT response after size. Decoding
// must succeed with Mtime zero rather than fail "short i64", so a new run client
// still talks to an old `rca serve`.
func TestResponseStatWithoutMtimeDecodes(t *testing.T) {
	var e builder
	e.i32(0)
	e.u32(0o100644)
	e.i64(12345)
	var buf bytes.Buffer
	if err := writeFrame(&buf, e.b); err != nil {
		t.Fatal(err)
	}
	got, err := ReadResponse(&buf, OpStat)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Err != 0 || got.Mode != 0o100644 || got.Size != 12345 || got.Mtime != 0 {
		t.Errorf("got=%+v", got)
	}
}

func TestIsDir(t *testing.T) {
	if (&Response{Mode: 0o40755}).IsDir() != true {
		t.Error("dir mode not detected")
	}
	if (&Response{Mode: 0o100644}).IsDir() != false {
		t.Error("regular file misdetected as dir")
	}
}
