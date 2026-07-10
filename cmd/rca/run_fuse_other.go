//go:build !linux

package main

import (
	"log"
	"os/exec"
)

// wrapMountNamespace is a no-op off Linux: macOS uses DYLD interposition (no
// FUSE, no mount namespace), and other platforms have no run-mode interceptor.
func wrapMountNamespace(cmd *exec.Cmd, _, _ string, _ []string, _ *log.Logger) (*exec.Cmd, error) {
	return cmd, nil
}
