//go:build !linux

package main

// rca _fuse and rca _nsrun are Linux-only (they depend on FUSE and on mount
// namespaces). These stubs keep the binary buildable on other platforms.

import (
	"fmt"
	"os"
)

func cmdFuse(args []string) int {
	fmt.Fprintln(os.Stderr, "rca: _fuse is only supported on Linux")
	return 1
}

func cmdNsRun(args []string) int {
	fmt.Fprintln(os.Stderr, "rca: _nsrun is only supported on Linux")
	return 1
}
