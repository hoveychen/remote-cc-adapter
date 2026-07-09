//go:build linux

package main

// Linux run mode needs a rcc-fuse mount that the seccomp supervisor redirects
// routed opens to. ensureFuseMount auto-orchestrates it — spawning `rca _fuse`
// against the adapter fs-RPC socket and waiting for the mount to come up — so
// `rca <command>` is a single command on Linux, matching the macOS DYLD path
// (which needs no FUSE). An external harness can still pre-mount and pass
// --fuse-mount, in which case that mount is used as-is.

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func ensureFuseMount(existing, adapterSock string, logger *log.Logger) (string, func(), error) {
	noop := func() {}
	if existing != "" {
		return existing, noop, nil // caller/harness provided its own mount
	}
	exe, err := os.Executable()
	if err != nil {
		return "", noop, fmt.Errorf("locate rca binary: %w", err)
	}
	mnt, err := os.MkdirTemp("", "rcc-fuse-")
	if err != nil {
		return "", noop, err
	}
	cmd := exec.Command(exe, "_fuse", "-mount", mnt, "-adapter-sock", adapterSock)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(mnt)
		return "", noop, fmt.Errorf("spawn rca _fuse: %w", err)
	}

	waitDone := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waitDone) }()
	cleanup := func() {
		if cmd.Process != nil {
			_ = cmd.Process.Signal(syscall.SIGTERM) // _fuse unmounts on signal
		}
		select {
		case <-waitDone:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
		}
		// Belt-and-suspenders: force-unmount in case the helper died dirty.
		_ = exec.Command("fusermount3", "-u", mnt).Run()
		_ = exec.Command("fusermount", "-u", mnt).Run()
		_ = os.RemoveAll(mnt)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		if isMounted(mnt) {
			logger.Printf("rcc-fuse mounted at %s", mnt)
			return mnt, cleanup, nil
		}
		select {
		case <-waitDone:
			cleanup()
			return "", noop, fmt.Errorf("rca _fuse exited before the mount came up")
		default:
		}
		if time.Now().After(deadline) {
			cleanup()
			return "", noop, fmt.Errorf("rcc-fuse mount %s did not come up within 15s", mnt)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// isMounted reports whether path is a mount point per /proc/mounts.
func isMounted(path string) bool {
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if fields := strings.Fields(sc.Text()); len(fields) >= 2 && fields[1] == path {
			return true
		}
	}
	return false
}
