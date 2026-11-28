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

// usbipStagingDir holds the usbip-win2 installer between the SHA-256 check
// and its start. It lies in the secured data directory, so no user can swap
// the file in between (the user's %TEMP% would allow that).
func usbipStagingDir() string { return filepath.Join(dataDir(), "usbip-setup") }

// downloadUsbip fetches the pinned release into the staging directory and
// verifies its SHA-256.
func downloadUsbip(p *prompter) (string, error) {
	if err := secureDataDir(dataDir()); err != nil {
		return "", err
	}
	if err := os.MkdirAll(usbipStagingDir(), 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(usbipStagingDir(), "USBip-"+usbipVersion+"-x64.exe")
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
	staged, err := stageUsbipSetup(setupPath)
	if err != nil {
		return err
	}
	defer os.RemoveAll(usbipStagingDir())
	p.printf("Installing usbip-win2 from %s ... ", setupPath)
	cmd := exec.Command(staged, "/VERYSILENT", "/SUPPRESSMSGBOXES", "/NORESTART", "/COMPONENTS=main,client", "/TASKS=vcredist")
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

// stageUsbipSetup copies the installer into the staging directory (unless it
// is there already) and checks the copy against the pinned SHA-256, so the
// file that runs is the file that was checked.
func stageUsbipSetup(src string) (string, error) {
	if err := secureDataDir(dataDir()); err != nil {
		return "", err
	}
	if err := os.MkdirAll(usbipStagingDir(), 0o755); err != nil {
		return "", err
	}
	dst := filepath.Join(usbipStagingDir(), "USBip-"+usbipVersion+"-x64.exe")
	if !sameFile(src, dst) {
		in, err := os.Open(src)
		if err != nil {
			return "", err
		}
		defer in.Close()
		out, err := os.Create(dst)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(out, in)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", fmt.Errorf("copy %s: %w", src, err)
		}
	}
	sum, err := fileSHA256(dst)
	if err != nil {
		return "", err
	}
	if sum != usbipSHA256 {
		os.Remove(dst)
		return "", fmt.Errorf("%s is not the usbip-win2 %s installer (SHA-256 %s, expected %s)", src, usbipVersion, sum, usbipSHA256)
	}
	return dst, nil
}

// usbipController reports whether the virtual USB host controller of
// usbip-win2 is present and its Configuration Manager problem code. Its
// instance id is a generic ROOT\USB\nnnn, so it is recognised by its hardware
// id (ROOT\USBIP_WIN2\...) or, failing that, by its description.
func usbipController() (present bool, problem uint32, err error) {
	err = forEachDevice(nil, "ROOT", func(set windows.DevInfo, data *windows.DevInfoData, _ string) error {
		if !isUsbipController(set, data) {
			return nil
		}
		present = true
		var status uint32
		windows.CM_Get_DevNode_Status(&status, &problem, data.DevInst, 0)
		return nil
	})
	return present, problem, err
}

func isUsbipController(set windows.DevInfo, data *windows.DevInfoData) bool {
	if v, err := set.DeviceRegistryProperty(data, windows.SPDRP_HARDWAREID); err == nil {
		ids, _ := v.([]string)
		for _, id := range ids {
			if strings.Contains(strings.ToUpper(id), "USBIP_WIN2") {
				return true
			}
		}
	}
	for _, prop := range []windows.SPDRP{windows.SPDRP_FRIENDLYNAME, windows.SPDRP_DEVICEDESC} {
		if v, err := set.DeviceRegistryProperty(data, prop); err == nil {
			name, _ := v.(string)
			name = strings.ToUpper(name)
			if strings.Contains(name, "USBIP") && strings.Contains(name, "HOST CONTROLLER") {
				return true
			}
		}
	}
	return false
}

// waitForUsbipController polls until the host controller is present and
// started or the timeout ends; right after the installation Windows needs a
// few seconds to set it up.
func waitForUsbipController(timeout time.Duration) (present bool, problem uint32, err error) {
	deadline := time.Now().Add(timeout)
	for {
		present, problem, err = usbipController()
		if (err == nil && present && problem == 0) || time.Now().After(deadline) {
			return present, problem, err
		}
		time.Sleep(time.Second)
	}
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
