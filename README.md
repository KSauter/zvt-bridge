# zvt-bridge

Small Go service that lets a cash register (ECR) which only speaks **ZVT over a
serial port** use a payment terminal that is connected over **ZVT over TCP/IP**
(the usual port 20007).

```
 cash register software          zvt-bridge                      payment terminal
 (opens COM6, ZVT serial)   ┌──────────────────────┐          (Ingenico, CCV, …)
        │                   │  serial framing      │                 │
   COM6 ◄══ virtual pair ══► COM7 ◄── DLE/STX/CRC ─┼── raw APDUs ──► TCP :20007
        │                   │  ACK/NAK handling    │                 │
                            └──────────────────────┘
```

ZVT is the German ECR interface standard published by the
[Verband der Terminalhersteller e.V.](https://www.terminalhersteller.de/downloads.aspx).
The application layer (APDUs such as *Registration*, *Authorization*,
*Status Information*, *Completion*, the `80 00 00` acknowledgement) is identical on
both transports; only the transport layer differs. The bridge therefore passes
APDUs through untouched and only translates the transport layer:

| | Serial (RS232 / USB) | TCP |
|---|---|---|
| Framing | `DLE STX` … `DLE ETX` + CRC-16 (polynomial 0x8408 over APDU + ETX, low byte first), `DLE` inside the payload doubled | none, the APDU length field delimits messages |
| Transport acknowledgement | `ACK 0x06` / `NAK 0x15` per frame, retransmission on NAK | none |
| Typical parameters | 9600 baud, 8 data bits, no parity, 2 stop bits | one TCP connection, port 20007 |

The TCP connection to the terminal is opened on demand and reused; if the
terminal closes it, the next command reconnects. If the serial port disappears
(USB adapter unplugged) it is reopened every 5 seconds.

## Why

Some POS applications run in the browser and reach the terminal through the
**Web Serial API**. They can open a COM port but not a TCP socket. With a
virtual (or physical loop-back) COM port pair on the POS computer, the browser
talks to one end and zvt-bridge translates on the other end to the terminal's
LAN interface.

## Contents

| Path | Purpose |
|---|---|
| `cmd/zvt-bridge` | the bridge; on Windows also a native service (`install`, `remove`, `start`, `stop`, `status`) |
| `cmd/fakept` | terminal simulator (ZVT over TCP) for tests |
| `cmd/fakeecr` | cash register simulator (ZVT over serial) for tests |
| `internal/zvt` | framing, CRC, decoder state machine, APDU reader, bridge logic, tests |
| `windows/install.ps1`, `windows/uninstall.ps1` | Windows setup: virtual COM pair, service, log directory |
| `linux/zvt-bridge.service` | systemd unit |
| `test/e2e-linux.sh` | end-to-end test with a PTY pair (socat) and both simulators |

## Build

Requires Go 1.27 or newer (older `go` binaries download the toolchain
automatically via `GOTOOLCHAIN=auto`). No CGO.

```sh
make test            # vet (linux + windows), unit tests
make build           # dist/zvt-bridge, dist/fakept, dist/fakeecr (host OS)
make build-windows   # dist/*.exe (windows/amd64)
make e2e             # Linux end-to-end test, needs socat
make snapshot        # goreleaser: archives for windows/linux, amd64/arm64/arm
```

Release archives are produced with [goreleaser](https://goreleaser.com)
(`goreleaser check`, `goreleaser release --snapshot --clean --skip=publish`).

## Usage

```
zvt-bridge [run] -serial COM7 -terminal 192.168.10.50 [options]

  -serial       serial port towards the ECR, e.g. COM7 or /dev/ttyUSB0 (required)
  -terminal     terminal address host[:port], default port 20007 (required)
  -baud 9600    -stopbits 2    -parity none
  -ack-timeout 2s   time to wait for the ECR's ACK per attempt
  -retries 3        send attempts towards the ECR
  -dial-timeout 5s  connection timeout towards the terminal
  -log FILE         log file (5 MB, 3 rotations) in addition to the console
  -v                log every byte

zvt-bridge list        show serial ports
zvt-bridge version
```

Example log line (one per APDU, direction, name, length, hex):

```
ECR → PT   Authorization         15 B  06 01 0C 04 00 00 00 00 12 50 49 09 78 19 40
PT  → ECR  ACK (positive)         3 B  80 00 00
PT  → ECR  Status-Information    23 B  04 0F 14 27 00 04 00 00 00 00 12 50 49 09 78 0B … (+7)
```

## Windows setup

### 1. A COM port pair

The bridge needs two COM ports that are wired to each other: the POS software
opens one, the bridge the other. Three ways to get them:

1. **com0com** (free, GPL): kernel driver from 2012, only legacy cross-signed.
   It installs on many machines, but **Windows 11 with Memory Integrity (HVCI,
   "Core isolation") enabled rejects it** (device manager code 52). Microsoft has
   been turning Memory Integrity on automatically since October 2026. Either
   disable Memory Integrity by policy (`install.ps1 -DisableMemoryIntegrity`,
   a security trade-off) or use one of the other options.
2. **USB null-modem cable** (hardware, no extra driver): a cable with a USB
   plug on both ends and two USB-serial converters inside (e.g. FTDI
   `USB-NMC-2.5M`). Plug *both* ends into the POS computer; Windows creates two
   COM ports via its built-in/WHQL drivers. Equivalent: two USB-serial adapters
   plus a null-modem cable. Works with Memory Integrity enabled.
3. **Commercial virtual COM driver** (WHQL-signed, HVCI-compatible), e.g.
   Virtual Serial Port Driver (Electronic Team), Virtual Serial Port Tools
   (HHD), Virtual Serial Port Kit (FabulaTech). Create a pair, then run the
   installer with `-VirtualPorts none`.

### 2. Run the installer (as administrator)

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\install.ps1 -Terminal 192.168.10.50                               # com0com, pair COM6/COM7
.\install.ps1 -Terminal 192.168.10.50 -VirtualPorts none -PosPort COM11 -BridgePort COM12
```

The installer

- creates/renames the com0com pair (when `-VirtualPorts com0com`), checks for
  Memory Integrity first,
- copies `zvt-bridge.exe` to `%ProgramFiles%\zvt-bridge`,
- registers the Windows service **ZvtBridge** (automatic start before login,
  restart after 5/10/30 s on failure, event log source),
- starts the service and prints its status.

Logs: `%ProgramData%\zvt-bridge\zvt-bridge.log`. Service control:
`zvt-bridge.exe status | stop | start`, remove with `uninstall.ps1`.

### 3. Point the POS software at the pair

Select the POS end of the pair (`COM6` by default) as the terminal's serial
port in the cash register software, 9600 8N2. Browser-based POS software using
the Web Serial API needs permission for the port; Chrome/Edge administrators
usually grant it with the `SerialAllowAllPortsForUrls` policy.

### 4. Configure the terminal

The terminal must be set to ZVT over TCP/IP ("ECR via LAN", port 20007) and
be reachable from the POS computer. Check with `Test-NetConnection <ip> -Port 20007`.

## Linux

```sh
sudo install -m 755 dist/zvt-bridge /usr/local/bin/
sudo install -m 644 linux/zvt-bridge.service /etc/systemd/system/
sudo systemctl edit zvt-bridge      # adjust ExecStart: -serial /dev/ttyUSB0 -terminal <ip>
sudo systemctl enable --now zvt-bridge
```

A Raspberry Pi running the bridge (USB-serial adapter + null-modem cable to the
POS computer, or USB gadget mode) is another way to avoid any driver on the POS
computer; goreleaser builds `linux/arm64` and `linux/armv6|v7` binaries.

## Testing without hardware

```sh
make e2e
```

starts `fakept` on 127.0.0.1:20017, creates a PTY pair with `socat`, runs the
bridge on one end and `fakeecr` on the other. `fakeecr` performs Registration,
Authorization (12.50 EUR) and Status-Enquiry and exits 0 when all three ended
with a *Completion*. Both simulators also run on Windows against a real pair:

```powershell
.\fakept.exe -listen :20007
.\zvt-bridge.exe run -serial COM7 -terminal 127.0.0.1 -v
.\fakeecr.exe -serial COM6
```

## Limitations

- One terminal per bridge instance; connections are always initiated by the
  bridge (ECR → PT). Terminal-initiated connections (PT → ECR, port 20008) are
  not supported.
- Plain TCP, no TLS (as specified for ZVT).
- Serial timeouts are configurable but not the full T1–T4 timer set of the
  specification; defaults work with common terminals.

## License

MIT, see `LICENSE`.
