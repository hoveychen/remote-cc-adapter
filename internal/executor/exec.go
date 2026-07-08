package executor

import (
	"bufio"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/hoveychen/remote-cc-adapter/internal/execproto"
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
	e.logf("[exec] spawn argv=%v cwd=%s", req.Argv, req.Cwd)

	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Cwd
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

	// Forward signals the proxy relays until the control stream closes.
	go func() {
		for {
			f, err := execproto.ReadFrame(br)
			if err != nil {
				return
			}
			if f.Tag == execproto.TagSignal && cmd.Process != nil {
				_ = cmd.Process.Signal(syscall.Signal(f.Signum()))
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
