#!/usr/bin/env bash
# e2e-local.sh — drive the REAL claude CLI through remote-cc-adapter on one host,
# proving that Read and Bash are redirected through the interceptor -> adapter ->
# executor path (design doc §4.1.1). Executor is co-located here (local Unix
# socket transport); true cross-host runs await the go-libp2p transport.
#
# Requirements: macOS, an installed & logged-in `claude`, Go, a C compiler.
# Usage: scripts/e2e-local.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
CLAUDE="${CLAUDE:-$(command -v claude)}"
MODEL="${RCC_E2E_MODEL:-haiku}"
# Short dir so Unix socket paths fit the ~104-char sun_path limit.
WORK="$(mktemp -d /tmp/rccE2E.XXXX)"
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
( cd "$REPO" && make go >/dev/null && make macos >/dev/null )

echo "== stage routed file =="
mkdir -p "$VFS"
printf 'marker: %s\n' "$MARKER" > "$ROUTED_FILE"

echo "== start executor =="
"$REPO/bin/rcc-executor" -sock "$EXEC_SOCK" >"$EXEC_LOG" 2>&1 &
EXEC_PID=$!
for _ in $(seq 1 50); do [[ -S "$EXEC_SOCK" ]] && break; sleep 0.1; done

run_claude() {
  local prompt="$1"
  "$REPO/bin/remote-cc-adapter" \
    --claude "$CLAUDE" \
    --resign \
    --dylib "$REPO/native/macos/rcc_interpose.dylib" \
    --spawn-proxy "$REPO/bin/rcc-spawn-proxy" \
    --executor-sock "$EXEC_SOCK" \
    --adapter-sock "$ADAPTER_SOCK" \
    --remote-prefix "$VFS" \
    -- --model "$MODEL" --allowedTools Read Bash -p "$prompt"
}

echo "== TEST 1: Read a routed file =="
OUT1="$(run_claude "Read the file $ROUTED_FILE and reply with ONLY the value after 'marker:'." 2>/dev/null || true)"
echo "claude said: $OUT1"

# The dylib's current opt-in trigger for routing a subprocess remote is the
# RCC_REMOTE_SENTINEL marker appearing in the command (design doc §4.1.1). We
# put it in the command so it routes; claude's own startup spawns (security for
# keychain, git status, ...) do NOT contain it and stay local, so auth works
# normally. A natural routing policy (by cwd, with a local-binary allowlist) is
# future work.
echo "== TEST 2: Bash on the executor =="
OUT2="$(run_claude "Run this exact bash command and reply with ONLY its output: echo RCC_REMOTE_MARK; echo REMOTE=\$RCC_EXECUTOR" 2>/dev/null || true)"
echo "claude said: $OUT2"

echo
echo "===== VERDICT ====="
PASS=1
if grep -q "OPEN $ROUTED_FILE" "$EXEC_LOG" && grep -q "PREAD" "$EXEC_LOG"; then
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
