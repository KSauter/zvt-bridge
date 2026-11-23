//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

// Pinned usbip-win2 release (BSD-2-Clause; drivers signed by Microsoft).
const (
	usbipVersion = "0.9.8.1"
	usbipURL     = "https://github.com/vadimgrn/usbip-win2/releases/download/v." + usbipVersion + "/USBip-" + usbipVersion + "-x64.exe"
	usbipSHA256  = "38cad6d4432b52d5bb9409d9ad03b72fdffc4ada4cd3a48fbeca1a2752a8518a"
)

// findUsbipExe returns the installed usbip.exe or "".
func findUsbipExe() string {
	if _, err := os.Stat(defaultUsbipExe); err == nil {
		return defaultUsbipExe
	}
	return ""
}

// findLocalUsbipSetup looks for USBip-*-x64.exe next to the program, so a
// release archive can ship the installer for offline setups.
func findLocalUsbipSetup() string {
	exe, err := currentExe()
	if err != nil {
		return ""
	}
	dir := filepath.Dir(exe)
	for _, d := range []string{dir, filepath.Join(dir, "windows"), filepath.Dir(dir)} {
		matches, _ := filepath.Glob(filepath.Join(d, "USBip-*-x64.exe"))
		if len(matches) > 0 {
			sort.Strings(matches)
			return matches[len(matches)-1]
		}
	}
	return ""
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// downloadUsbip fetches the pinned release into the temp directory and
// verifies its SHA-256.
func downloadUsbip(p *prompter) (string, error) {
	dst := filepath.Join(os.TempDir(), "USBip-"+usbipVersion+"-x64.exe")
	if sum, err := fileSHA256(dst); err == nil && sum == usbipSHA256 {
		p.printf("Using the already downloaded %s\n", dst)
		return dst, nil
	}
	p.printf("Downloading %s ... ", usbipURL)
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(usbipURL)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: HTTP %s", resp.Status)
	}
	f, err := os.Create(dst)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), resp.Body)
	f.Close()
	if err != nil {
		os.Remove(dst)
		return "", fmt.Errorf("download: %w", err)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != usbipSHA256 {
		os.Remove(dst)
		return "", fmt.Errorf("download: SHA-256 mismatch (got %s, expected %s)", sum, usbipSHA256)
	}
	p.printf("%.1f MB, SHA-256 ok\n", float64(n)/(1<<20))
	return dst, nil
}

// installUsbip runs the usbip-win2 installer silently with the virtual host
// controller ("main") and the usbip.exe client.
func installUsbip(p *prompter, setupPath string) error {
	p.printf("Installing usbip-win2 from %s ... ", setupPath)
	cmd := exec.Command(setupPath, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/COMPONENTS=main,client", "/TASKS=vcredist")
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("usbip-win2 setup exited with code %d (7 or 8: a pending restart blocks the installation; restart Windows and run the setup again)", ee.ExitCode())
		}
		return fmt.Errorf("usbip-win2 setup: %w", err)
	}
	if findUsbipExe() == "" {
		return fmt.Errorf("usbip.exe not found after the installation (%s)", defaultUsbipExe)
	}
	p.printf("ok\n")
	return nil
}

// usbipController reports whether the virtual USB host controller of
// usbip-win2 is present and its Configuration Manager problem code.
func usbipController() (present bool, problem uint32, err error) {
	err = forEachDevice(nil, "ROOT", func(_ windows.DevInfo, data *windows.DevInfoData, id string) error {
		if !strings.Contains(strings.ToUpper(id), "USBIP_WIN2") {
			return nil
		}
		present = true
		var status uint32
		windows.CM_Get_DevNode_Status(&status, &problem, data.DevInst, 0)
		return nil
	})
	return present, problem, err
}

// uninstallUsbip detaches all USB/IP devices and runs the usbip-win2 uninstaller.
func uninstallUsbip() error {
	dir := filepath.Dir(defaultUsbipExe)
	if exe := findUsbipExe(); exe != "" {
		exec.Command(exe, "detach", "--all").Run()
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "unins*.exe"))
	if len(matches) == 0 {
		return fmt.Errorf("no uninstaller found in %s", dir)
	}
	sort.Strings(matches)
	return exec.Command(matches[0], "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART").Run()
}
