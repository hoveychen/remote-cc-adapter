// Command rcc-spawn-proxy stands in for a subprocess that the native
// interceptor decided to run remotely. The interceptor rewrites a
// posix_spawn/exec of the real target into an exec of this proxy; the proxy
// connects to the executor, forwards argv + cwd + env, streams stdout/stderr
// back through its own inherited pipes, forwards SIGINT/SIGTERM, and exits with
// the remote child's exit code. It is the Go port of the POC's remote_run.py
// (design doc §4.1 point 4).
//
// Invocation (by the interceptor):
//
//	rcc-spawn-proxy <exec-path> <argv0> [argv1...]
//
// <exec-path> is the binary to run on the executor; the remaining arguments are
// the child's full argument vector including argv[0], which is preserved so
// binaries that switch behaviour on argv[0] (claude's embedded ripgrep keys off
// argv[0] basename "rg") work when routed. The executor socket is read from
// RCC_EXECUTOR_SOCK.
package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/hoveychen/remote-cc-adapter/internal/execproto"
	"github.com/hoveychen/remote-cc-adapter/internal/executor"
	"github.com/hoveychen/remote-cc-adapter/internal/transport"
)

func main() {
	os.Exit(run())
}

func run() int {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "rcc-spawn-proxy: usage: rcc-spawn-proxy <exec-path> <argv0> [argv1...]")
		return 127
	}
	sock := os.Getenv("RCC_EXECUTOR_SOCK")
	if sock == "" {
		fmt.Fprintln(os.Stderr, "rcc-spawn-proxy: RCC_EXECUTOR_SOCK not set")
		return 127
	}

	cwd, _ := os.Getwd()
	stream, err := transport.NewUnixDialer(sock).Dial(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "rcc-spawn-proxy: dial executor: %v\n", err)
		return 127
	}
	defer stream.Close()

	// Identify this as an exec stream, then send the spawn request.
	if _, err := stream.Write([]byte{executor.StreamKindExec}); err != nil {
		fmt.Fprintf(os.Stderr, "rcc-spawn-proxy: write kind: %v\n", err)
		return 127
	}
	req := &execproto.SpawnRequest{Path: os.Args[1], Argv: os.Args[2:], Cwd: cwd, Env: os.Environ()}
	if err := execproto.WriteSpawnRequest(stream, req); err != nil {
		fmt.Fprintf(os.Stderr, "rcc-spawn-proxy: send request: %v\n", err)
		return 127
	}

	// Forward SIGINT/SIGTERM to the remote child.
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for s := range sigc {
			if sig, ok := s.(syscall.Signal); ok {
				_ = execproto.WriteSignal(stream, int32(sig))
			}
		}
	}()

	// Pump tagged frames until the exit frame arrives.
	br := bufio.NewReader(stream)
	for {
		f, err := execproto.ReadFrame(br)
		if err != nil {
			if err == io.EOF {
				return 0
			}
			fmt.Fprintf(os.Stderr, "rcc-spawn-proxy: read frame: %v\n", err)
			return 127
		}
		switch f.Tag {
		case execproto.TagStdout:
			os.Stdout.Write(f.Data)
		case execproto.TagStderr:
			os.Stderr.Write(f.Data)
		case execproto.TagExit:
			return int(f.ExitCode()) & 0xff
		}
	}
}
