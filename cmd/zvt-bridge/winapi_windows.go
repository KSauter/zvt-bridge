//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	modKernel32               = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleProcessList = modKernel32.NewProc("GetConsoleProcessList")

	// msports.dll keeps the "COM name arbiter" database that Device Manager
	// consults when it hands out COM numbers.
	modMsports                   = windows.NewLazySystemDLL("msports.dll")
	procComDBOpen                = modMsports.NewProc("ComDBOpen")
	procComDBClose               = modMsports.NewProc("ComDBClose")
	procComDBClaimPort           = modMsports.NewProc("ComDBClaimPort")
	procComDBReleasePort         = modMsports.NewProc("ComDBReleasePort")
	procComDBGetCurrentPortUsage = modMsports.NewProc("ComDBGetCurrentPortUsage")
)

// guidDevClassPorts is the device setup class "Ports (COM & LPT)".
var guidDevClassPorts = windows.GUID{Data1: 0x4D36E978, Data2: 0xE325, Data3: 0x11CE,
	Data4: [8]byte{0xBF, 0xC1, 0x08, 0x00, 0x2B, 0xE1, 0x03, 0x18}}

func isElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// relaunchElevated starts this program again with the same arguments through
// the UAC prompt. The new process gets a console window of its own.
func relaunchElevated() error {
	exe, err := currentExe()
	if err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	args, _ := windows.UTF16PtrFromString(windows.ComposeCommandLine(os.Args[1:]))
	cwd, _ := windows.UTF16PtrFromString(filepath.Dir(exe))
	return windows.ShellExecute(0, verb, file, args, cwd, windows.SW_SHOWNORMAL)
}

// ownsConsole reports whether this process is the only one attached to its
// console, which means the window closes as soon as the program exits
// (started by double-click or through the UAC prompt).
func ownsConsole() bool {
	var pids [4]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

// ---- COM name arbiter -------------------------------------------------------------------

type comDB uintptr

func openComDB() (comDB, error) {
	var h uintptr
	if r, _, _ := procComDBOpen.Call(uintptr(unsafe.Pointer(&h))); r != 0 {
		return 0, fmt.Errorf("ComDBOpen: %w", syscall.Errno(r))
	}
	return comDB(h), nil
}

func (db comDB) close() { procComDBClose.Call(uintptr(db)) }

// inUse reports whether COM<n> is recorded as taken.
func (db comDB) inUse(n int) (bool, error) {
	buf := make([]byte, 512) // one bit per port, COM1 … COM4096
	var reported uint32
	const cdbReportBits = 0
	if r, _, _ := procComDBGetCurrentPortUsage.Call(uintptr(db), uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)), cdbReportBits, uintptr(unsafe.Pointer(&reported))); r != 0 {
		return false, fmt.Errorf("ComDBGetCurrentPortUsage: %w", syscall.Errno(r))
	}
	i := n - 1
	if i < 0 || i/8 >= len(buf) {
		return false, nil
	}
	return buf[i/8]&(1<<(i%8)) != 0, nil
}

func (db comDB) claim(n int) error {
	var forced uint32
	if r, _, _ := procComDBClaimPort.Call(uintptr(db), uintptr(n), 0, uintptr(unsafe.Pointer(&forced))); r != 0 {
		return fmt.Errorf("ComDBClaimPort(COM%d): %w", n, syscall.Errno(r))
	}
	return nil
}

func (db comDB) release(n int) error {
	if r, _, _ := procComDBReleasePort.Call(uintptr(db), uintptr(n)); r != 0 {
		return fmt.Errorf("ComDBReleasePort(COM%d): %w", n, syscall.Errno(r))
	}
	return nil
}

// comPortInUse asks the COM name arbiter whether COM<n> is taken.
func comPortInUse(n int) (bool, error) {
	db, err := openComDB()
	if err != nil {
		return false, err
	}
	defer db.close()
	return db.inUse(n)
}

// ---- Plug and Play devices ----------------------------------------------------------------

type comPort struct {
	Name         string // COM7
	InstanceID   string // USB\VID_1209&PID_0001&MI_00\…
	FriendlyName string // USB Serial Device (COM7)
	Problem      uint32 // Configuration Manager problem code, 0 = none
}

// forEachDevice calls fn for every present device of class (nil = any class)
// below the given enumerator ("USB", "ROOT", …).
func forEachDevice(class *windows.GUID, enumerator string, fn func(windows.DevInfo, *windows.DevInfoData, string) error) error {
	flags := windows.DIGCF_PRESENT
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

// usbComPorts lists the present COM ports of the first function of the USB
// device vid:pid, i.e. the CDC interface of the emulated serial device.
func usbComPorts(vid, pid string) ([]comPort, error) {
	prefix := strings.ToUpper(fmt.Sprintf(`USB\VID_%s&PID_%s&MI_00\`, vid, pid))
	var ports []comPort
	err := forEachDevice(&guidDevClassPorts, "USB", func(set windows.DevInfo, data *windows.DevInfoData, id string) error {
		if !strings.HasPrefix(strings.ToUpper(id), prefix) {
			return nil
		}
		cp := comPort{InstanceID: id}
		if key, err := set.OpenDevRegKey(data, windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DEV, registry.QUERY_VALUE); err == nil {
			k := registry.Key(key)
			cp.Name, _, _ = k.GetStringValue("PortName")
			k.Close()
		}
		if v, err := set.DeviceRegistryProperty(data, windows.SPDRP_FRIENDLYNAME); err == nil {
			cp.FriendlyName, _ = v.(string)
		}
		var status uint32
		windows.CM_Get_DevNode_Status(&status, &cp.Problem, data.DevInst, 0)
		ports = append(ports, cp)
		return nil
	})
	return ports, err
}

// renameComPort gives the port a new COM name the way Device Manager does:
// claim the number in the COM database, write PortName, adjust the friendly
// name, restart the device so usbser.sys creates the new name, and release
// the old number.
func renameComPort(port comPort, newName string) error {
	_, newNum, err := parseComName(newName)
	if err != nil {
		return err
	}
	_, oldNum, _ := parseComName(port.Name)
	db, err := openComDB()
	if err != nil {
		return err
	}
	defer db.close()
	if newNum != oldNum {
		if err := db.claim(newNum); err != nil {
			if errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
				return fmt.Errorf("%s is already in use by another device", newName)
			}
			return err
		}
	}
	found := false
	err = forEachDevice(&guidDevClassPorts, "USB", func(set windows.DevInfo, data *windows.DevInfoData, id string) error {
		if !strings.EqualFold(id, port.InstanceID) {
			return nil
		}
		found = true
		key, err := set.OpenDevRegKey(data, windows.DICS_FLAG_GLOBAL, 0, windows.DIREG_DEV, registry.SET_VALUE)
		if err != nil {
			return fmt.Errorf("open device parameters: %w", err)
		}
		k := registry.Key(key)
		err = k.SetStringValue("PortName", newName)
		k.Close()
		if err != nil {
			return fmt.Errorf("set PortName: %w", err)
		}
		friendly := port.FriendlyName
		if old := "(" + port.Name + ")"; port.Name != "" && strings.Contains(friendly, old) {
			friendly = strings.Replace(friendly, old, "("+newName+")", 1)
		} else if friendly != "" {
			friendly += " (" + newName + ")"
		}
		if friendly != "" {
			set.SetDeviceRegistryPropertyString(data, windows.SPDRP_FRIENDLYNAME, friendly)
		}
		params := windows.PropChangeParams{
			ClassInstallHeader: *windows.MakeClassInstallHeader(windows.DIF_PROPERTYCHANGE),
			StateChange:        windows.DICS_PROPCHANGE,
			Scope:              windows.DICS_FLAG_GLOBAL,
		}
		if err := set.SetClassInstallParams(data, &params.ClassInstallHeader, uint32(unsafe.Sizeof(params))); err != nil {
			return fmt.Errorf("SetClassInstallParams: %w", err)
		}
		if err := set.CallClassInstaller(windows.DIF_PROPERTYCHANGE, data); err != nil {
			return fmt.Errorf("restart device: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("device %s is not present", port.InstanceID)
	}
	if newNum != oldNum && oldNum > 0 {
		db.release(oldNum)
	}
	return nil
}

// ---- Settings > Apps entry ----------------------------------------------------------------

const uninstallKeyPath = `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\zvt-bridge`

// registerUninstall lists the program under Settings > Apps.
func registerUninstall(exe, uninstaller, installDir string) error {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, uninstallKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	values := map[string]string{
		"DisplayName":          "ZVT Bridge",
		"DisplayVersion":       version,
		"Publisher":            "zvt-bridge",
		"InstallLocation":      installDir,
		"DisplayIcon":          exe,
		"UninstallString":      fmt.Sprintf(`"%s"`, uninstaller),
		"QuietUninstallString": fmt.Sprintf(`"%s" uninstall -yes`, uninstaller),
	}
	for name, v := range values {
		if err := k.SetStringValue(name, v); err != nil {
			return err
		}
	}
	k.SetDWordValue("NoModify", 1)
	k.SetDWordValue("NoRepair", 1)
	if fi, err := os.Stat(exe); err == nil {
		k.SetDWordValue("EstimatedSize", uint32(fi.Size()/1024))
	}
	return nil
}

func unregisterUninstall() error {
	err := registry.DeleteKey(registry.LOCAL_MACHINE, uninstallKeyPath)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	return err
}

// registeredInstallDir returns the directory recorded by registerUninstall.
func registeredInstallDir() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, uninstallKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, _ := k.GetStringValue("InstallLocation")
	return v
}

// deleteAtReboot schedules a file or empty directory that is still in use
// (such as the running program) for deletion at the next start of Windows.
func deleteAtReboot(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(p, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}
