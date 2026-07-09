#!/usr/bin/env bash
# e2e-subagent.sh — prove that a claude SUBAGENT (Task tool) inherits the
# interceptor injection: its own tool calls are redirected to the executor just
# like the parent's.
#
# Claude Code runs a subagent as a spawned child claude process. That child
# inherits DYLD_INSERT_LIBRARIES + the RCC_* environment, so it is injected and
# connects to the same adapter. We prove it by having the subagent (and only the
# subagent) Read a routed file: if the executor log shows that file opened, the
# child's Read was redirected — i.e. the subagent is intercepted.
#
# Requirements: macOS, installed & logged-in claude, Go, a C compiler.
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
CLAUDE="${CLAUDE:-$(command -v claude)}"
MODEL="${RCC_E2E_MODEL:-haiku}"
WORK="$(cd "$(mktemp -d /tmp/rccSUB.XXXX)" && pwd -P)"
VFS="$WORK/proj"
EXEC_SOCK="$WORK/exec.sock"
ADAPTER_SOCK="$WORK/adapter.sock"
EXEC_LOG="$WORK/executor.log"
DYLIB_LOG="$WORK/dylib.log"
MARKER="SUBAGENT-5591"
SUB_FILE="$VFS/subagent-only.txt"

EXEC_PID=""
cleanup() {
  [[ -n "$EXEC_PID" ]] && kill "$EXEC_PID" 2>/dev/null || true
}
trap cleanup EXIT

echo "== build =="
( cd "$REPO" && make >/dev/null )   # native first, then rca embedding it

echo "== stage routed file (only the subagent should read it) =="
mkdir -p "$VFS"
printf 'marker: %s\n' "$MARKER" > "$SUB_FILE"

echo "== start executor =="
"$REPO/bin/rca" serve --sock "$EXEC_SOCK" >"$EXEC_LOG" 2>&1 &
EXEC_PID=$!
for _ in $(seq 1 50); do [[ -S "$EXEC_SOCK" ]] && break; sleep 0.1; done

echo "== run claude, instruct it to delegate the Read to a subagent =="
PROMPT="Use the Task tool to launch a subagent. The subagent must Read the file ${SUB_FILE} and report the value after 'marker:'. Do NOT read the file yourself — delegate it to the subagent. Then report what the subagent found."
# rca defaults: resign on, dylib from the embedded artifact, spawn proxy = rca.
OUT="$(RCC_LOG="$DYLIB_LOG" "$REPO/bin/rca" \
  --sock "$EXEC_SOCK" --adapter-sock "$ADAPTER_SOCK" \
  --remote-prefix "$VFS" --workdir "$VFS" \
  "$CLAUDE" --model "$MODEL" --allowedTools Task Read \
  -p "$PROMPT" 2>>"$WORK/claude.err" || true)"
echo "claude said: $OUT"

echo
echo "===== EVIDENCE ====="
echo "-- claude self-spawns (child processes = subagents/helpers) --"
grep -E '^SPAWN' "$DYLIB_LOG" | grep -c 'rcc-claude-copy' | xargs echo "child claude spawns:"
echo "-- executor RPC for the subagent-only file --"
grep -E "\[fs\] (OPEN|PREAD).*subagent-only" "$EXEC_LOG" | head -5

echo
echo "===== VERDICT ====="
PASS=1
if grep -qE "\[fs\] OPEN .*subagent-only" "$EXEC_LOG"; then
  echo "[PASS] the subagent-only file was opened through the executor RPC"
  echo "       -> a child claude (subagent) inherited injection and its Read routed"
else
  echo "[FAIL] no executor RPC for the subagent-only file"; PASS=0
fi
if echo "$OUT" | grep -q "$MARKER"; then
  echo "[PASS] the marker propagated back up through the subagent to the parent"
else
  echo "[WARN] marker not in final output (subagent may not have run in headless -p)"
fi
echo "==================="
echo "logs: $WORK"
exit $(( PASS ? 0 : 1 ))
