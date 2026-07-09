//go:build !linux

package main

import "log"

// ensureFuseMount is a no-op off Linux: macOS uses DYLD interposition (no FUSE),
// and other platforms have no run-mode interceptor. Any caller-provided mount is
// returned unchanged.
func ensureFuseMount(existing, _ string, _ *log.Logger) (string, func(), error) {
	return existing, func() {}, nil
}
