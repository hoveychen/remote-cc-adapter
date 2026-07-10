#!/usr/bin/env bash
# supervisor-signal-test.sh — regression test for the supervisor's ptrace signal
# handling, independent of FUSE, rca, claude and the network.
#
#   docker run --rm -v "$PWD":/src -w /src golang:1.25 scripts/supervisor-signal-test.sh
#
# stoptheworld_probe models JavaScriptCore's signal-based stop-the-world GC: a
# collector suspends every mutator thread with SIGPWR (tgkill, si_code=SI_TKILL),
# waits for each to park in its handler, then resumes them, for many rounds. Bare,
# it finishes in milliseconds. Under rcc_seccomp the supervisor ptraces every
# thread and must propagate that signal handshake intact; if it loses or mishandles
# a suspend signal the collector waits forever. We bound the run and treat a
# timeout as the reproduced deadlock.
set -euo pipefail

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

echo "== build supervisor + probe =="
cc -O2 -Wall -Wextra -o "$WORK/sup" native/linux/rcc_seccomp.c
cc -O2 -Wall -Wextra -pthread -o "$WORK/probe" native/linux/stoptheworld_probe.c

echo "== sanity: probe runs clean WITHOUT the supervisor =="
BARE="$(timeout 20 "$WORK/probe" 2>&1 || echo "BARE_TIMEOUT_OR_FAIL")"
echo "bare: $BARE"

echo "== probe UNDER the supervisor (the actual test) =="
SUP_OUT="$WORK/sup.out"
set +e
timeout 30 "$WORK/sup" "$WORK/probe" >"$SUP_OUT" 2>&1
RC=$?
set -e
echo "supervised exit=$RC output: $(cat "$SUP_OUT")"

echo
echo "===== VERDICT ====="
PASS=1
if [[ "$BARE" != *STOPTHEWORLD_OK* ]]; then
  echo "[FAIL] probe did not even run clean bare — test is broken, not the supervisor"; PASS=0
fi
# timeout(1) exits 124 when it had to kill the child: that is the deadlock.
if [[ "$RC" == "124" ]]; then
  echo "[FAIL] supervisor DEADLOCKED the stop-the-world probe (timed out)"; PASS=0
elif [[ "$RC" == "0" ]] && grep -q STOPTHEWORLD_OK "$SUP_OUT"; then
  echo "[PASS] supervisor propagated the SIGPWR stop-the-world handshake"
else
  echo "[FAIL] supervisor run failed (exit=$RC), not a clean stop-the-world"; PASS=0
fi
echo "==================="
exit $(( PASS ? 0 : 1 ))
