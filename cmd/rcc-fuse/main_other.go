//go:build !linux

// rcc-fuse is Linux-only (it depends on FUSE). This stub keeps the package
// buildable on other platforms so `go build ./...` stays green everywhere.
package main

import "log"

func main() {
	log.Fatal("rcc-fuse is only supported on Linux")
}
