// zvt-bridge connects an electronic cash register (ECR) that speaks ZVT over a
// serial port with a payment terminal (PT) that expects ZVT over TCP (port 20007).
//
// Commands:
//
//	zvt-bridge [run] -serial COM7 -terminal 192.168.1.50 [options]
//	zvt-bridge list                             show serial ports
//	zvt-bridge install -serial … -terminal …    install and start the Windows service
//	zvt-bridge remove | start | stop | status   manage the Windows service
//	zvt-bridge version
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.bug.st/serial"

	"zvt-bridge/internal/zvt"
)

var version = "dev"

const serviceName = "ZvtBridge"

type runConfig struct {
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
}

func newRunFlags(name string) (*flag.FlagSet, *runConfig) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	c := &runConfig{}
	fs.StringVar(&c.Serial, "serial", "", "serial port towards the ECR, e.g. COM7 or /dev/ttyUSB0 (required)")
	fs.IntVar(&c.Baud, "baud", 9600, "baud rate")
	fs.IntVar(&c.StopBits, "stopbits", 2, "stop bits (1 or 2)")
	fs.StringVar(&c.Parity, "parity", "none", "parity: none, even, odd")
	fs.StringVar(&c.Terminal, "terminal", "", "terminal address host[:port], default port 20007 (required)")
	fs.DurationVar(&c.AckTimeout, "ack-timeout", 2*time.Second, "time to wait for the ECR's ACK per attempt")
	fs.IntVar(&c.Retries, "retries", 3, "send attempts towards the ECR")
	fs.DurationVar(&c.DialTimeout, "dial-timeout", 5*time.Second, "connection timeout towards the terminal")
	fs.StringVar(&c.LogFile, "log", "", "log file (in addition to the console; as a service defaults to ProgramData)")
	fs.BoolVar(&c.Verbose, "v", false, "log every byte")
	return fs, c
}

func (c *runConfig) validate() error {
	if c.Serial == "" || c.Terminal == "" {
		return fmt.Errorf("-serial and -terminal are required")
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

// args renders the configuration back into a command line (for the service entry).
func (c *runConfig) args() []string {
	a := []string{
		"-serial", c.Serial,
		"-terminal", c.Terminal,
		"-baud", strconv.Itoa(c.Baud),
		"-stopbits", strconv.Itoa(c.StopBits),
		"-parity", c.Parity,
		"-ack-timeout", c.AckTimeout.String(),
		"-retries", strconv.Itoa(c.Retries),
		"-dial-timeout", c.DialTimeout.String(),
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

Commands:
  run      run the bridge in the foreground (default when no command is given)
  list     show serial ports
  install  install and start the Windows service (same options as run)
  remove   remove the Windows service
  start | stop | status   control the Windows service
  version

Options for run and install:
`, version)
	fs, _ := newRunFlags("run")
	fs.SetOutput(os.Stderr)
	fs.PrintDefaults()
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	args := os.Args[1:]
	cmd := "run"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "run":
		os.Exit(cmdRun(args))
	case "list":
		os.Exit(cmdList())
	case "version":
		fmt.Println("zvt-bridge", version)
	case "install", "remove", "start", "stop", "status":
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
	if err := fs.Parse(args); err != nil {
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
		closer = func() { rf.Close() }
	}
	if len(writers) == 0 {
		writers = append(writers, io.Discard)
	}
	log.SetOutput(io.MultiWriter(writers...))
	return closer, nil
}

// runBridge operates the bridge until ctx is cancelled. If the serial side
// fails (e.g. USB adapter unplugged) the port is reopened after a pause.
func runBridge(ctx context.Context, cfg *runConfig) error {
	addr := cfg.addr()
	log.Printf("zvt-bridge %s", version)
	log.Printf("ECR: %s, %d baud, 8 data bits, parity %s, %d stop bits", cfg.Serial, cfg.Baud, cfg.Parity, cfg.StopBits)
	log.Printf("PT: %s (connected on demand)", addr)
	const retryAfter = 5 * time.Second
	for {
		port, err := serial.Open(cfg.Serial, cfg.mode())
		if err != nil {
			log.Printf("serial port %s: %v, retrying in %s", cfg.Serial, err, retryAfter)
		} else {
			log.Printf("ECR: %s opened", cfg.Serial)
			b := &zvt.Bridge{
				Port:       port,
				Dial:       func() (net.Conn, error) { return net.DialTimeout("tcp", addr, cfg.DialTimeout) },
				AckTimeout: cfg.AckTimeout,
				Retries:    cfg.Retries,
				Verbose:    cfg.Verbose,
				Logf:       log.Printf,
			}
			err = b.Run(ctx)
			port.Close()
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
