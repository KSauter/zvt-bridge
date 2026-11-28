<img src="winres/icon.png" alt="" width="96" align="right">

# zvt-bridge

[![CI](https://github.com/KSauter/zvt-bridge/actions/workflows/ci.yml/badge.svg)](https://github.com/KSauter/zvt-bridge/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/KSauter/zvt-bridge?include_prereleases)](https://github.com/KSauter/zvt-bridge/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

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

A byte outside a frame on the serial side is answered with `NAK`, as terminals
do. Cash register software may probe each serial port that way to find the
terminal; the bridge answers on its own without contacting the terminal.

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
| `cmd/zvt-bridge` | the bridge; on Windows also the setup wizard (`setup`, `uninstall`) and a native service (`install`, `remove`, `start`, `stop`, `status`) |
| `cmd/fakept` | terminal simulator (ZVT over TCP) for tests |
| `cmd/fakeecr` | cash register simulator (ZVT over serial) for tests |
| `internal/zvt` | framing, CRC, decoder state machine, APDU reader, bridge logic |
| `internal/usbip` | USB/IP server and the emulated CDC-ACM device |
| `installer/` | Inno Setup script of the Windows installer and its build script |
| `winres/` | icon, version information and manifest of `zvt-bridge.exe` (go-winres) |
| `linux/zvt-bridge.service` | systemd unit |
| `test/e2e-linux.sh` | end-to-end test with a PTY pair (socat) |
| `test/e2e-usbip-linux.sh` | end-to-end test through the kernel's USB/IP client (`vhci-hcd`, needs root) |

## Build

Requires Go 1.27 or newer (older `go` binaries download the toolchain
automatically via `GOTOOLCHAIN=auto`). No CGO.

```sh
make test            # vet (linux + windows), unit tests
make build           # dist/zvt-bridge, dist/fakept, dist/fakeecr (host OS)
make build-windows   # dist/*.exe (windows/amd64), with icon and version resource
make e2e             # Linux end-to-end test over a PTY pair, needs socat
make e2e-usbip       # Linux end-to-end test over USB/IP, needs usbip + root
make snapshot        # goreleaser: archives for windows/linux, amd64/arm64/arm
```

Releases are built by [goreleaser](https://goreleaser.com) in GitHub Actions
when a `v*` tag is pushed (`.github/workflows/release.yml`). Each release has
the Windows installer `zvt-bridge-setup_<version>_windows_amd64.exe` (see below)
plus archives with all binaries for Windows and Linux. The installer is built
on Windows with [Inno Setup](https://jrsoftware.org/isinfo.php):

```powershell
.\installer\build.ps1 -Version 1.1.0 -Exe dist\zvt-bridge.exe   # → dist\zvt-bridge-setup_1.1.0_windows_amd64.exe
```

The CI installs it silently on a Windows runner and talks ZVT through the
resulting COM port (fakept ↔ installed service ↔ COM port ↔ fakeecr).

## Usage

```
zvt-bridge [run] -terminal 192.168.10.50 (-usb | -serial COM7) [options]
zvt-bridge [run] -config zvt-bridge.conf

  -config FILE  read options from a file ("option = value" per line, # comments);
                options given on the command line override it
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
  -v                log every byte; with -usb also every USB control request

zvt-bridge setup       Windows: setup wizard (default when started without arguments)
zvt-bridge uninstall   Windows: remove service, program files and Settings > Apps entry
zvt-bridge repair      Windows: rebuild a broken emulated COM port (see below)
zvt-bridge install     Windows: register the service with the given options (or -config FILE)
zvt-bridge start | stop | restart | status | remove
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

Download `zvt-bridge-setup_<version>_windows_amd64.exe` from the
[releases](https://github.com/KSauter/zvt-bridge/releases) and run it. The
installer (Inno Setup, German or English) asks for administrator rights and for

- the **payment terminal**: IP address or host name and TCP port (default 20007),
- the **COM port name** for the cash register software: empty lets Windows pick
  the next free number; a fixed one such as `COM8` makes several tills uniform
  (check that it is free, Intel AMT often takes `COM7` for Serial-over-LAN).

It installs usbip-win2 (bundled, BSD-2-Clause, drivers signed by Microsoft) if
needed, the program into `%ProgramFiles%\zvt-bridge`, the Windows service
**ZvtBridge** (automatic start before login, restart after 5/10/30 s on
failure, event log source) and the Start menu folder *ZVT Bridge* with
**Repair COM port**, **Show log** and **Uninstall**. It waits for Windows to
create the COM port, renames it if a fixed number was chosen and shows it on the
last page. The setup output is kept in `%ProgramData%\zvt-bridge\setup.log`.

Running a newer installer updates in place and keeps terminal and COM port.
It also takes over an installation of the self-installing versions 1.0.x.

Unattended (deployment scripts, Intune, …):

```powershell
.\zvt-bridge-setup_1.1.0_windows_amd64.exe /VERYSILENT /TERMINAL=192.168.10.50 /COM=COM8
.\zvt-bridge-setup_1.1.0_windows_amd64.exe /VERYSILENT          # update, keeps the settings
& "$env:ProgramFiles\zvt-bridge\unins000.exe" /VERYSILENT        # remove (usbip-win2 stays)
```

`/TERMINAL` takes `host` or `host:port`; `/LOG=<file>` writes the installer log.

**Changing settings later** (new terminal IP, port, …): run the installer again,
or edit `%ProgramData%\zvt-bridge\zvt-bridge.conf` – a commented
`option = value` file the service reads at every start – and run
`zvt-bridge.exe restart` from the install directory. The COM number is a
property of the device, not of the file: change it through the installer or
Device Manager.

**COM port gone or "Unknown USB Device"?** If Windows no longer sets up the
emulated device (the COM port is greyed out in Device Manager, an "Unknown USB
Device (Device Descriptor Request Failed)" sits on the USBip controller), start
*ZVT Bridge > Repair COM port* from the Start menu, or run

```powershell
& "$env:ProgramFiles\zvt-bridge\zvt-bridge.exe" repair -yes
```

It stops the service, unplugs all USB/IP devices, removes every device entry of
the emulated device (phantoms included) and its cached USB settings, starts the
service again and gives the new port its old COM name. `-reinstall-usbip`
additionally reinstalls usbip-win2, `-com COM8` sets another name. If it still
fails, restart Windows and run it once more.

Windows SmartScreen warns about unsigned downloads ("More info" → "Run
anyway") until the releases are code-signed.

**Without the installer:** `zvt-bridge.exe setup` is a console wizard that
installs the program itself (also unattended: `setup -terminal 192.168.10.50
-download-usbip -yes`, or `-mode serial -serial COM12` for an existing COM port
pair), `zvt-bridge.exe uninstall` removes it again.

Select the printed COM port (9600 8N2) as the terminal's serial port in the
cash register software. Browser-based POS software using the Web Serial API
needs permission for the port; Chrome/Edge administrators usually grant it with
the `SerialAllowAllPortsForUrls` policy. Do not run Zadig or similar tools on
the device: replacing `usbser.sys` removes the COM port.

Logs: `%ProgramData%\zvt-bridge\zvt-bridge.log`. Service control:
`zvt-bridge.exe status | stop | start | restart`.

The terminal must be set to ZVT over TCP/IP ("ECR via LAN", port 20007) and
be reachable from the POS computer: `Test-NetConnection <ip> -Port 20007`.

## Linux

```sh
sudo install -m 755 dist/zvt-bridge /usr/local/bin/
sudo install -m 644 linux/zvt-bridge.service /etc/systemd/system/
sudo install -d /etc/zvt-bridge && sudo install -m 644 linux/zvt-bridge.conf /etc/zvt-bridge/
sudoedit /etc/zvt-bridge/zvt-bridge.conf    # serial port, terminal address
sudo systemctl enable --now zvt-bridge
```

`-usb` works on Linux too (`sudo modprobe vhci-hcd && sudo usbip attach -r 127.0.0.1 -b 1-1`
creates `/dev/ttyACMn`), which is how the USB device emulation is tested here.

## Testing without hardware

```sh
make e2e         # PTY pair: fakeecr ↔ socat ↔ zvt-bridge -serial ↔ fakept
make e2e-usbip   # kernel USB/IP: fakeecr ↔ /dev/ttyACMn ↔ vhci-hcd ↔ zvt-bridge -usb ↔ fakept
```

`fakeecr` performs Registration, Authorization and Status-Enquiry and exits 0
when every sequence ended with a *Completion*. `-steps registration,status`
leaves the Authorization out, which is what you want against a real terminal
(an Authorization makes it ask for a card); `-password` sets the terminal
password for the Registration (default 123456). Both simulators also run on
Windows:

```powershell
.\fakept.exe -listen :20007
.\zvt-bridge.exe run -usb -terminal 127.0.0.1 -v
.\fakeecr.exe -serial COM9        # the port the setup / Device Manager shows
```

## Limitations

- One terminal per bridge instance; connections are always initiated by the
  bridge (ECR → PT). Terminal-initiated connections (PT → ECR, port 20008) are
  not supported.
- Plain TCP, no TLS (as specified for ZVT).
- The emulated device identifies itself as **1209:0001**, the
  [pid.codes](https://pid.codes/1209/0001/) test id. It is not universally
  unique: other test devices may use it too. Windows binds the COM port by USB
  class, not by these ids, and tells devices apart by the serial number
  (`-usb-serial`), so this only matters if a vendor driver for exactly these
  ids is installed. `-usb-vid`/`-usb-pid` (or the configuration file) change
  them; do that before the roll-out, because Windows then sees a new device
  and assigns a new COM number.
- The USB/IP listener accepts one host controller at a time on 127.0.0.1; it
  implements control, bulk and interrupt transfers, no isochronous ones.

## License

MIT, see `LICENSE`.
