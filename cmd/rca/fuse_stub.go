//go:build !linux

package main

// rca _fuse is Linux-only (it depends on FUSE). This stub keeps the binary
// buildable on other platforms.

import (
	"fmt"
	"os"
)

func cmdFuse(args []string) int {
	fmt.Fprintln(os.Stderr, "rca: _fuse is only supported on Linux")
	return 1
}
