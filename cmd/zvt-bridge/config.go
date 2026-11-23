package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// loadConfigFile applies a "key = value" file to the flags of fs that were not
// given on the command line (command-line options win). Keys are the option
// names of 'run'; lines starting with # or ; are comments.
func loadConfigFile(fs *flag.FlagSet, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	text := strings.TrimPrefix(string(data), "\uFEFF")
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("%s:%d: expected key = value", path, i+1)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			val = val[1 : len(val)-1]
		}
		if fs.Lookup(key) == nil {
			return fmt.Errorf("%s:%d: unknown option %q", path, i+1, key)
		}
		if key == "config" || given[key] {
			continue
		}
		if err := fs.Set(key, val); err != nil {
			return fmt.Errorf("%s:%d: %s: %w", path, i+1, key, err)
		}
	}
	return nil
}

// parseRunFlags parses the command line and then the configuration file named
// by -config, if any.
func parseRunFlags(fs *flag.FlagSet, cfg *runConfig, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if cfg.ConfigFile != "" {
		if err := loadConfigFile(fs, cfg.ConfigFile); err != nil {
			return fmt.Errorf("configuration: %w", err)
		}
	}
	return nil
}

// writeConfigFile stores cfg as a commented configuration file that the
// service reads at start.
func writeConfigFile(path string, cfg *runConfig) error {
	nl := "\n"
	if runtime.GOOS == "windows" {
		nl = "\r\n"
	}
	var lines []string
	add := func(l string) { lines = append(lines, l) }
	add("# zvt-bridge configuration")
	add("# Keys are the options of 'zvt-bridge run'. Lines starting with # are comments.")
	add("# The service reads this file when it starts: after changes run 'zvt-bridge restart'.")
	add("")
	add("# Payment terminal: IP address or host name, with the ZVT port (default 20007).")
	add("terminal = " + cfg.Terminal)
	add("")
	if cfg.USB {
		add("# Emulated USB serial device for the cash register software. Keep the serial")
		add("# number fixed: Windows ties the COM number to it.")
		add("usb = true")
		add("usb-serial = " + cfg.USBSerial)
		add("usb-vid = " + cfg.USBVendor)
		add("usb-pid = " + cfg.USBProduct)
		add("usb-listen = " + cfg.USBListen)
		add("usb-busid = " + cfg.USBBusID)
		add("usb-attach = " + strconv.FormatBool(cfg.USBAttach))
		add("usbip-exe = " + cfg.USBIPExe)
	} else {
		add("# Serial port towards the cash register software (it uses the other end).")
		add("serial = " + cfg.Serial)
		add("baud = " + strconv.Itoa(cfg.Baud))
		add("stopbits = " + strconv.Itoa(cfg.StopBits))
		add("parity = " + cfg.Parity)
	}
	add("")
	add("# Protocol timing.")
	add("ack-timeout = " + cfg.AckTimeout.String())
	add("retries = " + strconv.Itoa(cfg.Retries))
	add("dial-timeout = " + cfg.DialTimeout.String())
	add("")
	add("# Log file (rotated at 5 MB, 3 files kept). v = true logs every byte.")
	add("log = " + cfg.LogFile)
	add("v = " + strconv.FormatBool(cfg.Verbose))
	add("")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(lines, nl)), 0o644)
}

// readConfigFile loads a configuration file into a fresh runConfig.
func readConfigFile(path string) (*runConfig, error) {
	fs, cfg := newRunFlags("config")
	if err := loadConfigFile(fs, path); err != nil {
		return nil, err
	}
	return cfg, nil
}
