#!/usr/bin/env bash
# e2e-local.sh — drive the REAL claude CLI through rca on one host, proving that
# Read and Bash are redirected through the interceptor -> adapter -> executor
# path (design doc §4.1.1). Executor is co-located here (local Unix socket
# transport); cross-host runs use `rca serve` + --code instead.
#
# Requirements: macOS, an installed & logged-in `claude`, Go, a C compiler.
# Usage: scripts/e2e-local.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
CLAUDE="${CLAUDE:-$(command -v claude)}"
MODEL="${RCC_E2E_MODEL:-haiku}"
# Short dir so Unix socket paths fit the ~104-char sun_path limit. Resolve
# symlinks (macOS /tmp -> /private/tmp) so the routed paths claude opens match
# the canonical prefix the adapter uses.
WORK="$(cd "$(mktemp -d /tmp/rccE2E.XXXX)" && pwd -P)"
VFS="$WORK/vfs"
EXEC_SOCK="$WORK/exec.sock"
ADAPTER_SOCK="$WORK/adapter.sock"
EXEC_LOG="$WORK/executor.log"
# A benign marker value (NOT framed as a secret/credential, which makes claude
# refuse to echo it). Proves the file was served through the remote fs RPC.
MARKER="RCCVALUE-8842"
ROUTED_FILE="$VFS/routed-file.txt"

cleanup() {
  [[ -n "${EXEC_PID:-}" ]] && kill "$EXEC_PID" 2>/dev/null || true
  echo "--- executor log ($EXEC_LOG) ---"; cat "$EXEC_LOG" 2>/dev/null || true
}
trap cleanup EXIT

echo "== build =="
( cd "$REPO" && make >/dev/null )   # native first, then rca embedding it

echo "== stage routed file =="
mkdir -p "$VFS"
printf 'marker: %s\n' "$MARKER" > "$ROUTED_FILE"

echo "== start executor =="
"$REPO/bin/rca" serve --sock "$EXEC_SOCK" >"$EXEC_LOG" 2>&1 &
EXEC_PID=$!
for _ in $(seq 1 50); do [[ -S "$EXEC_SOCK" ]] && break; sleep 0.1; done

# Natural routing: claude runs with its working directory UNDER the remote
# prefix (--workdir "$VFS"). Files it reads and subprocesses it spawns then route
# remote by path/cwd automatically — no sentinel. claude's own credential/self
# spawns (security, the claude binary) stay local via the interceptor's
# local-binary allowlist, so auth still works.
# rca defaults: resign on, dylib from the embedded artifact, spawn proxy = rca
# itself. Only transport/routing/socket flags are explicit here.
run_claude() {
  local prompt="$1"
  "$REPO/bin/rca" \
    --sock "$EXEC_SOCK" \
    --adapter-sock "$ADAPTER_SOCK" \
    --remote-prefix "$VFS" \
    --workdir "$VFS" \
    "$CLAUDE" --model "$MODEL" --allowedTools Read Bash -p "$prompt"
}

echo "== TEST 1: Read a routed file =="
OUT1="$(run_claude "Read the file $ROUTED_FILE and reply with ONLY the value after 'marker:'." 2>/dev/null || true)"
echo "claude said: $OUT1"

echo "== TEST 2: Bash on the executor (natural cwd routing, no sentinel) =="
OUT2="$(run_claude "Run this exact bash command and reply with ONLY its output: echo REMOTE=\$RCC_EXECUTOR" 2>/dev/null || true)"
echo "claude said: $OUT2"

echo
echo "===== VERDICT ====="
PASS=1
# Match on the basename: paths in the log are symlink-canonical (e.g. macOS
# /tmp -> /private/tmp), so an exact $ROUTED_FILE match would spuriously miss.
if grep -qE "OPEN .*/$(basename "$ROUTED_FILE")" "$EXEC_LOG" && grep -q "PREAD" "$EXEC_LOG"; then
  echo "[PASS] Read routed through executor RPC (OPEN+PREAD in executor log)"
else
  echo "[FAIL] no executor RPC evidence for the routed Read"; PASS=0
fi
if echo "$OUT1" | grep -q "$MARKER"; then
  echo "[PASS] claude returned the marker via the redirected Read"
else
  echo "[WARN] marker not found in claude output (model phrasing/refusal?) — check log above"
fi
if echo "$OUT2" | grep -q "REMOTE=1"; then
  echo "[PASS] Bash ran on the executor (RCC_EXECUTOR=1 marker present)"
else
  echo "[FAIL] Bash did not show the remote-executor marker"; PASS=0
fi
echo "==================="
exit $(( PASS ? 0 : 1 ))
