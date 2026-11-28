//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.bug.st/serial"
	"golang.org/x/sys/windows"
)

// USB identity of the emulated device. 1209 is the pid.codes vendor id,
// 0001 its test product id (not unique, see README).
const (
	usbVendorID  = "1209"
	usbProductID = "0001"
)

const uninstallerName = "uninstall.exe"

func defaultInstallDir() string {
	pf := os.Getenv("ProgramFiles")
	if pf == "" {
		pf = `C:\Program Files`
	}
	return filepath.Join(pf, "zvt-bridge")
}

func defaultConfigPath() string {
	return filepath.Join(filepath.Dir(defaultLogPath()), "zvt-bridge.conf")
}

// ensureElevated relaunches the program through UAC when it lacks
// administrator rights. It returns true when the caller must exit with code.
func ensureElevated(p *prompter) (exit bool, code int) {
	if isElevated() {
		return false, 0
	}
	if p.assumeYes {
		p.printf("administrator rights are required; run this from an elevated prompt\n")
		return true, 1
	}
	p.printf("Administrator rights are required. Please confirm the Windows prompt;\nthe program continues in a new window.\n")
	if err := relaunchElevated(); err != nil {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			p.printf("The request for administrator rights was declined.\n")
		} else {
			p.printf("could not request administrator rights: %v\n", err)
		}
		if ownsConsole() {
			p.pause("Press Enter to close this window.")
		}
		return true, 1
	}
	return true, 0
}

func runSetup(cfg *setupConfig, p *prompter) int {
	if exit, code := ensureElevated(p); exit {
		return code
	}
	if err := secureDataDir(dataDir()); err != nil {
		p.printf("Setup failed: %v\n", err)
		return 1
	}
	if cfg.OutputFile != "" {
		f, err := os.Create(cfg.OutputFile)
		if err != nil {
			p.printf("Setup failed: %v\n", err)
			return 1
		}
		defer f.Close()
		p.out = io.MultiWriter(p.out, f)
	}
	if ownsConsole() {
		defer p.pause("\nPress Enter to close this window.")
	}
	if err := setupWizard(cfg, p); err != nil {
		if errors.Is(err, io.EOF) {
			p.printf("\nSetup aborted.\n")
		} else {
			p.printf("\nSetup failed: %v\n", err)
		}
		return 1
	}
	return 0
}

func runUninstall(cfg *uninstallConfig, p *prompter) int {
	if exit, code := ensureElevated(p); exit {
		return code
	}
	if ownsConsole() {
		defer p.pause("\nPress Enter to close this window.")
	}
	if err := uninstallWizard(cfg, p); err != nil {
		if errors.Is(err, io.EOF) {
			p.printf("\nAborted.\n")
		} else {
			p.printf("\nUninstall failed: %v\n", err)
		}
		return 1
	}
	return 0
}

var errCancelled = errors.New("cancelled")

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, name+"=") {
			return a[len(name)+1:]
		}
	}
	return ""
}

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

func fileExists(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func setupWizard(cfg *setupConfig, p *prompter) error {
	p.printf("zvt-bridge %s – setup\n", version)
	p.printf("Connects cash register software that expects a serial ZVT terminal with a\npayment terminal that speaks ZVT over TCP/IP.\n\n")

	if cfg.ConfigFile == "" {
		cfg.ConfigFile = defaultConfigPath()
	}
	if prev, err := readConfigFile(cfg.ConfigFile); err == nil {
		p.printf("The existing configuration %s was found. Its values are offered as\ndefaults; the installation is renewed.\n\n", cfg.ConfigFile)
		if cfg.Terminal == "" {
			cfg.Terminal = prev.Terminal
		}
		if !prev.USB && prev.Serial != "" {
			cfg.Mode = "serial"
			if cfg.SerialPort == "" {
				cfg.SerialPort = prev.Serial
			}
		}
		if prev.USB && prev.USBSerial != "" && cfg.USBSerial == "ZVT-BRIDGE-1" {
			cfg.USBSerial = prev.USBSerial
		}
		if prev.LogFile != "" && cfg.LogFile == defaultLogPath() {
			cfg.LogFile = prev.LogFile
		}
	} else if prev := installedServiceArgs(); prev != nil {
		p.printf("An existing %s service was found. Its settings are offered as defaults;\nit is replaced by the new one.\n\n", serviceName)
		if cfg.Terminal == "" {
			cfg.Terminal = flagValue(prev, "-terminal")
		}
		if !hasFlag(prev, "-usb") && flagValue(prev, "-serial") != "" {
			cfg.Mode = "serial"
			if cfg.SerialPort == "" {
				cfg.SerialPort = flagValue(prev, "-serial")
			}
		}
	}

	// ---- 1. Terminal
	p.printf("Step 1/4: payment terminal\n")
	defHost, defPort := cfg.Terminal, "20007"
	if h, pt, err := net.SplitHostPort(cfg.Terminal); err == nil {
		defHost, defPort = h, pt
	}
	host, err := p.ask("Terminal IP address or host name", defHost,
		func(s string) error { _, err := terminalAddr(s); return err })
	if err != nil {
		return err
	}
	if h, pt, err := net.SplitHostPort(host); err == nil { // host:port typed at once
		host, defPort = h, pt
	}
	port, err := p.ask("Terminal TCP port for ZVT", defPort, func(s string) error {
		if n, err := strconv.Atoi(s); err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("%q is not a valid port number", s)
		}
		return nil
	})
	if err != nil {
		return err
	}
	cfg.Terminal = net.JoinHostPort(host, port)
	p.printf("Connecting to %s ... ", cfg.Terminal)
	if err := probeTerminal(cfg.Terminal, 3*time.Second); err != nil {
		p.printf("failed (%v)\n", err)
		p.printf("The terminal must be configured for ZVT over TCP/IP (\"ECR via LAN\", port 20007)\nand be reachable from this computer. The service keeps retrying, so the setup\ncan continue nevertheless.\n")
		ok, err := p.yesNo("Continue anyway?", true)
		if err != nil {
			return err
		}
		if !ok {
			return errCancelled
		}
	} else {
		p.printf("ok\n")
	}

	// ---- 2. Serial port
	p.printf("\nStep 2/4: serial port for the cash register software\n")
	modes := []string{
		"Emulated USB serial port (recommended): Windows' own usbser.sys provides the\n     COM port; the Microsoft-signed virtual USB controller of usbip-win2 plugs it in",
		"Existing COM port: the bridge opens one end of a virtual COM pair or a\n     null-modem cable, the cash register software the other",
	}
	def := 0
	if cfg.Mode == "serial" {
		def = 1
	}
	choice, err := p.choose("How should the cash register software reach the bridge?", modes, def)
	if err != nil {
		return err
	}
	usb := choice == 0
	cfg.Mode = "serial"
	if usb {
		cfg.Mode = "usb"
	}
	var usbipExe string
	var ownPorts []comPort
	if !usb {
		if ports, err := serial.GetPortsList(); err == nil && len(ports) > 0 {
			p.printf("Serial ports on this computer: %s\n", strings.Join(ports, ", "))
		}
		cfg.SerialPort, err = p.ask("COM port the bridge opens", cfg.SerialPort,
			func(s string) error { _, _, err := parseComName(s); return err })
		if err != nil {
			return err
		}
		cfg.SerialPort, _, _ = parseComName(cfg.SerialPort)
	} else {
		usbipExe = findUsbipExe()
		if usbipExe != "" {
			p.printf("usbip-win2: installed (%s)\n", usbipExe)
		} else {
			p.printf("usbip-win2 is not installed yet.\n")
			if cfg.UsbipSetup == "" {
				cfg.UsbipSetup = findLocalUsbipSetup()
			}
			if cfg.UsbipSetup != "" {
				p.printf("Installer found: %s\n", cfg.UsbipSetup)
			} else {
				download := cfg.DownloadUsbip
				if !download {
					if p.assumeYes {
						return errors.New("usbip-win2 is not installed; pass -download-usbip or -usbip-setup <path>")
					}
					p.printf("Release %s (BSD-2-Clause, drivers signed by Microsoft) can be downloaded from\n%s\nand is verified by SHA-256 before it runs.\n", usbipVersion, usbipURL)
					if download, err = p.yesNo("Download usbip-win2 now?", true); err != nil {
						return err
					}
				}
				if !download {
					if cfg.UsbipSetup, err = p.ask("Path to the usbip-win2 installer (USBip-<version>-x64.exe)", "", fileExists); err != nil {
						return err
					}
				}
			}
		}
		ownPorts, _ = usbComPorts(usbVendorID, usbProductID)
		comDef := "auto"
		if len(ownPorts) > 0 && ownPorts[0].Name != "" {
			comDef = ownPorts[0].Name
		}
		if cfg.ComPort != "" {
			comDef = cfg.ComPort
		}
		p.printf("Windows gives the new port the next free COM number and keeps it. A fixed\nnumber (e.g. COM7) makes several installations uniform.\n")
		cfg.ComPort, err = p.ask("COM port name for the cash register software ('auto' = chosen by Windows)", comDef,
			func(s string) error {
				if isAutoCom(s) {
					return nil
				}
				name, n, err := parseComName(s)
				if err != nil {
					return err
				}
				for _, op := range ownPorts {
					if strings.EqualFold(op.Name, name) {
						return nil
					}
				}
				if used, err := comPortInUse(n); err == nil && used {
					return fmt.Errorf("%s is already taken by another device (Device Manager > Ports)", name)
				}
				return nil
			})
		if err != nil {
			return err
		}
		if isAutoCom(cfg.ComPort) {
			cfg.ComPort = ""
		} else {
			cfg.ComPort, _, _ = parseComName(cfg.ComPort)
		}
	}

	// ---- 3. Files
	p.printf("\nStep 3/4: program files\n")
	if cfg.Installer {
		self, err := currentExe()
		if err != nil {
			return err
		}
		cfg.InstallDir = filepath.Dir(self)
		p.printf("Install directory: %s (managed by the installer)\n", cfg.InstallDir)
	} else if cfg.InstallDir, err = p.ask("Install directory", cfg.InstallDir, nil); err != nil {
		return err
	}
	if cfg.LogFile, err = p.ask("Log file", cfg.LogFile, nil); err != nil {
		return err
	}
	p.printf("The settings are stored in %s; the service reads that file at every start.\n", cfg.ConfigFile)
	exe := filepath.Join(cfg.InstallDir, "zvt-bridge.exe")
	uninstaller := filepath.Join(cfg.InstallDir, uninstallerName)

	// ---- 4. Summary
	p.printf("\nStep 4/4: summary\n")
	p.printf("  Terminal:     %s\n", cfg.Terminal)
	if usb {
		name := "assigned by Windows"
		if cfg.ComPort != "" {
			name = cfg.ComPort
		}
		p.printf("  Serial port:  emulated USB serial device (USB serial number %s), COM name %s\n", cfg.USBSerial, name)
		if usbipExe == "" {
			src := cfg.UsbipSetup
			if src == "" {
				src = "download from GitHub"
			}
			p.printf("  usbip-win2:   install version %s (%s)\n", usbipVersion, src)
		}
	} else {
		p.printf("  Serial port:  %s (existing; the cash register software uses the other end)\n", cfg.SerialPort)
	}
	p.printf("  Program:      %s\n", exe)
	p.printf("  Service:      %s, starts automatically with Windows, restarts on failure\n", serviceName)
	p.printf("  Log:          %s\n", cfg.LogFile)
	p.printf("  Settings:     %s\n", cfg.ConfigFile)
	ok, err := p.yesNo("\nInstall now?", true)
	if err != nil {
		return err
	}
	if !ok {
		return errCancelled
	}
	p.printf("\n")

	// ---- Install
	if usb && usbipExe == "" {
		setupPath := cfg.UsbipSetup
		if setupPath == "" {
			if setupPath, err = downloadUsbip(p); err != nil {
				return err
			}
		}
		if err := installUsbip(p, setupPath); err != nil {
			return err
		}
		usbipExe = defaultUsbipExe
	}
	if usb {
		present, problem, err := waitForUsbipController(20 * time.Second)
		switch {
		case err != nil:
			p.printf("warning: could not check the usbip-win2 host controller: %v\n", err)
		case !present:
			p.printf("warning: the usbip-win2 virtual host controller is not present yet; a restart of Windows may be needed\n")
		case problem != 0:
			p.printf("warning: the usbip-win2 host controller reports problem code %d (see Device Manager)\n", problem)
		default:
			p.printf("usbip-win2 host controller: ok\n")
		}
	}
	if serviceInstalled() {
		p.printf("Stopping and removing the existing service ... ")
		if err := removeService(); err != nil && !errors.Is(err, errServiceNotInstalled) {
			return err
		}
		p.printf("ok\n")
	}
	if cfg.Installer {
		removeSelfInstall(p, cfg.InstallDir)
	} else if err := installFiles(p, cfg.InstallDir, exe, uninstaller); err != nil {
		return err
	}
	_, rc := newRunFlags("setup")
	rc.Terminal = cfg.Terminal
	rc.LogFile = cfg.LogFile
	if usb {
		rc.USB = true
		rc.USBSerial = cfg.USBSerial
		rc.USBVendor = usbVendorID
		rc.USBProduct = usbProductID
		rc.USBIPExe = usbipExe
	} else {
		rc.Serial = cfg.SerialPort
	}
	if err := writeConfigFile(cfg.ConfigFile, rc); err != nil {
		return fmt.Errorf("write %s: %w", cfg.ConfigFile, err)
	}
	rc.ConfigFile = cfg.ConfigFile // the service reads the file at every start
	p.printf("Installing service %s ... ", serviceName)
	if _, err := installService(rc, exe); err != nil {
		return err
	}
	p.printf("running\n")
	if !cfg.Installer {
		if err := registerUninstall(exe, uninstaller, cfg.InstallDir); err != nil {
			p.printf("warning: not registered under Settings > Apps: %v\n", err)
		}
	}

	posPort := cfg.SerialPort
	if usb {
		p.printf("Waiting for Windows to create the COM port ... ")
		port, err := waitForComPort(30*time.Second, "")
		if err != nil {
			p.printf("\n%v\nCheck %s and Device Manager (Ports: \"USB Serial Device\";\nUSB controllers: \"USBip Emulated Host Controller\").\nIf an old or broken device is in the way, \"%s\" repair rebuilds the port.\n", err, cfg.LogFile, exe)
			return errors.New("the COM port did not appear")
		}
		p.printf("%s\n", port.Name)
		port = assignComName(p, port, cfg.ComPort)
		posPort = port.Name
	}

	if cfg.ResultFile != "" {
		if err := os.WriteFile(cfg.ResultFile, []byte(posPort), 0o644); err != nil {
			p.printf("warning: write %s: %v\n", cfg.ResultFile, err)
		}
	}

	p.printf("\nDone.\n")
	if usb {
		p.printf("  Serial port for the cash register software: %s (9600 baud, 8 data bits, no parity, 2 stop bits)\n", posPort)
	} else {
		p.printf("  The bridge uses %s; select the other end of the pair in the cash register software\n  (9600 baud, 8 data bits, no parity, 2 stop bits).\n", posPort)
	}
	p.printf("  Service %s is running. Log: %s\n", serviceName, cfg.LogFile)
	p.printf("  Settings: %s\n    (edit, then \"%s\" restart; or run the setup again)\n", cfg.ConfigFile, exe)
	if cfg.Installer {
		p.printf("  Remove: Settings > Apps > ZVT Bridge\n")
	} else {
		p.printf("  Remove: Settings > Apps > ZVT Bridge, or %s\n", uninstaller)
	}
	return nil
}

// installFiles copies the running program into the install directory, once as
// zvt-bridge.exe and once as uninstall.exe.
func installFiles(p *prompter, dir, exe, uninstaller string) error {
	src, err := currentExe()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("read %s: %w", src, err)
	}
	p.printf("Installing program files to %s ... ", dir)
	for _, dst := range []string{exe, uninstaller} {
		if sameFile(src, dst) {
			continue
		}
		if err := os.WriteFile(dst, data, 0o755); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}
	p.printf("ok\n")
	return nil
}

// removeSelfInstall cleans up after a version that installed itself (1.0.x):
// its Settings > Apps entry and uninstall.exe would otherwise stay next to
// the installer's own.
func removeSelfInstall(p *prompter, dir string) {
	if registeredInstallDir() != "" {
		if err := unregisterUninstall(); err == nil {
			p.printf("Removed the Settings > Apps entry of the previous self-installed version\n")
		}
	}
	if err := os.Remove(filepath.Join(dir, uninstallerName)); err == nil {
		p.printf("Removed %s of the previous version\n", uninstallerName)
	}
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// waitForComPort polls until a working COM port of the emulated device is
// present, with the wanted name if one is given.
func waitForComPort(timeout time.Duration, wantName string) (comPort, error) {
	deadline := time.Now().Add(timeout)
	var last []comPort
	for {
		ports, err := usbComPorts(usbVendorID, usbProductID)
		if err == nil {
			for _, cp := range ports {
				if cp.Name != "" && cp.Problem == 0 && (wantName == "" || strings.EqualFold(cp.Name, wantName)) {
					return cp, nil
				}
			}
			last = ports
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	for _, cp := range last {
		if cp.Problem != 0 {
			return comPort{}, fmt.Errorf("the device %s is present but reports problem code %d", cp.InstanceID, cp.Problem)
		}
	}
	return comPort{}, fmt.Errorf("no COM port appeared within %s", timeout)
}

func uninstallWizard(cfg *uninstallConfig, p *prompter) error {
	p.printf("zvt-bridge %s – uninstall\n\n", version)
	dir := registeredInstallDir()
	if dir == "" {
		dir = defaultInstallDir()
	}
	if cfg.Installer {
		p.printf("This removes the %s service.\n", serviceName)
	} else {
		p.printf("This removes the %s service and the program files in %s.\n", serviceName, dir)
	}
	ok, err := p.yesNo("Continue?", true)
	if err != nil {
		return err
	}
	if !ok {
		return errCancelled
	}
	removeUsbip := cfg.RemoveUsbip
	if !removeUsbip && !p.assumeYes && findUsbipExe() != "" {
		if removeUsbip, err = p.yesNo("Also uninstall usbip-win2 (only if nothing else on this computer uses USB/IP)?", false); err != nil {
			return err
		}
	}
	logDir := filepath.Dir(defaultLogPath())
	removeLogs := !cfg.KeepLogs
	if removeLogs && !p.assumeYes {
		if removeLogs, err = p.yesNo(fmt.Sprintf("Delete the configuration and log files in %s?", logDir), true); err != nil {
			return err
		}
	}
	p.printf("\nRemoving service ... ")
	switch err := removeService(); {
	case errors.Is(err, errServiceNotInstalled):
		p.printf("not installed\n")
	case err != nil:
		return err
	default:
		p.printf("ok\n")
	}
	if !cfg.Installer {
		if err := unregisterUninstall(); err != nil {
			p.printf("warning: Settings > Apps entry not removed: %v\n", err)
		}
	}
	if removeUsbip {
		p.printf("Uninstalling usbip-win2 ... ")
		if err := uninstallUsbip(); err != nil {
			p.printf("failed: %v\n", err)
		} else {
			p.printf("ok\n")
		}
	}
	if removeLogs {
		os.RemoveAll(logDir)
	}
	if cfg.Installer {
		p.printf("\nService removed; the installer removes the program files.\n")
		return nil
	}
	p.printf("Removing program files in %s ... ", dir)
	pending, err := removeInstallDir(dir)
	switch {
	case err != nil:
		p.printf("failed: %v\n", err)
	case pending:
		p.printf("ok (%s is in use and disappears at the next start of Windows)\n", uninstallerName)
	default:
		p.printf("ok\n")
	}
	p.printf("\nzvt-bridge has been removed.\n")
	return nil
}

// removeInstallDir deletes the files the setup put into dir and then the
// directory. Files still in use (the running uninstaller) are scheduled for
// deletion at the next start of Windows.
func removeInstallDir(dir string) (pending bool, err error) {
	if dir == "" || filepath.Clean(dir) == filepath.VolumeName(dir)+`\` {
		return false, fmt.Errorf("refusing to remove %q", dir)
	}
	for _, name := range []string{"zvt-bridge.exe", uninstallerName, "install.ps1", "uninstall.ps1"} {
		path := filepath.Join(dir, name)
		rmErr := os.Remove(path)
		if rmErr == nil || errors.Is(rmErr, fs.ErrNotExist) {
			continue
		}
		if dErr := deleteAtReboot(path); dErr != nil {
			return pending, rmErr
		}
		pending = true
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		if pending {
			_ = deleteAtReboot(dir)
		} else {
			return false, fmt.Errorf("%s not removed (other files inside?): %w", dir, err)
		}
	}
	return pending, nil
}

// probeTerminal checks whether the terminal accepts TCP connections.
func probeTerminal(addr string, timeout time.Duration) error {
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return err
	}
	return c.Close()
}

// pause waits for Enter; used to keep a console window open that would
// otherwise close together with the program.
func (p *prompter) pause(label string) {
	if p.assumeYes {
		return
	}
	p.printf("%s", label)
	_, _ = p.readLine()
}
