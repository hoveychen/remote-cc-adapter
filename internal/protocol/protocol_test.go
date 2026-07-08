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
		if got.Op != want.Op || got.Path != want.Path || got.Flags != want.Flags ||
			got.Handle != want.Handle || got.Off != want.Off || got.Len != want.Len ||
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
		{OpStat, &Response{Mode: 0o100644, Size: 12345}},
		{OpOpen, &Response{Mode: 0o40755, Size: 0, Handle: 99}},
		{OpPread, &Response{Data: []byte("abcdef")}},
		{OpReaddir, &Response{Names: []string{"a.txt", "b.txt", "sub"}, Types: []uint8{DTReg, DTReg, DTDir}}},
		{OpWriteFile, &Response{}},
		{OpClose, &Response{}},
		{OpStat, &Response{Err: -2}}, // ENOENT: no payload beyond errno
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
			got.Handle != c.resp.Handle || !bytes.Equal(got.Data, c.resp.Data) {
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

func TestIsDir(t *testing.T) {
	if (&Response{Mode: 0o40755}).IsDir() != true {
		t.Error("dir mode not detected")
	}
	if (&Response{Mode: 0o100644}).IsDir() != false {
		t.Error("regular file misdetected as dir")
	}
}
