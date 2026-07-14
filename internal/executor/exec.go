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

	// Preserve argv[0]: run req.Path (the binary) with the full req.Argv as the
	// argument vector, so argv[0] reaches the child verbatim (claude enters
	// ripgrep mode only when argv[0]'s basename is "rg"). Fall back to Argv[0] as
	// the path when Path is unset (older proxy).
	binPath := req.Path
	if binPath == "" {
		binPath = req.Argv[0]
	}
	// PATH fallback: the client may reference a binary by an absolute path that
	// exists on the brain host but not here — e.g. codex "code mode" always runs
	// the client's login shell /bin/zsh, but a Linux executor keeps zsh at
	// /usr/bin/zsh. When the exact path is absent, resolve the basename via this
	// host's PATH so an equivalent binary is used. argv[0] is left untouched
	// below, so the child still sees the original path (and rg-mode detection,
	// which keys on argv[0]'s basename, is unaffected).
	if strings.HasPrefix(binPath, "/") {
		if _, statErr := os.Stat(binPath); statErr != nil {
			if resolved, lookErr := exec.LookPath(filepath.Base(binPath)); lookErr == nil && resolved != binPath {
				e.logf("[exec] path fallback: %s -> %s", binPath, resolved)
				binPath = resolved
			}
		}
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
