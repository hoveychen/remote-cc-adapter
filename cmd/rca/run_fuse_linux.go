//go:build linux

package main

// Linux run mode puts the target inside a private mount namespace where each
// remote-routed directory is a FUSE mount of the executor's copy, at the same
// absolute path (see cmd/rca/nsrun_linux.go). wrapMountNamespace rewrites the
// already-built target command into `rca _nsrun ... -- <target argv>` so
// `rca <command>` stays a single command, matching the macOS DYLD path (which
// needs no FUSE).

import (
	"log"
	"os"
	"os/exec"
)

func wrapMountNamespace(cmd *exec.Cmd, adapterSock, workdir string, remotePrefixes []string, logger *log.Logger) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{"_nsrun", "-adapter-sock", adapterSock, "-workdir", workdir}
	for _, p := range remotePrefixes {
		args = append(args, "-mount", p)
	}
	args = append(args, "--")
	args = append(args, cmd.Args...)

	logger.Printf("routing %d remote prefix(es) through a private mount namespace", len(remotePrefixes))
	wrapped := exec.Command(self, args...)
	wrapped.Env = cmd.Env
	wrapped.Stdin, wrapped.Stdout, wrapped.Stderr = cmd.Stdin, cmd.Stdout, cmd.Stderr
	// The working directory is applied inside the namespace, after the mounts
	// exist; setting it here would resolve against the host's directory instead.
	return wrapped, nil
}
