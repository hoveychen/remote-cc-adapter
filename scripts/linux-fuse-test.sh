#!/usr/bin/env bash
# linux-fuse-test.sh — verify the Linux routed-directory mount end to end. Runs
# INSIDE a privileged Linux container with /dev/fuse:
#
#   docker run --rm --privileged --device /dev/fuse \
#     -v "$PWD":/src -w /src golang:1.25 scripts/linux-fuse-test.sh
#
# Topology mirrors production: the *run* host has an empty placeholder directory
# at the routed path ($STORE), while the *serve* host has the real project
# there. One container fakes both by giving the executor its own mount namespace
# in which $REAL is bind-mounted onto $STORE. Everything else — adapter, seccomp
# supervisor, consumers — sees $STORE as it really is on disk.
#
# Pipeline: rca serve (fs IO-RPC server, backs a real 10 MiB file) <- rca _nsrun
# (private mount namespace; mounts the remote directory at its own absolute path)
# <- seccomp supervisor <- raw-syscall consumers that read, enumerate,
# cross-check and mutate.
set -euo pipefail

command -v fusermount3 >/dev/null 2>&1 || { apt-get update -qq >/dev/null && apt-get install -y -qq fuse3 >/dev/null; }

WORK="$(mktemp -d)"
REAL="$WORK/real"; mkdir -p "$REAL"    # the serve host's real project directory
STORE="$WORK/store"; mkdir -p "$STORE" # the run host's placeholder at the routed path
EXECSOCK="$WORK/exec.sock"   # remote executor
ADSOCK="$WORK/ad.sock"       # brain adapter (fs-RPC, raw protocol)
BIG="$STORE/bigfile.dat"     # the path consumers ask for; content lives in $REAL
DIR="$STORE/listme"
MARK="LAZY-SLICE-MARKER-9931-XY"  # 25 bytes

echo "== build =="
go build -o "$WORK/rca" ./cmd/rca
cc -O2 -Wall -Wextra -o "$WORK/sup" native/linux/rcc_seccomp.c

echo "== stage 10MiB file with a marker at offset 5MiB (on the serve side) =="
dd if=/dev/zero of="$REAL/bigfile.dat" bs=1M count=10 status=none
printf '%s' "$MARK" | dd of="$REAL/bigfile.dat" bs=1 seek=$((5*1024*1024)) conv=notrunc status=none

echo "== stage a routed directory (openat O_DIRECTORY + getdents64 must work) =="
mkdir -p "$REAL/listme/nested"
: > "$REAL/listme/alpha.txt"; : > "$REAL/listme/beta.txt"

echo "== start executor in its own mount ns: \$REAL bind-mounted onto \$STORE =="
# The executor resolves routed paths against $STORE and finds the real content;
# no other process in this container does. That is exactly the production split:
# the run host's $STORE is empty, the serve host's $STORE is the project.
unshare -m -- sh -c "mount --bind '$REAL' '$STORE'; exec '$WORK/rca' serve --sock '$EXECSOCK'" >"$WORK/exec.log" 2>&1 &
EXEC_PID=$!
for _ in $(seq 1 50); do [[ -S "$EXECSOCK" ]] && break; sleep 0.1; done

echo "== start adapter (brain, routes STORE -> executor, serves fs-RPC) =="
"$WORK/rca" --serve-fs-only --sock "$EXECSOCK" --adapter-sock "$ADSOCK" --remote-prefix "$STORE" >"$WORK/adapter.log" 2>&1 &
AD_PID=$!
for _ in $(seq 1 50); do [[ -S "$ADSOCK" ]] && break; sleep 0.1; done

# nsrun <workdir> <cmd...> — run cmd the way `rca <command>` runs it: inside a
# private mount namespace where $STORE is a FUSE mount of the executor's copy of
# $STORE, at that same absolute path. Every syscall the target makes — openat,
# stat, statx, getdents64, getcwd — then resolves through one filesystem.
nsrun() {
  local wd="$1"; shift
  RCC_REMOTE_PREFIXES="$STORE" "$WORK/rca" _nsrun \
    -adapter-sock "$ADSOCK" -mount "$STORE" -workdir "$wd" -- "$@"
}

echo "== raw consumer: openat routed path, pread 25 bytes @ 5MiB =="
cat > "$WORK/consumer.c" <<'EOF'
#define _GNU_SOURCE
#include <fcntl.h>
#include <unistd.h>
#include <stdio.h>
#include <sys/syscall.h>
int main(int argc, char **argv) {
  (void)argc;
  int fd = syscall(SYS_openat, AT_FDCWD, argv[1], O_RDONLY, 0);
  if (fd < 0) { perror("openat"); return 1; }
  char b[64] = {0};
  ssize_t n = pread(fd, b, 25, 5L*1024*1024);
  if (n < 0) { perror("pread"); return 1; }
  printf("GOT[%zd]:%.*s\n", n, (int)n, b);
  return 0;
}
EOF
cc -O2 -o "$WORK/consumer" "$WORK/consumer.c"

OUT="$(nsrun "$WORK" "$WORK/sup" "$WORK/consumer" "$BIG" 2>"$WORK/sup.log" || true)"
echo "consumer said: $OUT"

echo "== raw consumer: openat routed DIRECTORY, getdents64 the entries =="
cat > "$WORK/dirconsumer.c" <<'EOF'
#define _GNU_SOURCE
#include <fcntl.h>
#include <unistd.h>
#include <stdio.h>
#include <string.h>
#include <sys/syscall.h>
struct linux_dirent64 {
  unsigned long long d_ino, d_off;
  unsigned short d_reclen;
  unsigned char d_type;
  char d_name[];
};
int main(int argc, char **argv) {
  (void)argc;
  int fd = syscall(SYS_openat, AT_FDCWD, argv[1], O_RDONLY | O_DIRECTORY, 0);
  if (fd < 0) { perror("openat"); return 1; }
  char buf[8192];
  for (;;) {
    long n = syscall(SYS_getdents64, fd, buf, sizeof buf);
    if (n < 0) { perror("getdents64"); return 1; }
    if (n == 0) break;
    for (long o = 0; o < n;) {
      struct linux_dirent64 *d = (struct linux_dirent64 *)(buf + o);
      printf("ENT:%s type=%d\n", d->d_name, d->d_type);
      o += d->d_reclen;
    }
  }
  return 0;
}
EOF
cc -O2 -o "$WORK/dirconsumer" "$WORK/dirconsumer.c"

DIROUT="$(nsrun "$WORK" "$WORK/sup" "$WORK/dirconsumer" "$DIR" 2>&1 || true)"
echo "dirconsumer said: $DIROUT"

echo "== raw consumer: openat and stat must agree on the same routed path =="
# The split-view bug this suite exists for: seccomp trapped openat but not
# stat/statx, so a process saw its routed cwd through two filesystems at once.
# openat landed on the FUSE-backed remote file; stat(2) fell through to the
# (empty) local directory and returned ENOENT. bun cross-checks the two and
# wedges at startup.
cat > "$WORK/splitview.c" <<'EOF'
#define _GNU_SOURCE
#include <fcntl.h>
#include <unistd.h>
#include <stdio.h>
#include <errno.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/statfs.h>
#include <sys/syscall.h>
int main(int argc, char **argv) {
  (void)argc;
  const char *p = argv[1];
  int fd = syscall(SYS_openat, AT_FDCWD, p, O_RDONLY, 0);
  printf("OPENAT:%s\n", fd < 0 ? strerror(errno) : "ok");
  struct stat sb;
  int sr = stat(p, &sb);
  printf("STAT:%s\n", sr < 0 ? strerror(errno) : "ok");
  if (fd < 0 || sr < 0) return 1;
  struct stat fb;
  if (fstat(fd, &fb) < 0) { perror("fstat"); return 1; }
  printf("SIZE_FSTAT:%lld SIZE_STAT:%lld\n", (long long)fb.st_size, (long long)sb.st_size);
  // st_dev pins it down: not merely "both are FUSE", but the very same mount.
  printf("DEV_FSTAT:%llu DEV_STAT:%llu\n",
         (unsigned long long)fb.st_dev, (unsigned long long)sb.st_dev);
  struct statfs ffs, pfs;
  if (fstatfs(fd, &ffs) < 0 || statfs(p, &pfs) < 0) { perror("statfs"); return 1; }
  printf("FSTYPE_FD:0x%lx FSTYPE_PATH:0x%lx\n",
         (unsigned long)ffs.f_type, (unsigned long)pfs.f_type);
  return 0;
}
EOF
cc -O2 -o "$WORK/splitview" "$WORK/splitview.c"

SPLITOUT="$(nsrun "$WORK" "$WORK/sup" "$WORK/splitview" "$BIG" 2>&1 || true)"
echo "splitview said: $SPLITOUT"

echo "== raw consumer: a routed cwd must enumerate and resolve relative names =="
# What claude actually does: chdir into the project, then openat(".") +
# getdents64 and stat relative names. Routing is a literal prefix match on the
# path string, so "." and "bigfile.dat" can never match a remote prefix — only a
# real mount at the routed path makes them resolve.
cat > "$WORK/cwdconsumer.c" <<'EOF'
#define _GNU_SOURCE
#include <fcntl.h>
#include <unistd.h>
#include <stdio.h>
#include <errno.h>
#include <string.h>
#include <sys/stat.h>
#include <sys/syscall.h>
struct linux_dirent64 {
  unsigned long long d_ino, d_off;
  unsigned short d_reclen;
  unsigned char d_type;
  char d_name[];
};
int main(int argc, char **argv) {
  (void)argc; (void)argv;
  char cwd[4096];
  if (!getcwd(cwd, sizeof cwd)) { perror("getcwd"); return 1; }
  printf("CWD:%s\n", cwd);
  int fd = syscall(SYS_openat, AT_FDCWD, ".", O_RDONLY | O_DIRECTORY, 0);
  if (fd < 0) { printf("OPENDOT:%s\n", strerror(errno)); return 1; }
  printf("OPENDOT:ok\n");
  char buf[8192];
  for (;;) {
    long n = syscall(SYS_getdents64, fd, buf, sizeof buf);
    if (n < 0) { perror("getdents64"); return 1; }
    if (n == 0) break;
    for (long o = 0; o < n;) {
      struct linux_dirent64 *d = (struct linux_dirent64 *)(buf + o);
      printf("CWDENT:%s\n", d->d_name);
      o += d->d_reclen;
    }
  }
  struct stat sb;
  printf("STATREL:%s\n", stat("bigfile.dat", &sb) < 0 ? strerror(errno) : "ok");
  return 0;
}
EOF
cc -O2 -o "$WORK/cwdconsumer" "$WORK/cwdconsumer.c"

CWDOUT="$(nsrun "$STORE" "$WORK/sup" "$WORK/cwdconsumer" 2>&1 || true)"
echo "cwdconsumer said: $CWDOUT"

echo "== mutate the routed directory: create, write, truncate, mkdir, rename, remove =="
# Exercises the FUSE write path (Create/Write/Setattr/Mkdir/Rename/Unlink/Rmdir)
# with ordinary shell tools. Everything must land on the *serve* side, i.e. in
# $REAL, which only the executor's mount namespace maps onto $STORE.
cat > "$WORK/writeprobe.sh" <<EOF
set -u
out=""
p() { out="\$out \$1"; }
( echo "hello remote" > "$STORE/newfile.txt" ) && p "create=ok" || p "create=FAIL"
[ "\$(cat "$REAL/newfile.txt" 2>/dev/null)" = "hello remote" ] && p "landed=ok" || p "landed=FAIL"
( printf 'xx' > "$STORE/newfile.txt" ) && p "trunc=ok" || p "trunc=FAIL"
[ "\$(cat "$REAL/newfile.txt" 2>/dev/null)" = "xx" ] && p "truncbody=ok" || p "truncbody=FAIL"
mkdir "$STORE/newdir" && p "mkdir=ok" || p "mkdir=FAIL"
mv "$STORE/newfile.txt" "$STORE/newdir/moved.txt" && p "rename=ok" || p "rename=FAIL"
[ -f "$REAL/newdir/moved.txt" ] && p "renamelanded=ok" || p "renamelanded=FAIL"
rm "$STORE/newdir/moved.txt" && p "unlink=ok" || p "unlink=FAIL"
rmdir "$STORE/newdir" && p "rmdir=ok" || p "rmdir=FAIL"
[ ! -e "$REAL/newdir" ] && p "removed=ok" || p "removed=FAIL"
echo "PROBES:\$out"
EOF
WROUT="$(nsrun "$WORK" sh "$WORK/writeprobe.sh" 2>&1 || true)"
echo "write probes: $WROUT"

echo "== subprocess routing: execve under a routed cwd is rewritten to the proxy =="
# The supervisor's only remaining seccomp job. Its BPF filter must send execve to
# SECCOMP_RET_TRACE and everything else to RET_ALLOW; an off-by-one in the jump
# offsets silently sends execve down the wrong branch and it runs local.
cat > "$WORK/fakeproxy" <<'EOF'
#!/bin/sh
echo "PROXY:$*"
EOF
chmod +x "$WORK/fakeproxy"
cat > "$WORK/execprobe.c" <<'EOF'
#define _GNU_SOURCE
#include <unistd.h>
#include <stdio.h>
#include <sys/syscall.h>
extern char **environ;
// Raw execve, the way bun issues it — glibc's wrapper is not what we must catch.
int main(void) {
  char *av[] = {"echo", "hi", 0};
  syscall(SYS_execve, "/bin/echo", av, environ);
  perror("execve");
  return 1;
}
EOF
cc -O2 -o "$WORK/execprobe" "$WORK/execprobe.c"

# RCC_CLAUDE_PATH keeps the supervisor's own exec of the probe local, exactly as
# it keeps claude local in production; only the probe's child execve routes.
export RCC_SPAWN_PROXY="$WORK/fakeproxy" RCC_CLAUDE_PATH="$WORK/execprobe" RCC_REMOTE_PREFIXES="$STORE"
REMOTE_EXEC="$(nsrun "$STORE" "$WORK/sup" "$WORK/execprobe" 2>&1 || true)"
echo "routed-cwd exec: $REMOTE_EXEC"
LOCAL_EXEC="$(cd "$WORK" && "$WORK/sup" "$WORK/execprobe" 2>&1 || true)"
echo "local-cwd exec: $LOCAL_EXEC"
unset RCC_SPAWN_PROXY RCC_CLAUDE_PATH

echo "== the routed mount must be invisible outside the namespace =="
# The mount shadows the run host's directory of the same name. Every other
# process on the box must keep seeing the real one, so it lives in a private
# mount namespace and must not propagate out of it.
ISO_READY="$WORK/iso.ready"; ISO_GO="$WORK/iso.go"
rm -f "$ISO_READY" "$ISO_GO" "$WORK/iso.inside"
cat > "$WORK/isoprobe.sh" <<EOF
set -u
{ grep -c rcc-vfs /proc/self/mountinfo || true; } > "$WORK/iso.inside"
ls "$STORE" > "$WORK/iso.inside.ls" 2>&1 || true
touch "$ISO_READY"
i=0; while [ ! -e "$ISO_GO" ] && [ \$i -lt 200 ]; do sleep 0.05; i=\$((i+1)); done
EOF
nsrun "$WORK" sh "$WORK/isoprobe.sh" >"$WORK/iso.log" 2>&1 &
ISO_BG=$!
for _ in $(seq 1 200); do [[ -e "$ISO_READY" ]] && break; sleep 0.05; done
HOST_MOUNTED=no; mountpoint -q "$STORE" 2>/dev/null && HOST_MOUNTED=yes
HOST_LS="$(ls -A "$STORE" 2>/dev/null | tr '\n' ' ')"
touch "$ISO_GO"; wait "$ISO_BG" 2>/dev/null || true
INSIDE_MOUNTS="$(cat "$WORK/iso.inside" 2>/dev/null || echo 0)"
INSIDE_LS="$(tr '\n' ' ' < "$WORK/iso.inside.ls" 2>/dev/null || true)"
echo "inside: mounts=$INSIDE_MOUNTS ls=[$INSIDE_LS]  outside: mounted=$HOST_MOUNTED ls=[$HOST_LS]"

kill "$AD_PID" 2>/dev/null || true
kill "$EXEC_PID" 2>/dev/null || true

echo
echo "===== VERDICT ====="
PASS=1
if echo "$OUT" | grep -q "$MARK"; then
  echo "[PASS] consumer read the correct slice through the routed mount"
else
  echo "[FAIL] consumer did not read the expected marker"; PASS=0
fi
# Sum bytes returned by PREAD in the fs-RPC server log; must be << 10 MiB.
FETCHED=$(grep -oE '\[fs\] PREAD .* -> [0-9]+' "$WORK/exec.log" | grep -oE '[0-9]+$' | awk '{s+=$1} END{print s+0}')
echo "fs-RPC bytes fetched: ${FETCHED} (file is $((10*1024*1024)))"
if [[ "$FETCHED" -gt 0 && "$FETCHED" -lt $((1024*1024)) ]]; then
  echo "[PASS] lazy: fetched ${FETCHED} bytes, far less than the 10MiB file"
else
  echo "[FAIL] not lazy (fetched ${FETCHED} bytes)"; PASS=0
fi
# A routed directory must open as a directory and enumerate. Regression guard for
# the FUSE layer typing every routed path S_IFREG, which made openat+getdents64
# on a routed cwd fail ENOTDIR and wedged bun's opendir() at startup.
if echo "$DIROUT" | grep -q "ENT:alpha.txt" && echo "$DIROUT" | grep -q "ENT:beta.txt"; then
  echo "[PASS] routed directory enumerated via getdents64"
else
  echo "[FAIL] routed directory did not enumerate: $DIROUT"; PASS=0
fi
if echo "$DIROUT" | grep -q "ENT:nested type=4"; then
  echo "[PASS] nested subdirectory reported d_type=DT_DIR"
else
  echo "[FAIL] nested subdirectory d_type wrong (ripgrep would not recurse)"; PASS=0
fi
# One consistent view: whatever openat resolves to, stat must resolve to as well
# — same mount, same size. Anything else is the split view that deadlocks bun on
# a remote-routed cwd.
SV_FD_FS="$(echo "$SPLITOUT" | sed -n 's/.*FSTYPE_FD:\([^ ]*\).*/\1/p')"
SV_PATH_FS="$(echo "$SPLITOUT" | sed -n 's/.*FSTYPE_PATH:\(.*\)/\1/p')"
SV_FSTAT="$(echo "$SPLITOUT" | sed -n 's/SIZE_FSTAT:\([0-9]*\).*/\1/p')"
SV_STAT="$(echo "$SPLITOUT" | sed -n 's/.*SIZE_STAT:\([0-9]*\).*/\1/p')"
SV_FDEV="$(echo "$SPLITOUT" | sed -n 's/DEV_FSTAT:\([0-9]*\).*/\1/p')"
SV_PDEV="$(echo "$SPLITOUT" | sed -n 's/.*DEV_STAT:\([0-9]*\).*/\1/p')"
if echo "$SPLITOUT" | grep -q '^OPENAT:ok' && echo "$SPLITOUT" | grep -q '^STAT:ok' &&
   [[ -n "$SV_FSTAT" && "$SV_FSTAT" == "$SV_STAT" && -n "$SV_FD_FS" && "$SV_FD_FS" == "$SV_PATH_FS" &&
      -n "$SV_FDEV" && "$SV_FDEV" == "$SV_PDEV" ]]; then
  echo "[PASS] openat and stat agree on the routed path (same mount, same size)"
else
  echo "[FAIL] split view on the routed path: $(echo "$SPLITOUT" | tr '\n' ' ')"; PASS=0
fi
# The routed cwd itself must behave like the remote directory: enumerate its
# entries and resolve relative names.
if echo "$CWDOUT" | grep -q "^CWD:$STORE$" && echo "$CWDOUT" | grep -q "^CWDENT:bigfile.dat$" &&
   echo "$CWDOUT" | grep -q "^STATREL:ok$"; then
  echo "[PASS] routed cwd enumerates and resolves relative names"
else
  echo "[FAIL] routed cwd is not the remote directory: $(echo "$CWDOUT" | tr '\n' ' ')"; PASS=0
fi
# Writes must reach the serve host, not a local shadow copy.
if echo "$WROUT" | grep -q '^PROBES:' && [[ "$WROUT" != *FAIL* ]]; then
  echo "[PASS] routed directory is writable end to end"
else
  echo "[FAIL] routed write path: $WROUT"; PASS=0
fi
# Subprocess routing: the supervisor's execve trap still fires, and still routes
# by cwd. Guards the seccomp filter's BPF jump offsets, which are relative to the
# *following* instruction and have been off by one before.
if [[ "$REMOTE_EXEC" == "PROXY:_spawn-proxy /bin/echo echo hi" ]]; then
  echo "[PASS] execve under a routed cwd was rewritten to the spawn proxy"
else
  echo "[FAIL] routed execve not rewritten: $REMOTE_EXEC"; PASS=0
fi
if [[ "$LOCAL_EXEC" == "hi" ]]; then
  echo "[PASS] execve under a local cwd ran unmodified"
else
  echo "[FAIL] local execve was disturbed: $LOCAL_EXEC"; PASS=0
fi
# Namespace isolation: mounted inside, absent outside, while the target runs.
if [[ "$INSIDE_MOUNTS" -ge 1 && "$INSIDE_LS" == *bigfile.dat* &&
      "$HOST_MOUNTED" == "no" && -z "${HOST_LS// /}" ]]; then
  echo "[PASS] routed mount is private to the target's namespace"
else
  echo "[FAIL] namespace leak: inside(mounts=$INSIDE_MOUNTS ls=$INSIDE_LS) outside(mounted=$HOST_MOUNTED ls=$HOST_LS)"; PASS=0
fi
echo "==================="
rm -rf "$WORK"
exit $(( PASS ? 0 : 1 ))
