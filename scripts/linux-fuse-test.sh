#!/usr/bin/env bash
# linux-fuse-test.sh — verify Linux lazy slicing end to end. Runs INSIDE a
# privileged Linux container with /dev/fuse:
#
#   docker run --rm --privileged --device /dev/fuse \
#     -v "$PWD":/src -w /src golang:1.25 scripts/linux-fuse-test.sh
#
# Pipeline: rcc-executor (fs IO-RPC server, stands in for the adapter, backs a
# real 10 MiB file) <- rcc-fuse (FUSE mount) <- seccomp supervisor (redirects a
# routed openat to the FUSE file) <- a raw-syscall consumer that preads a 25-byte
# slice from 5 MiB in. Passes iff the consumer reads the right bytes AND far less
# than the whole file crossed the fs-RPC (lazy).
set -euo pipefail

command -v fusermount3 >/dev/null 2>&1 || { apt-get update -qq >/dev/null && apt-get install -y -qq fuse3 >/dev/null; }

WORK="$(mktemp -d)"
STORE="$WORK/work"; mkdir -p "$STORE"
MNT="$WORK/fuse"; mkdir -p "$MNT"
EXECSOCK="$WORK/exec.sock"   # remote executor
ADSOCK="$WORK/ad.sock"       # brain adapter (fs-RPC, raw protocol)
BIG="$STORE/bigfile.dat"
MARK="LAZY-SLICE-MARKER-9931-XY"  # 25 bytes

echo "== build =="
go build -o "$WORK/rcc-executor" ./cmd/rcc-executor
go build -o "$WORK/rcc-adapter" ./cmd/remote-cc-adapter
go build -o "$WORK/rcc-fuse" ./cmd/rcc-fuse
cc -O2 -Wall -Wextra -o "$WORK/sup" native/linux/rcc_seccomp.c

echo "== stage 10MiB file with a marker at offset 5MiB =="
dd if=/dev/zero of="$BIG" bs=1M count=10 status=none
printf '%s' "$MARK" | dd of="$BIG" bs=1 seek=$((5*1024*1024)) conv=notrunc status=none

echo "== start executor (remote side, backs the file) =="
"$WORK/rcc-executor" -sock "$EXECSOCK" >"$WORK/exec.log" 2>&1 &
EXEC_PID=$!
for _ in $(seq 1 50); do [[ -S "$EXECSOCK" ]] && break; sleep 0.1; done

echo "== start adapter (brain, routes STORE -> executor, serves fs-RPC) =="
"$WORK/rcc-adapter" --serve-fs-only --executor-sock "$EXECSOCK" --adapter-sock "$ADSOCK" --remote-prefix "$STORE" >"$WORK/adapter.log" 2>&1 &
AD_PID=$!
for _ in $(seq 1 50); do [[ -S "$ADSOCK" ]] && break; sleep 0.1; done

echo "== start rcc-fuse (connects to adapter, raw protocol) =="
"$WORK/rcc-fuse" -mount "$MNT" -adapter-sock "$ADSOCK" >"$WORK/fuse.log" 2>&1 &
FUSE_PID=$!
for _ in $(seq 1 50); do mountpoint -q "$MNT" 2>/dev/null && break; sleep 0.1; done

echo "== raw consumer: openat routed path, pread 25 bytes @ 5MiB =="
cat > "$WORK/consumer.c" <<'EOF'
#define _GNU_SOURCE
#include <fcntl.h>
#include <unistd.h>
#include <stdio.h>
#include <sys/syscall.h>
int main(int argc, char **argv) {
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

OUT="$(RCC_FUSE_MNT="$MNT" RCC_REMOTE_PREFIXES="$STORE" "$WORK/sup" "$WORK/consumer" "$BIG" 2>"$WORK/sup.log" || true)"
echo "consumer said: $OUT"

kill "$FUSE_PID" 2>/dev/null || true; fusermount3 -u "$MNT" 2>/dev/null || true
kill "$AD_PID" 2>/dev/null || true
kill "$EXEC_PID" 2>/dev/null || true

echo
echo "===== VERDICT ====="
PASS=1
if echo "$OUT" | grep -q "$MARK"; then
  echo "[PASS] consumer read the correct slice via the FUSE-backed injected fd"
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
echo "==================="
rm -rf "$WORK"
exit $(( PASS ? 0 : 1 ))
