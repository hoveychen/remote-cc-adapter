package main

// rca <command> — run mode. Wired in P3; this stub keeps the P1 skeleton
// buildable and the dispatch contract visible.

import (
	"fmt"
	"os"
)

func cmdRun(args []string) int {
	fmt.Fprintf(os.Stderr, "rca: run mode for %q is not wired yet (P3)\n", args[0])
	return 2
}
