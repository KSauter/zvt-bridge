//go:build windows

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
)

// dataDirSDDL: owned by Administrators, not inheriting from %ProgramData%
// (where every user may create files), full control for SYSTEM and
// Administrators, read for Users (so the Start menu entry can show the log).
const (
	dataDirSDDL  = "O:BAD:PAI(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;BU)"
	dataFileSDDL = "O:BAD:PAI(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)"
)

// dataDir is %ProgramData%\zvt-bridge: configuration, logs, staged installers.
func dataDir() string { return filepath.Dir(defaultConfigPath()) }

// secureDataDir creates dir if needed and gives it and everything in it the
// owner and DACL above. The service runs as LocalSystem and reads its
// settings (including the path of usbip.exe) from here, so a folder a user
// created in advance must not stay under that user's control. Reparse points
// (junctions, symbolic links) are refused or removed, so nothing is
// redirected elsewhere.
func secureDataDir(dir string) error {
	if fi, err := os.Lstat(dir); err == nil {
		if isReparsePoint(fi) || !fi.IsDir() {
			return fmt.Errorf("%s is not a plain directory; remove it and run the setup again", dir)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	apply := func(path, sddl string) error {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			return err
		}
		owner, _, err := sd.Owner()
		if err != nil {
			return err
		}
		dacl, _, err := sd.DACL()
		if err != nil {
			return err
		}
		err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
			windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
			owner, nil, dacl, nil)
		if err != nil {
			return fmt.Errorf("secure %s: %w", path, err)
		}
		return nil
	}
	if err := apply(dir, dataDirSDDL); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if isReparsePoint(fi) {
			// os.Remove deletes the link, not its target.
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("remove link %s: %w", path, err)
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return apply(path, dataDirSDDL)
		}
		return apply(path, dataFileSDDL)
	})
}

func isReparsePoint(fi fs.FileInfo) bool {
	if fi.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
		return true
	}
	if a, ok := fi.Sys().(*syscall.Win32FileAttributeData); ok {
		return a.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0
	}
	return false
}

// checkTrustedExe refuses an executable outside %ProgramFiles%: the path
// comes from the configuration file, and the service runs it as LocalSystem.
func checkTrustedExe(path string) error {
	pf, err := windows.KnownFolderPath(windows.FOLDERID_ProgramFiles, 0)
	if err != nil {
		return err
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) || !strings.HasPrefix(strings.ToLower(clean), strings.ToLower(filepath.Clean(pf))+`\`) {
		return fmt.Errorf("%s is not below %s; refusing to run it", path, pf)
	}
	return nil
}
