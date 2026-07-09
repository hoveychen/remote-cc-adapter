//go:build linux

// Command rcc-fuse mounts the Linux lazy-slice FUSE filesystem (design doc
// §4.1.3 / §4.3). The seccomp supervisor injects fds opened under this mount;
// reads are served one slice at a time from the adapter. Run by the adapter on
// Linux alongside the seccomp-wrapped claude process.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hoveychen/remote-cc-adapter/internal/linuxfuse"
	"github.com/hoveychen/remote-cc-adapter/internal/transport"
)

func main() {
	mount := flag.String("mount", "", "FUSE mount point (required)")
	adapterSock := flag.String("adapter-sock", "", "adapter fs IO-RPC unix socket (required)")
	flag.Parse()

	if *mount == "" || *adapterSock == "" {
		log.Fatal("rcc-fuse: -mount and -adapter-sock are required")
	}
	if err := os.MkdirAll(*mount, 0o755); err != nil {
		log.Fatalf("rcc-fuse: mkdir mount: %v", err)
	}

	client := linuxfuse.NewClient(transport.NewUnixDialer(*adapterSock))
	server, err := linuxfuse.Mount(*mount, client)
	if err != nil {
		log.Fatalf("rcc-fuse: mount: %v", err)
	}
	log.Printf("rcc-fuse: mounted at %s (adapter %s)", *mount, *adapterSock)

	// Unmount cleanly on signal.
	sigc := make(chan os.Signal, 2)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigc
		_ = server.Unmount()
	}()
	server.Wait()
}
