package executor

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/hoveychen/remote-adapter/internal/execproto"
)

// resolveBinPath picks the binary to exec for a routed spawn on this host.
//
// It prefers reqPath. When reqPath is an absolute path absent on this host it
// falls back, in order, to: the path's own basename on PATH (e.g. codex's
// /bin/zsh -> /usr/bin/zsh on a distro that keeps it elsewhere); argv[0]'s
// basename on PATH (claude re-execs its own host-OS binary with argv[0]=rg, so
// the useful identity is "rg", not the copy's basename); and a bundled ripgrep
// for an rg spawn when this host has no rg of its own. Returns reqPath (or
// argv[0] when reqPath is empty, for older proxies) when nothing better resolves.
func resolveBinPath(reqPath string, argv []string, embeddedRg string) string {
	return resolveBinPathWith(reqPath, argv, embeddedRg, os.Stat, exec.LookPath)
}

// resolveBinPathWith is resolveBinPath with the filesystem/PATH lookups injected
// so the resolution order is unit-testable independent of the test host.
func resolveBinPathWith(reqPath string, argv []string, embeddedRg string,
	stat func(string) (os.FileInfo, error), lookPath func(string) (string, error)) string {
	binPath := reqPath
	if binPath == "" && len(argv) > 0 {
		binPath = argv[0]
	}
	if !strings.HasPrefix(binPath, "/") {
		return binPath // a bare name; the child's own PATH search applies
	}
	if _, err := stat(binPath); err == nil {
		return binPath // present here, use as-is
	}
	if resolved, err := lookPath(filepath.Base(binPath)); err == nil && resolved != binPath {
		return resolved
	}
	var arg0Base string
	if len(argv) > 0 {
		arg0Base = filepath.Base(argv[0])
	}
	if arg0Base != "" {
		if resolved, err := lookPath(arg0Base); err == nil {
			return resolved
		}
	}
	if arg0Base == "rg" && embeddedRg != "" {
		return embeddedRg
	}
	return binPath
}

// serveExec runs one subprocess on the executor host on behalf of a connected
// spawn proxy. It reads the SpawnRequest header, launches the process, pumps
// stdout/stderr back as tagged frames, forwards signals the proxy relays, and
// finishes with an exit frame. This mirrors the POC exec_server.py
// (design doc §4.1 point 4 and §4.1.2 stderr split).
func (e *Executor) serveExec(stream io.ReadWriteCloser) {
	defer stream.Close()
	br := bufio.NewReader(stream)

	req, err := execproto.ReadSpawnRequest(br)
	if err != nil {
		e.logf("[exec] read spawn request: %v", err)
		return
	}
	if len(req.Argv) == 0 {
		e.logf("[exec] empty argv")
		_ = execproto.WriteExit(stream, 127)
		return
	}
	e.logf("[exec] spawn path=%s argv=%v cwd=%s", req.Path, req.Argv, req.Cwd)

	// Resolve the binary to exec. argv[0] is preserved verbatim in cmd.Args below
	// (claude enters ripgrep mode only when argv[0]'s basename is "rg"), so
	// rewriting only the path is safe.
	binPath := resolveBinPath(req.Path, req.Argv, e.embeddedRg)
	orig := req.Path
	if orig == "" && len(req.Argv) > 0 {
		orig = req.Argv[0]
	}
	if binPath != orig {
		e.logf("[exec] path fallback: %s -> %s", orig, binPath)
	}
	cmd := exec.Command(binPath)
	cmd.Args = req.Argv
	cmd.Dir = req.Cwd
	// Run the child in its own process group so a forwarded signal (KillBash)
	// reaches the whole tree. Otherwise killing e.g. /bin/sh orphans its children
	// (a `sleep`), which keep the stdout pipe open — the pumps never see EOF and
	// the exit frame is never sent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	base := req.Env
	if len(base) == 0 {
		base = os.Environ()
	}
	// The child runs natively on the executor, so strip the brain-side
	// interception environment: DYLD_INSERT_LIBRARIES (the macOS interpose dylib)
	// and the RCC_* wiring. Otherwise a routed subprocess would re-inject itself
	// and forward its own filesystem ops back to the adapter — a re-injection
	// loop / metadata storm. Then mark it as running remotely.
	cmd.Env = append(scrubInjectionEnv(base), "RCC_EXECUTOR=1")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = execproto.WriteExit(stream, 127)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = execproto.WriteExit(stream, 127)
		return
	}
	// A stdin pipe so the proxy can stream the child's stdin. codex "code mode"
	// runs a persistent shell that reads commands from stdin (`read line`); with
	// no stdin channel that shell blocks forever and the whole exec hangs.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = execproto.WriteExit(stream, 127)
		return
	}
	if err := cmd.Start(); err != nil {
		e.logf("[exec] start: %v", err)
		_ = execproto.WriteExit(stream, 127)
		return
	}

	// Serialize frame writes: stdout pump, stderr pump, and exit all share the
	// stream.
	var wmu sync.Mutex
	writeChunk := func(tag byte, data []byte) {
		wmu.Lock()
		_ = execproto.WriteChunk(stream, tag, data)
		wmu.Unlock()
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go pump(stdout, execproto.TagStdout, writeChunk, &wg)
	go pump(stderr, execproto.TagStderr, writeChunk, &wg)

	// Read control frames the proxy relays until the control stream closes:
	// signal forwarding and stdin chunks. Closing stdin on EOF (a zero-length
	// stdin frame) or on stream close lets a child blocked on `read` finish.
	go func() {
		defer stdin.Close()
		for {
			f, err := execproto.ReadFrame(br)
			if err != nil {
				return
			}
			switch f.Tag {
			case execproto.TagSignal:
				if cmd.Process != nil {
					// Signal the whole process group (negative pid) so children
					// die too; fall back to the leader if the group send fails.
					sig := syscall.Signal(f.Signum())
					if err := syscall.Kill(-cmd.Process.Pid, sig); err != nil {
						_ = cmd.Process.Signal(sig)
					}
				}
			case execproto.TagStdin:
				if len(f.Data) == 0 {
					_ = stdin.Close() // EOF: no more input for the child
				} else {
					_, _ = stdin.Write(f.Data)
				}
			}
		}
	}()

	wg.Wait()
	err = cmd.Wait()
	code := exitCode(err)
	wmu.Lock()
	_ = execproto.WriteExit(stream, int32(code))
	wmu.Unlock()
	e.logf("[exec] exit code=%d", code)
}

// scrubInjectionEnv drops the brain-side interception variables from a child's
// environment: DYLD_INSERT_LIBRARIES and everything RCC_* (RCC_EXECUTOR is
// re-added by the caller).
func scrubInjectionEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "DYLD_INSERT_LIBRARIES=") || strings.HasPrefix(kv, "RCC_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func pump(r io.Reader, tag byte, write func(byte, []byte), wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			write(tag, buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// exitCode extracts a process exit code from cmd.Wait's error.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 127
}
