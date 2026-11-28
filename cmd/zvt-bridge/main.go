// zvt-bridge connects an electronic cash register (ECR) that speaks ZVT over a
// serial port with a payment terminal (PT) that expects ZVT over TCP (port 20007).
//
// Commands:
//
//	zvt-bridge                                  Windows: interactive setup (also by double-click)
//	zvt-bridge setup [-terminal … -yes …]       Windows: setup wizard, unattended with -yes
//	zvt-bridge uninstall                        Windows: remove everything again
//	zvt-bridge repair                           Windows: rebuild a broken emulated COM port
//	zvt-bridge [run] -serial COM7 -terminal 192.168.1.50 [options]
//	zvt-bridge run -config zvt-bridge.conf        same, options from a file
//	zvt-bridge list                             show serial ports
//	zvt-bridge install -serial … -terminal …    install and start the Windows service
//	zvt-bridge remove | start | stop | status   manage the Windows service
//	zvt-bridge version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"go.bug.st/serial"

	"github.com/KSauter/zvt-bridge/internal/usbip"
	"github.com/KSauter/zvt-bridge/internal/zvt"
)

var version = "dev"

type runConfig struct {
	ConfigFile  string
	Serial      string
	Baud        int
	StopBits    int
	Parity      string
	Terminal    string
	AckTimeout  time.Duration
	Retries     int
	DialTimeout time.Duration
	LogFile     string
	Verbose     bool

	// Virtual USB serial device via USB/IP instead of a physical/virtual serial port.
	USB        bool
	USBListen  string
	USBBusID   string
	USBSerial  string
	USBVendor  string
	USBProduct string
	USBAttach  bool
	USBIPExe   string
}

func newRunFlags(name string) (*flag.FlagSet, *runConfig) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	c := &runConfig{}
	fs.StringVar(&c.ConfigFile, "config", "", "configuration file with 'option = value' lines; options on the command line override it")
	fs.StringVar(&c.Serial, "serial", "", "serial port towards the ECR, e.g. COM7 or /dev/ttyUSB0 (required)")
	fs.IntVar(&c.Baud, "baud", 9600, "baud rate")
	fs.IntVar(&c.StopBits, "stopbits", 2, "stop bits (1 or 2)")
	fs.StringVar(&c.Parity, "parity", "none", "parity: none, even, odd")
	fs.StringVar(&c.Terminal, "terminal", "", "terminal address host[:port], default port 20007 (required)")
	fs.DurationVar(&c.AckTimeout, "ack-timeout", 2*time.Second, "time to wait for the ECR's ACK per attempt")
	fs.IntVar(&c.Retries, "retries", 3, "send attempts towards the ECR")
	fs.DurationVar(&c.DialTimeout, "dial-timeout", 5*time.Second, "connection timeout towards the terminal")
	fs.StringVar(&c.LogFile, "log", "", "log file (in addition to the console; as a service defaults to ProgramData)")
	fs.BoolVar(&c.Verbose, "v", false, "log every byte; with -usb also every USB control request")
	fs.BoolVar(&c.USB, "usb", false, "emulate a USB serial device over USB/IP instead of opening -serial")
	fs.StringVar(&c.USBListen, "usb-listen", "127.0.0.1:3240", "USB/IP server address the host controller connects to")
	fs.StringVar(&c.USBBusID, "usb-busid", "1-1", "USB/IP bus id of the emulated device")
	fs.StringVar(&c.USBSerial, "usb-serial", "ZVT-BRIDGE-1", "USB serial number; keep it fixed so Windows reuses the COM number")
	fs.StringVar(&c.USBVendor, "usb-vid", "1209", "USB vendor id (hex); 1209 is pid.codes")
	fs.StringVar(&c.USBProduct, "usb-pid", "0001", "USB product id (hex); 0001 is the pid.codes test id")
	fs.BoolVar(&c.USBAttach, "usb-attach", true, "Windows: plug the device in via usbip.exe (usbip-win2)")
	fs.StringVar(&c.USBIPExe, "usbip-exe", defaultUsbipExe, "path of usbip.exe (usbip-win2)")
	return fs, c
}

func (c *runConfig) validate() error {
	if c.Terminal == "" {
		return fmt.Errorf("-terminal is required")
	}
	if c.Serial == "" && !c.USB {
		return fmt.Errorf("either -serial or -usb is required")
	}
	if c.Serial != "" && c.USB {
		return fmt.Errorf("-serial and -usb exclude each other")
	}
	if c.USB {
		if _, err := strconv.ParseUint(c.USBVendor, 16, 16); err != nil {
			return fmt.Errorf("-usb-vid: %w", err)
		}
		if _, err := strconv.ParseUint(c.USBProduct, 16, 16); err != nil {
			return fmt.Errorf("-usb-pid: %w", err)
		}
	}
	if c.StopBits != 1 && c.StopBits != 2 {
		return fmt.Errorf("-stopbits must be 1 or 2")
	}
	switch strings.ToLower(c.Parity) {
	case "none", "even", "odd":
	default:
		return fmt.Errorf("unknown parity %q", c.Parity)
	}
	return nil
}

func (c *runConfig) addr() string {
	if strings.Contains(c.Terminal, ":") {
		return c.Terminal
	}
	return c.Terminal + ":20007"
}

func (c *runConfig) mode() *serial.Mode {
	m := &serial.Mode{BaudRate: c.Baud, DataBits: 8, Parity: serial.NoParity, StopBits: serial.TwoStopBits}
	switch strings.ToLower(c.Parity) {
	case "even":
		m.Parity = serial.EvenParity
	case "odd":
		m.Parity = serial.OddParity
	}
	if c.StopBits == 1 {
		m.StopBits = serial.OneStopBit
	}
	return m
}

// args renders the configuration back into a command line (for the service
// entry). With a configuration file the service reads everything from there.
func (c *runConfig) args() []string {
	if c.ConfigFile != "" {
		return []string{"-config", c.ConfigFile}
	}
	a := []string{
		"-terminal", c.Terminal,
		"-baud", strconv.Itoa(c.Baud),
		"-stopbits", strconv.Itoa(c.StopBits),
		"-parity", c.Parity,
		"-ack-timeout", c.AckTimeout.String(),
		"-retries", strconv.Itoa(c.Retries),
		"-dial-timeout", c.DialTimeout.String(),
	}
	if c.USB {
		a = append(a, "-usb", "-usb-listen", c.USBListen, "-usb-busid", c.USBBusID, "-usb-serial", c.USBSerial,
			"-usb-vid", c.USBVendor, "-usb-pid", c.USBProduct, "-usb-attach="+strconv.FormatBool(c.USBAttach), "-usbip-exe", c.USBIPExe)
	} else {
		a = append(a, "-serial", c.Serial)
	}
	if c.LogFile != "" {
		a = append(a, "-log", c.LogFile)
	}
	if c.Verbose {
		a = append(a, "-v")
	}
	return a
}

func usage() {
	fmt.Fprintf(os.Stderr, `zvt-bridge %s – ZVT over serial (ECR) ↔ ZVT over TCP (payment terminal)

Two ways to reach the cash register:
  -serial COMx   a real or virtual serial port
  -usb           emulate a USB serial device (CDC-ACM) over USB/IP; Windows' own
                 usbser.sys then provides the COM port (needs usbip-win2 on Windows)

Commands:
  setup      Windows: interactive setup wizard – installs usbip-win2, the program and the
             service, and shows the COM port (default when started without arguments;
             -yes makes it unattended, see 'zvt-bridge setup -h')
  uninstall  Windows: remove the service, the program files and the Settings > Apps entry
  repair     Windows: rebuild the emulated COM port when Windows no longer sets it up
             (phantom COM port, "Unknown USB Device"); see 'zvt-bridge repair -h'
  run        run the bridge in the foreground
  list       show serial ports
  install    install and start the Windows service (same options as run; with -config
             the service reads all settings from that file at every start)
  remove     remove the Windows service
  start | stop | restart | status   control the Windows service
  version

Options for run and install:
`, version)
	fs, _ := newRunFlags("run")
	fs.SetOutput(os.Stderr)
	fs.PrintDefaults()
}

// defaultCommand picks what happens without arguments: on Windows the setup
// wizard (a copy named uninstall.exe runs the uninstaller), elsewhere run.
func defaultCommand() string {
	if runtime.GOOS != "windows" {
		return "run"
	}
	if exe, err := os.Executable(); err == nil &&
		strings.HasPrefix(strings.ToLower(filepath.Base(exe)), "uninstall") {
		return "uninstall"
	}
	return "setup"
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	args := os.Args[1:]
	cmd := "run"
	switch {
	case len(args) == 0:
		cmd = defaultCommand()
	case !strings.HasPrefix(args[0], "-"):
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		os.Exit(cmdRun(args))
	case "setup":
		os.Exit(cmdSetup(args))
	case "uninstall":
		os.Exit(cmdUninstall(args))
	case "repair":
		os.Exit(cmdRepair(args))
	case "list":
		os.Exit(cmdList())
	case "version":
		fmt.Println("zvt-bridge", version)
	case "install", "remove", "start", "stop", "restart", "status":
		os.Exit(cmdService(cmd, args))
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func cmdList() int {
	ports, err := serial.GetPortsList()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if len(ports) == 0 {
		fmt.Println("no serial ports found")
	}
	for _, p := range ports {
		fmt.Println(p)
	}
	return 0
}

func cmdRun(args []string) int {
	fs, cfg := newRunFlags("run")
	fs.Usage = usage
	if err := parseRunFlags(fs, cfg, args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, err)
		}
		return 2
	}
	if err := cfg.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		usage()
		return 2
	}
	if isSvc, _ := runningAsService(); isSvc {
		return runAsService(cfg)
	}
	closeLog, err := setupLogging(cfg.LogFile, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer closeLog()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runBridge(ctx, cfg); err != nil {
		log.Print(err)
		return 1
	}
	log.Println("stopped")
	return 0
}

// setupLogging sends the log to a rotating file and optionally to the console.
func setupLogging(path string, console bool) (func(), error) {
	var writers []io.Writer
	if console {
		writers = append(writers, os.Stderr)
	}
	closer := func() {}
	if path != "" {
		rf, err := newRotatingFile(path, 5<<20, 3)
		if err != nil {
			return nil, fmt.Errorf("log file %s: %w", path, err)
		}
		writers = append(writers, rf)
		closer = func() { _ = rf.Close() }
	}
	if len(writers) == 0 {
		writers = append(writers, io.Discard)
	}
	log.SetOutput(io.MultiWriter(writers...))
	return closer, nil
}

func newBridge(port io.ReadWriteCloser, cfg *runConfig) *zvt.Bridge {
	addr := cfg.addr()
	return &zvt.Bridge{
		Port:       port,
		Dial:       func() (net.Conn, error) { return net.DialTimeout("tcp", addr, cfg.DialTimeout) },
		AckTimeout: cfg.AckTimeout,
		Retries:    cfg.Retries,
		Verbose:    cfg.Verbose,
		Logf:       log.Printf,
	}
}

// runBridge operates the bridge until ctx is cancelled.
func runBridge(ctx context.Context, cfg *runConfig) error {
	log.Printf("zvt-bridge %s", version)
	log.Printf("PT: %s (connected on demand)", cfg.addr())
	if cfg.USB {
		return runUSB(ctx, cfg)
	}
	return runSerial(ctx, cfg)
}

// runUSB serves an emulated USB serial device over USB/IP and bridges it.
func runUSB(ctx context.Context, cfg *runConfig) error {
	vid, _ := strconv.ParseUint(cfg.USBVendor, 16, 16)
	pid, _ := strconv.ParseUint(cfg.USBProduct, 16, 16)
	dev := &usbip.ACM{
		VendorID: uint16(vid), ProductID: uint16(pid),
		Manufacturer: "zvt-bridge", Product: "zvt-bridge virtual serial port", Serial: cfg.USBSerial,
		BusID: cfg.USBBusID, Logf: log.Printf, Trace: cfg.Verbose,
	}
	// released fires when the host controller lets go of the device.
	released := make(chan struct{}, 1)
	var wasAttached atomic.Bool
	dev.OnState = func(st usbip.ACMState) {
		if wasAttached.Swap(st.Attached) && !st.Attached {
			select {
			case released <- struct{}{}:
			default:
			}
		}
	}
	srv := &usbip.Server{Addr: cfg.USBListen, Device: dev, Logf: log.Printf, Trace: cfg.Verbose}
	ln, err := net.Listen("tcp", cfg.USBListen)
	if err != nil {
		return fmt.Errorf("USB/IP server: %w", err)
	}
	log.Printf("ECR: USB serial device %04X:%04X serial %q, USB/IP server on %s, bus id %s", vid, pid, cfg.USBSerial, ln.Addr(), cfg.USBBusID)
	// The server outlives ctx until the device is unplugged: closing the
	// USB/IP connection first looks like a pulled cable to the host controller.
	srvCtx, stopSrv := context.WithCancel(context.Background())
	defer stopSrv()
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Serve(srvCtx, ln) }()
	// attachLoop unplugs the device when its context ends; wait for that
	// before returning, or the process exits first and leaves a dead device
	// on the virtual hub.
	attachCtx, stopAttach := context.WithCancel(ctx)
	attachDone := make(chan struct{})
	if cfg.USBAttach {
		go func() {
			defer close(attachDone)
			attachLoop(attachCtx, cfg, ln.Addr().String(), released, func() bool { return dev.State().Attached })
		}()
	} else {
		close(attachDone)
	}
	defer func() {
		stopAttach()
		select {
		case <-attachDone:
		case <-time.After(15 * time.Second):
			log.Printf("usbip detach: no answer from usbip.exe, giving up")
		}
	}()
	b := newBridge(dev, cfg)
	runErr := make(chan error, 1)
	go func() { runErr <- b.Run(ctx) }()
	select {
	case err := <-srvErr:
		_ = dev.Close()
		if err := <-runErr; err != nil {
			log.Printf("bridge: %v", err)
		}
		return fmt.Errorf("USB/IP server: %w", err)
	case err := <-runErr:
		_ = dev.Close()
		return err
	case <-ctx.Done():
		_ = dev.Close()
		if err := <-runErr; err != nil {
			log.Printf("bridge: %v", err)
		}
		return nil
	}
}

// runSerial opens a serial port and bridges it. If the port disappears
// (e.g. USB adapter unplugged) it is reopened after a pause.
func runSerial(ctx context.Context, cfg *runConfig) error {
	log.Printf("ECR: %s, %d baud, 8 data bits, parity %s, %d stop bits", cfg.Serial, cfg.Baud, cfg.Parity, cfg.StopBits)
	const retryAfter = 5 * time.Second
	for {
		port, err := serial.Open(cfg.Serial, cfg.mode())
		if err != nil {
			log.Printf("serial port %s: %v, retrying in %s", cfg.Serial, err, retryAfter)
		} else {
			log.Printf("ECR: %s opened", cfg.Serial)
			err = newBridge(port, cfg).Run(ctx)
			_ = port.Close()
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("bridge interrupted: %v, retrying in %s", err, retryAfter)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(retryAfter):
		}
	}
}
