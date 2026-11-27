//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

var (
	modCfgmgr32          = windows.NewLazySystemDLL("cfgmgr32.dll")
	procCMGetParent      = modCfgmgr32.NewProc("CM_Get_Parent")
	procCMGetDeviceIDW   = modCfgmgr32.NewProc("CM_Get_Device_IDW")
	procCMLocateDevNodeW = modCfgmgr32.NewProc("CM_Locate_DevNodeW")
)

func runRepair(cfg *repairConfig, p *prompter) int {
	if exit, code := ensureElevated(p); exit {
		return code
	}
	if ownsConsole() {
		defer p.pause("\nPress Enter to close this window.")
	}
	if err := repairUSB(cfg, p); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, errCancelled) {
			p.printf("\nAborted.\n")
		} else {
			p.printf("\nRepair failed: %v\n", err)
		}
		return 1
	}
	return 0
}

// repairUSB stops the service, unplugs every USB/IP device, removes all device
// nodes of the emulated serial device (including phantoms and a failed
// "Unknown USB Device" on the usbip-win2 hub) and the cached USB flags,
// optionally reinstalls usbip-win2, starts the service again and gives the
// new COM port its old name.
func repairUSB(cfg *repairConfig, p *prompter) error {
	p.printf("zvt-bridge %s – repair the emulated USB serial port\n\n", version)
	rc, err := readConfigFile(cfg.ConfigFile)
	if err != nil {
		return fmt.Errorf("read %s: %w (is the bridge installed?)", cfg.ConfigFile, err)
	}
	if !rc.USB {
		return fmt.Errorf("%s does not use the emulated USB serial port (usb = false); nothing to repair", cfg.ConfigFile)
	}
	vid, pid := strings.ToUpper(rc.USBVendor), strings.ToUpper(rc.USBProduct)
	usbipExe := rc.USBIPExe
	if usbipExe == "" {
		usbipExe = defaultUsbipExe
	}

	// The COM name hangs on the device node that is about to be removed.
	want := cfg.ComPort
	if want == "" {
		want = previousComName(vid, pid)
	}
	if isAutoCom(want) {
		want = ""
	} else if want, _, err = parseComName(want); err != nil {
		return err
	}

	nodes, err := staleDeviceNodes(vid, pid)
	if err != nil {
		return err
	}
	p.printf("This will\n")
	p.printf("  - stop the service %s and unplug all USB/IP devices\n", serviceName)
	p.printf("  - remove the device entries of the emulated serial device (%d found):\n", len(nodes))
	for _, n := range nodes {
		p.printf("      %s\n", n)
	}
	p.printf("  - clear the cached USB settings of %s:%s\n", vid, pid)
	if cfg.ReinstallUsbip {
		p.printf("  - uninstall and reinstall usbip-win2 %s\n", usbipVersion)
	}
	p.printf("  - start the service again and wait for the COM port")
	if want != "" {
		p.printf(" (name: %s)", want)
	}
	p.printf("\n")
	ok, err := p.yesNo("\nContinue?", true)
	if err != nil {
		return err
	}
	if !ok {
		return errCancelled
	}
	p.printf("\n")

	p.printf("Stopping the service ... ")
	if err := setServiceRunning(false); err != nil {
		return err
	}
	p.printf("ok\n")
	if _, err := exec.LookPath(usbipExe); err == nil {
		p.printf("Unplugging all USB/IP devices ... ")
		if out, err := exec.Command(usbipExe, "detach", "--all").CombinedOutput(); err != nil {
			p.printf("%v: %s\n", err, strings.TrimSpace(string(out)))
		} else {
			p.printf("ok\n")
		}
	}

	var setupPath string
	if cfg.ReinstallUsbip {
		if setupPath, err = usbipInstaller(cfg, p); err != nil {
			return err
		}
		p.printf("Uninstalling usbip-win2 ... ")
		if err := uninstallUsbip(); err != nil {
			p.printf("%v\n", err)
		} else {
			p.printf("ok\n")
		}
	}

	p.printf("Removing device entries ... ")
	removed, failed := removeDeviceNodes(vid, pid)
	p.printf("%d removed\n", removed)
	for _, f := range failed {
		p.printf("  could not remove %s\n", f)
	}
	key := `SYSTEM\CurrentControlSet\Control\usbflags\` + vid + pid + "0100"
	if err := registry.DeleteKey(registry.LOCAL_MACHINE, key); err == nil {
		p.printf("Cleared the cached USB settings (HKLM\\%s)\n", key)
	}

	if cfg.ReinstallUsbip {
		if err := installUsbip(p, setupPath); err != nil {
			return err
		}
	}
	present, problem, err := waitForUsbipController(20 * time.Second)
	switch {
	case err != nil:
		p.printf("warning: could not check the usbip-win2 host controller: %v\n", err)
	case !present:
		return errors.New("the usbip-win2 virtual host controller is missing; restart Windows and run repair again, or add -reinstall-usbip")
	case problem != 0:
		return fmt.Errorf("the usbip-win2 host controller reports problem code %d; restart Windows and run repair again", problem)
	default:
		p.printf("usbip-win2 host controller: ok\n")
	}

	p.printf("Starting the service ... ")
	if err := setServiceRunning(true); err != nil {
		return err
	}
	p.printf("ok\n")
	p.printf("Waiting for Windows to create the COM port ... ")
	port, err := waitForComPort(30*time.Second, "")
	if err != nil {
		p.printf("\n%v\n", err)
		return fmt.Errorf("the COM port did not come back; restart Windows and run repair again (with -reinstall-usbip if that does not help). Log: %s", rc.LogFile)
	}
	p.printf("%s\n", port.Name)
	port = assignComName(p, port, want)
	p.printf("\nDone. The cash register software uses %s (9600 baud, 8 data bits, no parity, 2 stop bits).\n", port.Name)
	return nil
}

// usbipInstaller finds or downloads the usbip-win2 installer.
func usbipInstaller(cfg *repairConfig, p *prompter) (string, error) {
	if cfg.UsbipSetup != "" {
		return cfg.UsbipSetup, nil
	}
	if path := findLocalUsbipSetup(); path != "" {
		return path, nil
	}
	download := cfg.DownloadUsbip
	if !download && !p.assumeYes {
		var err error
		if download, err = p.yesNo("Download usbip-win2 "+usbipVersion+" now?", true); err != nil {
			return "", err
		}
	}
	if !download {
		return "", errors.New("no usbip-win2 installer; pass -download-usbip or -usbip-setup <path>")
	}
	return downloadUsbip(p)
}

// assignComName renames port to want (if set and different) and returns the
// port as it is afterwards.
func assignComName(p *prompter, port comPort, want string) comPort {
	if want == "" || strings.EqualFold(port.Name, want) {
		return port
	}
	p.printf("Renaming %s to %s ... ", port.Name, want)
	if err := renameComPort(port, want); err != nil {
		p.printf("failed: %v\nThe port keeps the name %s. It can be changed in Device Manager > Ports >\nProperties > Port Settings > Advanced.\n", err, port.Name)
		return port
	}
	renamed, err := waitForComPort(20*time.Second, want)
	if err != nil {
		p.printf("the port has not come back as %s yet (%v)\n", want, err)
		port.Name = want
		return port
	}
	p.printf("ok\n")
	return renamed
}

// previousComName returns the COM name of the emulated device, present or not.
func previousComName(vid, pid string) string {
	prefix := strings.ToUpper(fmt.Sprintf(`USB\VID_%s&PID_%s&MI_00\`, vid, pid))
	name := ""
	forEachDeviceAll(&guidDevClassPorts, "USB", func(set windows.DevInfo, data *windows.DevInfoData, id string) error {
		if name != "" || !strings.HasPrefix(strings.ToUpper(id), prefix) {
			return nil
		}
		if key, err := set.OpenDevRegKey(data, windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DEV, registry.QUERY_VALUE); err == nil {
			k := registry.Key(key)
			name, _, _ = k.GetStringValue("PortName")
			k.Close()
		}
		return nil
	})
	return name
}

// isRepairTarget tells whether a device node belongs to the emulated serial
// device or is a failed device on the usbip-win2 hub.
func isRepairTarget(id, vid, pid string, controllers []string) bool {
	up := strings.ToUpper(id)
	if strings.HasPrefix(up, fmt.Sprintf(`USB\VID_%s&PID_%s`, vid, pid)) {
		return true
	}
	// Windows names a device whose descriptor it could not read VID_0000&PID_0000.
	// Only remove those that hang below the usbip-win2 controller.
	return strings.HasPrefix(up, `USB\VID_0000&PID_0000\`) && underController(id, controllers)
}

func staleDeviceNodes(vid, pid string) ([]string, error) {
	controllers := usbipControllerIDs()
	var ids []string
	err := forEachDeviceAll(nil, "USB", func(_ windows.DevInfo, _ *windows.DevInfoData, id string) error {
		if isRepairTarget(id, vid, pid, controllers) {
			ids = append(ids, id)
		}
		return nil
	})
	return ids, err
}

// removeDeviceNodes uninstalls the device nodes, children first, so Windows
// enumerates the device from scratch and assigns a COM name again.
func removeDeviceNodes(vid, pid string) (removed int, failed []string) {
	controllers := usbipControllerIDs()
	// Interfaces (MI_xx) and the COM port before the composite parent.
	for _, pass := range []func(string) bool{
		func(id string) bool { return strings.Contains(strings.ToUpper(id), "&MI_") },
		func(id string) bool { return true },
	} {
		forEachDeviceAll(nil, "USB", func(set windows.DevInfo, data *windows.DevInfoData, id string) error {
			if !isRepairTarget(id, vid, pid, controllers) || !pass(id) {
				return nil
			}
			if err := set.CallClassInstaller(windows.DIF_REMOVE, data); err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", id, err))
			} else {
				removed++
			}
			return nil
		})
	}
	return removed, failed
}

// forEachDeviceAll is forEachDevice including devices that are not present.
func forEachDeviceAll(class *windows.GUID, enumerator string, fn func(windows.DevInfo, *windows.DevInfoData, string) error) error {
	var flags windows.DIGCF
	if class == nil {
		flags |= windows.DIGCF_ALLCLASSES
	}
	set, err := windows.SetupDiGetClassDevsEx(class, enumerator, 0, flags, 0, "")
	if err != nil {
		return fmt.Errorf("SetupDiGetClassDevs: %w", err)
	}
	defer set.Close()
	for i := 0; ; i++ {
		data, err := set.EnumDeviceInfo(i)
		if err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
				return nil
			}
			return fmt.Errorf("SetupDiEnumDeviceInfo: %w", err)
		}
		id, err := set.DeviceInstanceID(data)
		if err != nil {
			continue
		}
		if err := fn(set, data, id); err != nil {
			return err
		}
	}
}

func usbipControllerIDs() []string {
	var ids []string
	forEachDevice(nil, "ROOT", func(set windows.DevInfo, data *windows.DevInfoData, id string) error {
		if isUsbipController(set, data) {
			ids = append(ids, strings.ToUpper(id))
		}
		return nil
	})
	return ids
}

// underController walks up to four parents of a present device node and
// reports whether one of them is a usbip-win2 controller.
func underController(id string, controllers []string) bool {
	if len(controllers) == 0 {
		return false
	}
	devInst, err := locateDevNode(id)
	if err != nil {
		return false
	}
	for range 4 {
		var parent uint32
		if r, _, _ := procCMGetParent.Call(uintptr(unsafe.Pointer(&parent)), uintptr(devInst), 0); r != 0 {
			return false
		}
		pid, err := deviceID(parent)
		if err != nil {
			return false
		}
		for _, c := range controllers {
			if strings.EqualFold(pid, c) {
				return true
			}
		}
		devInst = parent
	}
	return false
}

func locateDevNode(id string) (uint32, error) {
	p, err := syscall.UTF16PtrFromString(id)
	if err != nil {
		return 0, err
	}
	var devInst uint32
	const cmLocateDevNodeNormal = 0
	if r, _, _ := procCMLocateDevNodeW.Call(uintptr(unsafe.Pointer(&devInst)), uintptr(unsafe.Pointer(p)), cmLocateDevNodeNormal); r != 0 {
		return 0, fmt.Errorf("CM_Locate_DevNode: CR %d", r)
	}
	return devInst, nil
}

func deviceID(devInst uint32) (string, error) {
	buf := make([]uint16, 512)
	if r, _, _ := procCMGetDeviceIDW.Call(uintptr(devInst), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0); r != 0 {
		return "", fmt.Errorf("CM_Get_Device_ID: CR %d", r)
	}
	return windows.UTF16ToString(buf), nil
}

// setServiceRunning starts or stops the service and waits for the new state.
func setServiceRunning(run bool) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("service control manager: %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed; run the setup instead", serviceName)
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	if run {
		if st.State == svc.Running {
			return nil
		}
		if err := s.Start(); err != nil {
			return fmt.Errorf("start service: %w", err)
		}
		if waitState(s, svc.Running, 30*time.Second) != svc.Running {
			return errors.New("the service did not start; see the log")
		}
		return nil
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		return fmt.Errorf("stop service: %w", err)
	}
	if waitState(s, svc.Stopped, 30*time.Second) != svc.Stopped {
		return errors.New("the service did not stop in time")
	}
	return nil
}
