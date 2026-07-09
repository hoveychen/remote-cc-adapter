//go:build linux

package main

// rca _fuse — mounts the Linux lazy-slice FUSE filesystem (design doc §4.1.3 /
// §4.3). The seccomp supervisor injects fds opened under this mount; reads are
// served one slice at a time from the adapter. Run alongside the seccomp-wrapped
// target process.

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hoveychen/remote-cc-adapter/internal/linuxfuse"
	"github.com/hoveychen/remote-cc-adapter/internal/transport"
)

func cmdFuse(args []string) int {
	fs := flag.NewFlagSet("rca _fuse", flag.ExitOnError)
	mount := fs.String("mount", "", "FUSE mount point (required)")
	adapterSock := fs.String("adapter-sock", "", "adapter fs IO-RPC unix socket (required)")
	_ = fs.Parse(args)

	if *mount == "" || *adapterSock == "" {
		log.Print("rca _fuse: -mount and -adapter-sock are required")
		return 2
	}
	if err := os.MkdirAll(*mount, 0o755); err != nil {
		log.Printf("rca _fuse: mkdir mount: %v", err)
		return 1
	}

	client := linuxfuse.NewClient(transport.NewUnixDialer(*adapterSock))
	server, err := linuxfuse.Mount(*mount, client)
	if err != nil {
		log.Printf("rca _fuse: mount: %v", err)
		return 1
	}
	log.Printf("rca _fuse: mounted at %s (adapter %s)", *mount, *adapterSock)

	// Unmount cleanly on signal.
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		_ = server.Unmount()
	}()
	server.Wait()
	return 0
}
