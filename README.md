# zvt-bridge

Small Go service that lets a cash register (ECR) which only speaks **ZVT over a
serial port** use a payment terminal that is connected over **ZVT over TCP/IP**
(the usual port 20007).

```
 cash register software                zvt-bridge service                    payment terminal
 (opens COMn, ZVT serial)                                                     (Ingenico, CCV, …)
        │                    ┌───────────────────────────────────┐                  │
   COMn ◄── usbser.sys ──► ─┤ emulated USB serial device (USB/IP) ├── raw APDUs ──► TCP :20007
        │   (Windows)        │ ZVT framing, CRC, ACK/NAK          │                  │
                             └───────────────────────────────────┘
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
terminal closes it, the next command reconnects.

## Two ways to reach the cash register

**`-usb` (recommended on Windows, no driver of our own).** The bridge emulates
a USB CDC-ACM ("USB serial") device and serves it over the
[USB/IP protocol](https://www.kernel.org/doc/html/latest/usb/usbip_protocol.html)
on the loopback interface. A USB/IP host controller plugs it in:

- on Windows the virtual host controller of
  [usbip-win2](https://github.com/vadimgrn/usbip-win2) (BSD-2-Clause, drivers
  **signed by Microsoft**, works with Secure Boot and Memory Integrity);
- on Linux the kernel's `vhci-hcd`.

Windows then binds its inbox `usbccgp.sys` and `usbser.sys` and creates
"USB Serial Device (COMn)"; Linux creates `/dev/ttyACMn`. No third-party
kernel driver, no test certificate, no COM port pair. Windows ties the COM
number to the device's serial number, so it stays the same across restarts.

**`-serial COMx`.** The bridge opens a real or virtual serial port. Use this
with a COM port pair (commercial virtual COM driver, com0com where Memory
Integrity allows it) or with a USB null-modem cable, or on a Raspberry Pi
wired to the POS computer.

## Why

Some POS applications run in the browser and reach the terminal through the
**Web Serial API**. They can open a COM port but not a TCP socket. The bridge
gives them a COM port that leads to the terminal's LAN interface.

## Contents

| Path | Purpose |
|---|---|
| `cmd/zvt-bridge` | the bridge; on Windows also a native service (`install`, `remove`, `start`, `stop`, `status`) |
| `cmd/fakept` | terminal simulator (ZVT over TCP) for tests |
| `cmd/fakeecr` | cash register simulator (ZVT over serial) for tests |
| `internal/zvt` | framing, CRC, decoder state machine, APDU reader, bridge logic |
| `internal/usbip` | USB/IP server and the emulated CDC-ACM device |
| `windows/install.ps1`, `windows/uninstall.ps1` | Windows setup: usbip-win2 (or a COM pair), service, log directory |
| `linux/zvt-bridge.service` | systemd unit |
| `test/e2e-linux.sh` | end-to-end test with a PTY pair (socat) |
| `test/e2e-usbip-linux.sh` | end-to-end test through the kernel's USB/IP client (`vhci-hcd`, needs root) |

## Build

Requires Go 1.27 or newer (older `go` binaries download the toolchain
automatically via `GOTOOLCHAIN=auto`). No CGO.

```sh
make test            # vet (linux + windows), unit tests
make build           # dist/zvt-bridge, dist/fakept, dist/fakeecr (host OS)
make build-windows   # dist/*.exe (windows/amd64)
make e2e             # Linux end-to-end test over a PTY pair, needs socat
make e2e-usbip       # Linux end-to-end test over USB/IP, needs usbip + root
make snapshot        # goreleaser: archives for windows/linux, amd64/arm64/arm
```

Release archives are produced with [goreleaser](https://goreleaser.com)
(`goreleaser check`, `goreleaser release --snapshot --clean --skip=publish`).

## Usage

```
zvt-bridge [run] -terminal 192.168.10.50 (-usb | -serial COM7) [options]

  -terminal     terminal address host[:port], default port 20007 (required)
  -usb          emulate a USB serial device over USB/IP
    -usb-listen 127.0.0.1:3240   USB/IP server address
    -usb-busid 1-1               bus id the host controller imports
    -usb-serial ZVT-BRIDGE-1     USB serial number (keep fixed: the COM number follows it)
    -usb-vid 1209 -usb-pid 0001  USB ids (pid.codes vendor id and test product id)
    -usb-attach=true             Windows: plug in via usbip.exe; -usbip-exe sets its path
  -serial COM7  open this serial port instead
    -baud 9600 -stopbits 2 -parity none
  -ack-timeout 2s   time to wait for the ECR's ACK per attempt
  -retries 3        send attempts towards the ECR
  -dial-timeout 5s  connection timeout towards the terminal
  -log FILE         log file (5 MB, 3 rotations) in addition to the console
  -v                log every byte

zvt-bridge list        show serial ports
zvt-bridge version
```

Example log (one line per APDU, direction, name, length, hex):

```
usb: port opened by the host application
usb: DTR=true RTS=true
ECR → PT   Authorization         15 B  06 01 0C 04 00 00 00 00 12 50 49 09 78 19 40
PT  → ECR  ACK (positive)         3 B  80 00 00
PT  → ECR  Status-Information    26 B  04 0F 17 27 00 04 00 00 00 00 12 50 49 09 78 0B … (+10)
```

## Windows setup

Run as administrator from the unpacked release archive:

```powershell
Set-ExecutionPolicy -Scope Process Bypass
.\windows\install.ps1 -Terminal 192.168.10.50 -DownloadUsbip
```

The installer

- installs usbip-win2 silently if it is missing (downloaded from the pinned
  GitHub release and verified by SHA-256, or taken from `USBip-*-x64.exe` next
  to the script),
- copies `zvt-bridge.exe` to `%ProgramFiles%\zvt-bridge`,
- registers the Windows service **ZvtBridge** (automatic start before login,
  restart after 5/10/30 s on failure, event log source) with `-usb`,
- starts the service, waits for Windows to create the COM port and prints its
  number.

Select that COM port (9600 8N2) as the terminal's serial port in the cash
register software. Browser-based POS software using the Web Serial API needs
permission for the port; Chrome/Edge administrators usually grant it with the
`SerialAllowAllPortsForUrls` policy. Do not run Zadig or similar tools on the
device: replacing `usbser.sys` removes the COM port.

Other modes: `-Mode none -PosPort COM11 -BridgePort COM12` for an existing
COM pair, `-Mode com0com` for the free pair driver (blocked while Memory
Integrity is on). Logs: `%ProgramData%\zvt-bridge\zvt-bridge.log`. Service
control: `zvt-bridge.exe status | stop | start`. Remove with `uninstall.ps1`
(`-UninstallUsbip` also removes usbip-win2).

The terminal must be set to ZVT over TCP/IP ("ECR via LAN", port 20007) and
be reachable from the POS computer: `Test-NetConnection <ip> -Port 20007`.

## Linux

```sh
sudo install -m 755 dist/zvt-bridge /usr/local/bin/
sudo install -m 644 linux/zvt-bridge.service /etc/systemd/system/
sudo systemctl edit zvt-bridge      # adjust ExecStart: -serial /dev/ttyUSB0 -terminal <ip>
sudo systemctl enable --now zvt-bridge
```

`-usb` works on Linux too (`sudo modprobe vhci-hcd && sudo usbip attach -r 127.0.0.1 -b 1-1`
creates `/dev/ttyACMn`), which is how the USB device emulation is tested here.

## Testing without hardware

```sh
make e2e         # PTY pair: fakeecr ↔ socat ↔ zvt-bridge -serial ↔ fakept
make e2e-usbip   # kernel USB/IP: fakeecr ↔ /dev/ttyACMn ↔ vhci-hcd ↔ zvt-bridge -usb ↔ fakept
```

`fakeecr` performs Registration, Authorization (12.50 EUR) and Status-Enquiry
and exits 0 when all three ended with a *Completion*. Both simulators also run
on Windows:

```powershell
.\fakept.exe -listen :20007
.\zvt-bridge.exe run -usb -terminal 127.0.0.1 -v
.\fakeecr.exe -serial COM9        # the port the installer / Device Manager shows
```

## Limitations

- One terminal per bridge instance; connections are always initiated by the
  bridge (ECR → PT). Terminal-initiated connections (PT → ECR, port 20008) are
  not supported.
- Plain TCP, no TLS (as specified for ZVT).
- The emulated device uses the pid.codes vendor id with the **test product id
  0x0001**; a published build should get its own id from [pid.codes](https://pid.codes).
- The USB/IP listener accepts one host controller at a time on 127.0.0.1; it
  implements control, bulk and interrupt transfers, no isochronous ones.

## License

MIT, see `LICENSE`.
