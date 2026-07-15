package executor

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoveychen/remote-adapter/internal/execproto"
	"github.com/hoveychen/remote-adapter/internal/transport"
)

// TestExecStreamsAndExitCode drives a subprocess through the executor's exec
// service over a real socket, checking stdout/stderr split and exit code
// fidelity (design doc §4.1.2).
func TestExecStreamsAndExitCode(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "rccx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "e.sock")

	ln, err := transport.ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go New(ln, nil).Serve()

	stream := dialExec(t, sock)
	defer stream.Close()

	if _, err := stream.Write([]byte{StreamKindExec}); err != nil {
		t.Fatal(err)
	}
	req := &execproto.SpawnRequest{
		Argv: []string{"/bin/sh", "-c", "echo out-line; echo err-line 1>&2; exit 3"},
		Cwd:  sockDir,
	}
	if err := execproto.WriteSpawnRequest(stream, req); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := int32(-1)
	br := bufio.NewReader(stream)
	for {
		f, err := execproto.ReadFrame(br)
		if err != nil {
			break
		}
		switch f.Tag {
		case execproto.TagStdout:
			stdout.Write(f.Data)
		case execproto.TagStderr:
			stderr.Write(f.Data)
		case execproto.TagExit:
			code = f.ExitCode()
		}
		if code >= 0 {
			break
		}
	}

	if code != 3 {
		t.Errorf("exit code = %d, want 3", code)
	}
	if got := stdout.String(); got != "out-line\n" {
		t.Errorf("stdout = %q, want %q", got, "out-line\n")
	}
	if got := stderr.String(); got != "err-line\n" {
		t.Errorf("stderr = %q, want %q", got, "err-line\n")
	}
}

// TestExecInjectsRemoteMarker verifies the executor tags every child with
// RCC_EXECUTOR=1 so remote execution is observable (used by scripts/e2e-local.sh).
func TestExecInjectsRemoteMarker(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "rccm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "e.sock")

	ln, err := transport.ListenUnix(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go New(ln, nil).Serve()

	stream := dialExec(t, sock)
	defer stream.Close()
	if _, err := stream.Write([]byte{StreamKindExec}); err != nil {
		t.Fatal(err)
	}
	req := &execproto.SpawnRequest{Argv: []string{"/bin/sh", "-c", "printf %s \"$RCC_EXECUTOR\""}, Cwd: sockDir}
	if err := execproto.WriteSpawnRequest(stream, req); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	br := bufio.NewReader(stream)
	for {
		f, err := execproto.ReadFrame(br)
		if err != nil {
			break
		}
		if f.Tag == execproto.TagStdout {
			out.Write(f.Data)
		}
		if f.Tag == execproto.TagExit {
			break
		}
	}
	if out.String() != "1" {
		t.Errorf("RCC_EXECUTOR in child = %q, want %q", out.String(), "1")
	}
}

func dialExec(t *testing.T, sock string) transport.Stream {
	t.Helper()
	d := transport.NewUnixDialer(sock)
	for i := 0; i < 50; i++ {
		s, err := d.Dial(t.Context())
		if err == nil {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dial exec socket failed")
	return nil
}

// TestResolveBinPath covers the cross-OS binary resolution order, especially the
// claude ripgrep case: claude re-execs its own host-OS binary with argv[0]=rg,
// so on a different-OS executor the requested path is absent and the copy's
// basename ("rcc-claude-copy") resolves to nothing — the executor must fall back
// to argv[0]'s basename ("rg"), then to a bundled ripgrep when the host has none.
func TestResolveBinPath(t *testing.T) {
	// fake stat: only paths in `present` exist.
	statWith := func(present map[string]bool) func(string) (os.FileInfo, error) {
		return func(p string) (os.FileInfo, error) {
			if present[p] {
				return nil, nil
			}
			return nil, os.ErrNotExist
		}
	}
	// fake lookPath: only basenames in `onPath` resolve, to /usr/bin/<name>.
	lookWith := func(onPath map[string]bool) func(string) (string, error) {
		return func(name string) (string, error) {
			if onPath[name] {
				return "/usr/bin/" + name, nil
			}
			return "", os.ErrNotExist
		}
	}
	claudeCopy := "/private/var/folders/x/rcc-claude-copy"

	cases := []struct {
		name       string
		reqPath    string
		argv       []string
		embeddedRg string
		present    map[string]bool
		onPath     map[string]bool
		want       string
	}{
		{
			name:    "present path used as-is",
			reqPath: "/bin/zsh", argv: []string{"/bin/zsh", "-c", "x"},
			present: map[string]bool{"/bin/zsh": true},
			want:    "/bin/zsh",
		},
		{
			name:    "absent path resolves by its own basename",
			reqPath: "/bin/zsh", argv: []string{"/bin/zsh", "-c", "x"},
			onPath: map[string]bool{"zsh": true},
			want:   "/usr/bin/zsh",
		},
		{
			name:    "claude rg: copy absent, host has rg -> native rg via argv0",
			reqPath: claudeCopy, argv: []string{"rg", "--files"},
			onPath: map[string]bool{"rg": true}, // rcc-claude-copy not on PATH
			want:   "/usr/bin/rg",
		},
		{
			name:    "claude rg: copy absent, no host rg -> bundled rg",
			reqPath: claudeCopy, argv: []string{"rg", "--files"},
			embeddedRg: "/cache/rca/rg",
			want:       "/cache/rca/rg",
		},
		{
			name:    "claude rg: no host rg and no bundle -> original (degrade)",
			reqPath: claudeCopy, argv: []string{"rg", "--files"},
			want: claudeCopy,
		},
		{
			name:    "bare name passes through for child PATH search",
			reqPath: "", argv: []string{"git", "status"},
			want: "git",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveBinPathWith(c.reqPath, c.argv, c.embeddedRg, statWith(c.present), lookWith(c.onPath))
			if got != c.want {
				t.Errorf("resolveBinPath = %q, want %q", got, c.want)
			}
		})
	}
}
