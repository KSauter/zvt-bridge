#!/usr/bin/env bash
# End-to-end test on Linux through the kernel's USB/IP client:
#   fakeecr ↔ /dev/ttyACMn (cdc-acm) ↔ vhci-hcd ↔ USB/IP ↔ zvt-bridge -usb ↔ TCP ↔ fakept
# Needs root for modprobe, usbip attach/detach and the tty device (sudo without
# password, or run the script itself as root). Binaries in dist/ (make build).
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN=${BIN:-$ROOT/dist}
PORT=${PORT:-20017}
UPORT=${UPORT:-3241}
T=$(mktemp -d)
PIDS=""
SUDO=""; [ "$(id -u)" -ne 0 ] && SUDO="sudo -n"
cleanup() {
  for p in $($SUDO usbip port 2>/dev/null | sed -nE 's/^Port ([0-9]+): <Port in Use>.*/\1/p'); do $SUDO usbip detach -p "$p" >/dev/null 2>&1 || true; done
  [ -n "$PIDS" ] && kill $PIDS 2>/dev/null || true; wait 2>/dev/null || true
  rm -rf "$T"
}
trap cleanup EXIT
command -v usbip >/dev/null || { echo "usbip tool missing (apt install linux-tools-generic)"; exit 2; }
for b in fakept fakeecr zvt-bridge; do [ -x "$BIN/$b" ] || { echo "$BIN/$b missing, run 'make build' first"; exit 2; }; done
$SUDO modprobe vhci-hcd
before=$(ls /dev/ttyACM* 2>/dev/null || true)

"$BIN/fakept" -listen 127.0.0.1:$PORT >"$T/fakept.log" 2>&1 & PIDS="$!"
"$BIN/zvt-bridge" run -usb -usb-listen 127.0.0.1:$UPORT -usb-attach=false -terminal 127.0.0.1:$PORT -ack-timeout 1s >"$T/bridge.log" 2>&1 & PIDS="$PIDS $!"
sleep 0.5

echo "== attaching the emulated device via vhci-hcd"
$SUDO usbip --tcp-port $UPORT attach -r 127.0.0.1 -b 1-1
dev=""
for _ in $(seq 1 50); do
  after=$(ls /dev/ttyACM* 2>/dev/null || true)
  dev=$(comm -13 <(echo "$before" | sort) <(echo "$after" | sort) | head -1)
  [ -n "$dev" ] && break
  sleep 0.1
done
[ -n "$dev" ] || { echo "no new /dev/ttyACM* appeared"; dmesg 2>/dev/null | tail -20; cat "$T/bridge.log"; exit 1; }
sleep 0.5
echo "== device: $dev"
$SUDO usbip port | sed -n '1,8p'

set +e
$SUDO "$BIN/fakeecr" -serial "$dev" -amount 1250 -timeout 5s 2>&1 | tee "$T/fakeecr.log"
rc=${PIPESTATUS[0]}
set -e
echo; echo "===== zvt-bridge ====="; cat "$T/bridge.log"
echo; echo "===== fakept ====="; cat "$T/fakept.log"
echo
if [ "$rc" -eq 0 ]; then echo "E2E (USB/IP) OK"; else echo "E2E (USB/IP) FAILED (rc=$rc)"; fi
exit "$rc"
