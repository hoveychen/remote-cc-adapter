#!/usr/bin/env bash
# e2e-codex.sh — drive the REAL codex CLI (openai/codex, "chatgpt work") through
# rca on one host, proving codex's shell/file tools are redirected through the
# interceptor -> adapter -> executor path, exactly as e2e-local.sh does for
# claude. Same POC-proven routing (ModeRemoteAllowlist): codex runs with its
# working root UNDER the remote prefix, so its shell commands and the files they
# open route remote by cwd/path automatically, while codex's own ~/.codex config
# and credentials stay local via the interceptor's local-binary allowlist.
#
# codex differs from claude in two ways that matter here:
#   1. codex has no separate "Read" tool — it reads files by running shell
#      commands (e.g. `cat file`), so BOTH tests below are shell routing.
#   2. codex ships its own sandbox (seatbelt/landlock). That must be OFF here —
#      the adapter+executor IS the external sandbox. We pass
#      --dangerously-bypass-approvals-and-sandbox, whose own help says it is
#      "intended solely for running in environments that are externally
#      sandboxed" — precisely this scenario.
#
# Requirements: macOS, an installed & logged-in `codex`, Go, a C compiler.
# Usage: scripts/e2e-codex.sh
set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
CODEX="${CODEX:-$(command -v codex)}"
# Optional: pin a model with RCC_E2E_CODEX_MODEL; empty uses codex's config default.
MODEL="${RCC_E2E_CODEX_MODEL:-}"
# Short dir so Unix socket paths fit the ~104-char sun_path limit. Resolve
# symlinks (macOS /tmp -> /private/tmp) so the routed paths codex opens match
# the canonical prefix the adapter uses.
WORK="$(cd "$(mktemp -d /tmp/rccCodexE2E.XXXX)" && pwd -P)"
VFS="$WORK/vfs"
EXEC_SOCK="$WORK/exec.sock"
ADAPTER_SOCK="$WORK/adapter.sock"
EXEC_LOG="$WORK/executor.log"
# A benign marker (NOT framed as a secret) proving the file was served through
# the remote fs RPC.
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

# Natural routing: codex runs with its working root UNDER the remote prefix
# (-C "$VFS" and rca --workdir "$VFS"). The shell commands it runs and the files
# they open then route remote by path/cwd automatically — no sentinel. codex's
# own credential reads (~/.codex) and self-spawns stay local via the
# interceptor's local-binary allowlist, so auth still works.
run_codex() {
  local prompt="$1" last="$2"
  local model_args=()
  [[ -n "$MODEL" ]] && model_args=(-m "$MODEL")
  "$REPO/bin/rca" \
    --sock "$EXEC_SOCK" \
    --adapter-sock "$ADAPTER_SOCK" \
    --remote-prefix "$VFS" \
    --workdir "$VFS" \
    "$CODEX" exec \
      --dangerously-bypass-approvals-and-sandbox \
      --skip-git-repo-check \
      --color never \
      -C "$VFS" \
      "${model_args[@]}" \
      -o "$last" \
      "$prompt"
}

echo "== TEST 1: read a routed file via codex's shell =="
run_codex "Run the shell command: cat routed-file.txt — then reply with ONLY the value after 'marker:'." \
  "$WORK/last1.txt" >/dev/null 2>&1 || true
OUT1="$(cat "$WORK/last1.txt" 2>/dev/null || true)"
echo "codex said: $OUT1"

echo "== TEST 2: shell on the executor (natural cwd routing, no sentinel) =="
run_codex "Run this exact shell command and reply with ONLY its output: echo REMOTE=\$RCC_EXECUTOR" \
  "$WORK/last2.txt" >/dev/null 2>&1 || true
OUT2="$(cat "$WORK/last2.txt" 2>/dev/null || true)"
echo "codex said: $OUT2"

echo
echo "===== VERDICT ====="
PASS=1
# Match on the basename: paths in the log are symlink-canonical (e.g. macOS
# /tmp -> /private/tmp), so an exact $ROUTED_FILE match would spuriously miss.
if grep -qE "OPEN .*/$(basename "$ROUTED_FILE")" "$EXEC_LOG" && grep -q "PREAD" "$EXEC_LOG"; then
  echo "[PASS] file Read routed through executor RPC (OPEN+PREAD in executor log)"
else
  echo "[FAIL] no executor RPC evidence for the routed Read"; PASS=0
fi
if echo "$OUT1" | grep -q "$MARKER"; then
  echo "[PASS] codex returned the marker via the redirected Read"
else
  echo "[WARN] marker not found in codex output (model phrasing/refusal?) — check log above"
fi
if echo "$OUT2" | grep -q "REMOTE=1"; then
  echo "[PASS] shell ran on the executor (RCC_EXECUTOR=1 marker present)"
else
  echo "[FAIL] shell did not show the remote-executor marker"; PASS=0
fi
echo "==================="
exit $(( PASS ? 0 : 1 ))
