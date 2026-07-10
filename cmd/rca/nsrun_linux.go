//go:build linux

package main

// rca _nsrun — run a command inside a private mount namespace in which every
// remote-routed directory is a FUSE mount of the executor's copy, mounted at the
// same absolute path it has on the remote.
//
// Mounting at the routed path is what gives the target one consistent view of
// its own working directory: openat, stat, statx, getdents64 and getcwd all go
// through the same filesystem. Doing it in a private namespace is what keeps
// that view from leaking — the mount shadows the run host's directory of the
// same name, and every other process on the box must keep seeing the real one.
//
// Two stages, because a process cannot unshare its own mount namespace after the
// Go runtime has started threads: the outer stage re-execs itself with -inner
// and Unshareflags=CLONE_NEWNS, so the namespace exists before main() runs. The
// inner stage mounts, spawns the target, and unmounts when it exits.

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fuse"

	"github.com/hoveychen/remote-cc-adapter/internal/linuxfuse"
	"github.com/hoveychen/remote-cc-adapter/internal/transport"
)

// From linux/capability.h and linux/prctl.h; the syscall package exports neither.
const (
	capSysAdmin          = 21
	prCapAmbient         = 47
	prCapAmbientClearAll = 4
)

// repeatedFlag collects a flag given more than once.
type repeatedFlag []string

func (r *repeatedFlag) String() string     { return fmt.Sprint(*r) }
func (r *repeatedFlag) Set(v string) error { *r = append(*r, v); return nil }

func cmdNsRun(args []string) int {
	fs := flag.NewFlagSet("rca _nsrun", flag.ExitOnError)
	adapterSock := fs.String("adapter-sock", "", "adapter fs IO-RPC unix socket (required)")
	workdir := fs.String("workdir", "", "working directory for the target command")
	inner := fs.Bool("inner", false, "internal: already inside the private mount namespace")
	var mounts repeatedFlag
	fs.Var(&mounts, "mount", "remote directory to mount at its own absolute path (repeatable)")
	_ = fs.Parse(args)

	target := fs.Args()
	if *adapterSock == "" || len(target) == 0 {
		log.Print("rca _nsrun: -adapter-sock and a command are required")
		return 2
	}
	if !*inner {
		return nsRunOuter(args)
	}
	return nsRunInner(*adapterSock, *workdir, mounts, target)
}

// nsRunOuter re-execs rca in a fresh mount namespace.
//
// unshare(CLONE_NEWNS) and mounting need CAP_SYS_ADMIN, which an unprivileged
// caller can only obtain inside a user namespace. Two details make that work
// without pretending to be root:
//
//   - The uid/gid mapping is the identity, so the target keeps running as the
//     same user. Mapping it to 0 instead — what `unshare -Ur` does — would make
//     claude believe it is root and refuse --dangerously-skip-permissions.
//   - Capabilities held by a non-root process are wiped by execve, so the
//     re-exec would land in -inner with an empty set (measured: CapEff=0, mount
//     then fails EPERM). AmbientCaps survives execve; the runtime raises it in
//     the child after the mappings are written and after the unshare.
//
// -inner drops the ambient capability again before it spawns the target.
func nsRunOuter(args []string) int {
	self, err := os.Executable()
	if err != nil {
		log.Printf("rca _nsrun: locate self: %v", err)
		return 1
	}
	cmd := exec.Command(self, append([]string{"_nsrun", "-inner"}, args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
	if uid := os.Geteuid(); uid != 0 {
		gid := os.Getegid()
		cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}}
		cmd.SysProcAttr.GidMappingsEnableSetgroups = false
		cmd.SysProcAttr.AmbientCaps = []uintptr{capSysAdmin}
	}
	if err := cmd.Start(); err != nil {
		log.Printf("rca _nsrun: enter mount namespace: %v", err)
		return 1
	}
	forwardSignals(cmd)
	return exitCode(cmd.Wait())
}

// nsRunInner mounts each routed directory, runs the target, and tears the mounts
// down. It never touches the mounts itself: this process serves them, so a
// blocking lookup under one of them would deadlock against its own FUSE loop.
func nsRunInner(adapterSock, workdir string, mounts []string, target []string) int {
	client := linuxfuse.NewClient(transport.NewUnixDialer(adapterSock))

	var servers []*fuse.Server
	var created []string
	unmountAll := func() {
		for _, s := range servers {
			_ = s.Unmount()
		}
		for _, d := range created {
			_ = os.Remove(d) // only succeeds while empty, which is what we want
		}
	}

	for _, dir := range mounts {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				log.Printf("rca _nsrun: mkdir %s: %v", dir, err)
				unmountAll()
				return 1
			}
			created = append(created, dir)
		}
		// fusermount3 is a setuid helper that reads the host's /etc/fuse.conf and
		// registers with the host's mount table; inside a private (possibly user)
		// namespace we already hold CAP_SYS_ADMIN, so mount(2) directly.
		srv, err := linuxfuse.MountRouted(dir, dir, client, true)
		if err != nil {
			log.Printf("rca _nsrun: mount %s: %v", dir, err)
			unmountAll()
			return 1
		}
		servers = append(servers, srv)
	}

	// Hold no working-directory reference into a mount we have to unmount later.
	if err := os.Chdir("/"); err != nil {
		log.Printf("rca _nsrun: chdir /: %v", err)
	}

	cmd := exec.Command(target[0], target[1:]...)
	cmd.Dir = workdir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := startWithoutAmbientCaps(cmd); err != nil {
		log.Printf("rca _nsrun: start %s: %v", target[0], err)
		unmountAll()
		return 1
	}
	forwardSignals(cmd)
	code := exitCode(cmd.Wait())
	unmountAll()
	return code
}

// startWithoutAmbientCaps spawns cmd with an empty ambient capability set, so
// the target does not inherit the CAP_SYS_ADMIN we needed to mount. Ambient
// capabilities are per-thread and the runtime clones from whichever thread calls
// Start, so the clear and the Start must happen on one pinned thread. Clearing
// the ambient set leaves this thread's own effective set intact, which is what
// the later Unmount still needs.
func startWithoutAmbientCaps(cmd *exec.Cmd) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prCapAmbient, prCapAmbientClearAll, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("clear ambient capabilities: %w", errno)
	}
	return cmd.Start()
}

func forwardSignals(cmd *exec.Cmd) {
	sigc := make(chan os.Signal, 4)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		for s := range sigc {
			if cmd.Process != nil {
				_ = cmd.Process.Signal(s)
			}
		}
	}()
}
