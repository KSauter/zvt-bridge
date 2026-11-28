package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// setupConfig is what the setup wizard collects. Every value can be preset
// with a flag; -yes answers all remaining questions with their defaults so the
// setup also runs unattended (deployment scripts, Intune, ...).
type setupConfig struct {
	Terminal      string
	Mode          string // usb | serial
	SerialPort    string
	USBSerial     string
	ComPort       string // wanted COM name of the emulated device; "" or "auto" lets Windows choose
	InstallDir    string
	LogFile       string
	ConfigFile    string
	UsbipSetup    string
	DownloadUsbip bool
	Yes           bool
	Installer     bool   // called by the installer package, which owns the program files
	ResultFile    string // receives the COM port name for the cash register software
}

func newSetupFlags(name string) (*flag.FlagSet, *setupConfig) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	c := &setupConfig{}
	fs.StringVar(&c.Terminal, "terminal", "", "terminal address host[:port], default port 20007 (asked if missing)")
	fs.StringVar(&c.Mode, "mode", "usb", "usb: emulated USB serial port via usbip-win2; serial: use an existing COM port")
	fs.StringVar(&c.SerialPort, "serial", "", "serial mode: COM port the bridge opens")
	fs.StringVar(&c.USBSerial, "usb-serial", "ZVT-BRIDGE-1", "usb mode: USB serial number; Windows ties the COM number to it")
	fs.StringVar(&c.ComPort, "com", "", "usb mode: COM port name the device shall get, e.g. COM7 (default: assigned by Windows)")
	fs.StringVar(&c.InstallDir, "install-dir", defaultInstallDir(), "where zvt-bridge.exe is installed")
	fs.StringVar(&c.LogFile, "log", defaultLogPath(), "log file of the service")
	fs.StringVar(&c.ConfigFile, "config", defaultConfigPath(), "configuration file the service reads at every start")
	fs.StringVar(&c.UsbipSetup, "usbip-setup", "", "usb mode: path to the usbip-win2 installer (USBip-<version>-x64.exe)")
	fs.BoolVar(&c.DownloadUsbip, "download-usbip", false, "usb mode: download the pinned usbip-win2 release when it is not installed")
	fs.BoolVar(&c.Yes, "yes", false, "do not ask anything, take the defaults and the given flags")
	fs.BoolVar(&c.Installer, "installer", false, "called by the installer: leave program files and the Settings > Apps entry alone")
	fs.StringVar(&c.ResultFile, "result", "", "write the COM port name for the cash register software to this file")
	return fs, c
}

type uninstallConfig struct {
	RemoveUsbip bool
	KeepLogs    bool
	Yes         bool
	Installer   bool
}

func newUninstallFlags(name string) (*flag.FlagSet, *uninstallConfig) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	c := &uninstallConfig{}
	fs.BoolVar(&c.RemoveUsbip, "remove-usbip", false, "also uninstall usbip-win2")
	fs.BoolVar(&c.KeepLogs, "keep-logs", false, "keep the configuration and log files")
	fs.BoolVar(&c.Yes, "yes", false, "do not ask for confirmation")
	fs.BoolVar(&c.Installer, "installer", false, "called by the installer: only remove the service (and usbip-win2, logs), not the program files")
	return fs, c
}

// repairConfig controls "repair": rebuild the emulated USB serial port when
// Windows no longer sets it up (phantom COM port, "Unknown USB Device").
type repairConfig struct {
	ComPort        string
	ConfigFile     string
	ReinstallUsbip bool
	UsbipSetup     string
	DownloadUsbip  bool
	Yes            bool
}

func newRepairFlags(name string) (*flag.FlagSet, *repairConfig) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	c := &repairConfig{}
	fs.StringVar(&c.ComPort, "com", "", "COM port name the device shall get afterwards (default: the name it had, 'auto' = chosen by Windows)")
	fs.StringVar(&c.ConfigFile, "config", defaultConfigPath(), "configuration file of the service")
	fs.BoolVar(&c.ReinstallUsbip, "reinstall-usbip", false, "also uninstall and reinstall usbip-win2")
	fs.StringVar(&c.UsbipSetup, "usbip-setup", "", "path to the usbip-win2 installer for -reinstall-usbip")
	fs.BoolVar(&c.DownloadUsbip, "download-usbip", false, "download the pinned usbip-win2 release for -reinstall-usbip")
	fs.BoolVar(&c.Yes, "yes", false, "do not ask anything")
	return fs, c
}

func cmdRepair(args []string) int {
	fs, cfg := newRepairFlags("repair")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return runRepair(cfg, newPrompter(os.Stdin, os.Stdout, cfg.Yes))
}

func cmdSetup(args []string) int {
	fs, cfg := newSetupFlags("setup")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if cfg.Mode != "usb" && cfg.Mode != "serial" {
		fmt.Fprintf(os.Stderr, "-mode must be usb or serial, not %q\n", cfg.Mode)
		return 2
	}
	return runSetup(cfg, newPrompter(os.Stdin, os.Stdout, cfg.Yes))
}

func cmdUninstall(args []string) int {
	fs, cfg := newUninstallFlags("uninstall")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	return runUninstall(cfg, newPrompter(os.Stdin, os.Stdout, cfg.Yes))
}

// terminalAddr validates "host" or "host:port" and returns the dial address
// with the default ZVT port filled in.
func terminalAddr(s string) (string, error) {
	s = strings.TrimSpace(s)
	host, port := s, "20007"
	if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
	} else if strings.Count(s, ":") == 1 || strings.HasPrefix(s, "[") {
		return "", fmt.Errorf("%q is not a valid address; use host or host:port", s)
	}
	if host == "" {
		return "", errors.New("a host name or IP address is required")
	}
	if strings.ContainsAny(host, " \t/\\") {
		return "", fmt.Errorf("%q is not a valid host name or IP address", host)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("%q is not a valid port", port)
	}
	return net.JoinHostPort(host, port), nil
}

// probeTerminal checks whether the terminal accepts TCP connections.
func probeTerminal(addr string, timeout time.Duration) error {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return c.Close()
}

var comNameRe = regexp.MustCompile(`(?i)^COM([1-9][0-9]{0,2})$`)

// parseComName accepts "COM7" or "7" and returns the canonical name and number.
func parseComName(s string) (string, int, error) {
	s = strings.TrimSpace(s)
	if _, err := strconv.Atoi(s); err == nil {
		s = "COM" + s
	}
	m := comNameRe.FindStringSubmatch(s)
	if m == nil {
		return "", 0, fmt.Errorf("%q is not a COM port name (e.g. COM7)", s)
	}
	n, _ := strconv.Atoi(m[1])
	if n > 256 {
		return "", 0, fmt.Errorf("COM%d: Windows allows COM1 to COM256", n)
	}
	return "COM" + m[1], n, nil
}

// isAutoCom tells whether a COM answer means "let Windows choose".
func isAutoCom(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "" || s == "auto" || s == "automatic"
}
