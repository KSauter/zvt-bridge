#!/usr/bin/env bash
# End-to-end test on Linux:
#   fakeecr (cash register) ↔ PTY pair (socat) ↔ zvt-bridge ↔ TCP ↔ fakept (terminal)
# Usage: test/e2e-linux.sh   (binaries in dist/, see `make build`)
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN=${BIN:-$ROOT/dist}
PORT=${PORT:-20017}
T=$(mktemp -d)
PIDS=""
cleanup() { [ -n "$PIDS" ] && kill $PIDS 2>/dev/null || true; wait 2>/dev/null || true; rm -rf "$T"; }
trap cleanup EXIT
command -v socat >/dev/null || { echo "socat is missing (apt install socat)"; exit 2; }
for b in fakept fakeecr zvt-bridge; do [ -x "$BIN/$b" ] || { echo "$BIN/$b missing, run 'make build' first"; exit 2; }; done

"$BIN/fakept" -listen 127.0.0.1:$PORT >"$T/fakept.log" 2>&1 & PIDS="$!"
socat -d -d pty,raw,echo=0,link="$T/ecr" pty,raw,echo=0,link="$T/bridge" >"$T/socat.log" 2>&1 & PIDS="$PIDS $!"
for _ in $(seq 1 50); do [ -e "$T/ecr" ] && [ -e "$T/bridge" ] && break; sleep 0.1; done
[ -e "$T/ecr" ] || { echo "socat did not create a PTY pair:"; cat "$T/socat.log"; exit 2; }

"$BIN/zvt-bridge" run -serial "$T/bridge" -terminal 127.0.0.1:$PORT -ack-timeout 1s >"$T/bridge.log" 2>&1 & PIDS="$PIDS $!"
sleep 0.5

set +e
"$BIN/fakeecr" -serial "$T/ecr" -amount 1250 -timeout 5s 2>&1 | tee "$T/fakeecr.log"
rc=${PIPESTATUS[0]}
set -e
echo; echo "===== zvt-bridge ====="; cat "$T/bridge.log"
echo; echo "===== fakept ====="; cat "$T/fakept.log"
echo
if [ "$rc" -eq 0 ]; then echo "E2E OK"; else echo "E2E FAILED (rc=$rc)"; fi
exit "$rc"
