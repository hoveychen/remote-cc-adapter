package adapter

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/hoveychen/remote-adapter/internal/executor"
	"github.com/hoveychen/remote-adapter/internal/protocol"
	"github.com/hoveychen/remote-adapter/internal/routing"
	"github.com/hoveychen/remote-adapter/internal/transport"
)

type testLogger struct{ t *testing.T }

func (l testLogger) Printf(f string, a ...any) { l.t.Logf(f, a...) }

// TestAdapterRoutesLocalAndRemote wires an adapter and an executor over real
// Unix sockets and drives fs ops through a synthetic interceptor connection,
// verifying that paths route to the right side.
func TestAdapterRoutesLocalAndRemote(t *testing.T) {
	// Unix socket paths must fit the ~104-char sun_path limit, so keep the
	// socket dir short (t.TempDir under /var/folders is too long on macOS).
	sockDir, err := os.MkdirTemp("/tmp", "rcc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	localDir := t.TempDir()
	remoteDir := t.TempDir()

	// A file that exists only on the "remote" side.
	remoteBody := bytes.Repeat([]byte("REMOTE-"), 1000)
	if err := os.WriteFile(filepath.Join(remoteDir, "secret.txt"), remoteBody, 0o644); err != nil {
		t.Fatal(err)
	}
	// A file that exists only on the "local" side.
	if err := os.WriteFile(filepath.Join(localDir, "local.txt"), []byte("local-only"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Executor serving the remote host's filesystem.
	execSock := filepath.Join(sockDir, "exec.sock")
	execLn, err := transport.ListenUnix(execSock)
	if err != nil {
		t.Fatal(err)
	}
	exe := executor.New(execLn, testLogger{t})
	go exe.Serve()
	defer execLn.Close()

	// Adapter: remote-allowlist routing sends only remoteDir to the executor.
	route := routing.New(routing.ModeRemoteAllowlist, []string{remoteDir}, nil)
	adapterSock := filepath.Join(sockDir, "adapter.sock")
	adapterLn, err := transport.ListenUnix(adapterSock)
	if err != nil {
		t.Fatal(err)
	}
	ad := New(adapterLn, transport.NewUnixDialer(execSock), route, testLogger{t})
	go ad.Serve()
	defer adapterLn.Close()

	// Synthetic interceptor connection to the adapter.
	conn := dialWithRetry(t, adapterSock)
	defer conn.Close()

	// 1. stat a remote-routed file: the adapter must see the remote copy.
	remotePath := filepath.Join(remoteDir, "secret.txt")
	must(t, conn.SendRequest(&protocol.Request{Op: protocol.OpStat, Path: remotePath}))
	st, err := conn.ReadResponse(protocol.OpStat)
	if err != nil || st.Err != 0 || st.Size != int64(len(remoteBody)) {
		t.Fatalf("remote stat: err=%v resp=%+v", err, st)
	}

	// 2. open + pread a slice of the remote file.
	must(t, conn.SendRequest(&protocol.Request{Op: protocol.OpOpen, Path: remotePath, Flags: uint32(os.O_RDONLY)}))
	op, err := conn.ReadResponse(protocol.OpOpen)
	if err != nil || op.Err != 0 {
		t.Fatalf("remote open: err=%v resp=%+v", err, op)
	}
	must(t, conn.SendRequest(&protocol.Request{Op: protocol.OpPread, Handle: op.Handle, Off: 100, Len: 7}))
	pr, err := conn.ReadResponse(protocol.OpPread)
	if err != nil || pr.Err != 0 || !bytes.Equal(pr.Data, remoteBody[100:107]) {
		t.Fatalf("remote pread: err=%v data=%q", err, pr.Data)
	}
	must(t, conn.SendRequest(&protocol.Request{Op: protocol.OpClose, Handle: op.Handle}))
	if _, err := conn.ReadResponse(protocol.OpClose); err != nil {
		t.Fatalf("remote close: %v", err)
	}

	// 3. stat a local-routed file: served on the brain host.
	localPath := filepath.Join(localDir, "local.txt")
	must(t, conn.SendRequest(&protocol.Request{Op: protocol.OpStat, Path: localPath}))
	ls, err := conn.ReadResponse(protocol.OpStat)
	if err != nil || ls.Err != 0 || ls.Size != int64(len("local-only")) {
		t.Fatalf("local stat: err=%v resp=%+v", err, ls)
	}

	// 4. write a file to a remote-routed path; it must land in remoteDir.
	outPath := filepath.Join(remoteDir, "written.txt")
	must(t, conn.SendRequest(&protocol.Request{Op: protocol.OpWriteFile, Path: outPath, Data: []byte("hi remote")}))
	if w, err := conn.ReadResponse(protocol.OpWriteFile); err != nil || w.Err != 0 {
		t.Fatalf("remote write: err=%v resp=%+v", err, w)
	}
	if got, _ := os.ReadFile(outPath); string(got) != "hi remote" {
		t.Fatalf("remote write not landed: %q", got)
	}

	// 5. create + pwrite on a remote-routed path: the FUSE mount's write path.
	newPath := filepath.Join(remoteDir, "created.txt")
	must(t, conn.SendRequest(&protocol.Request{Op: protocol.OpCreate, Path: newPath,
		Flags: uint32(os.O_RDWR), Mode: 0o644}))
	cr, err := conn.ReadResponse(protocol.OpCreate)
	if err != nil || cr.Err != 0 {
		t.Fatalf("remote create: err=%v resp=%+v", err, cr)
	}
	must(t, conn.SendRequest(&protocol.Request{Op: protocol.OpPwrite, Handle: cr.Handle, Off: 0, Data: []byte("pwritten")}))
	if pw, err := conn.ReadResponse(protocol.OpPwrite); err != nil || pw.Err != 0 || pw.Size != 8 {
		t.Fatalf("remote pwrite: err=%v resp=%+v", err, pw)
	}
	must(t, conn.SendRequest(&protocol.Request{Op: protocol.OpClose, Handle: cr.Handle}))
	if _, err := conn.ReadResponse(protocol.OpClose); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(newPath); string(got) != "pwritten" {
		t.Fatalf("remote pwrite not landed: %q", got)
	}

	// 6. a rename that would straddle the routing boundary is EXDEV, not a
	// silent half-move. Callers fall back to copy+unlink on EXDEV.
	must(t, conn.SendRequest(&protocol.Request{Op: protocol.OpRename, Path: newPath,
		Path2: filepath.Join(localDir, "moved.txt")}))
	rn, err := conn.ReadResponse(protocol.OpRename)
	if err != nil || rn.Err != -int32(syscall.EXDEV) {
		t.Fatalf("cross-boundary rename: err=%v resp=%+v, want EXDEV", err, rn)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("source vanished on a refused rename: %v", err)
	}
}

// TestQueryServerInfo verifies the adapter can read the executor's GOOS/GOARCH
// over a one-shot fs stream — the signal run mode uses to detect a cross-OS
// deployment (agent host != executor host) before launching the agent.
func TestQueryServerInfo(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "rcc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })

	execSock := filepath.Join(sockDir, "exec.sock")
	execLn, err := transport.ListenUnix(execSock)
	if err != nil {
		t.Fatal(err)
	}
	defer execLn.Close()
	go executor.New(execLn, testLogger{t}).Serve()

	// Retry the dial: the executor's Accept loop may not be running yet.
	var gotOS, gotArch string
	for i := 0; i < 50; i++ {
		gotOS, gotArch, err = QueryServerInfo(t.Context(), transport.NewUnixDialer(execSock))
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("QueryServerInfo: %v", err)
	}
	// The executor runs in this test process, so its OS/arch is ours.
	if gotOS != runtime.GOOS || gotArch != runtime.GOARCH {
		t.Fatalf("got os=%q arch=%q, want os=%q arch=%q", gotOS, gotArch, runtime.GOOS, runtime.GOARCH)
	}
}

func dialWithRetry(t *testing.T, sock string) *protocol.Conn {
	t.Helper()
	dialer := transport.NewUnixDialer(sock)
	for i := 0; i < 50; i++ {
		stream, err := dialer.Dial(t.Context())
		if err == nil {
			return protocol.NewConn(stream)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("could not dial adapter socket %s", sock)
	return nil
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
