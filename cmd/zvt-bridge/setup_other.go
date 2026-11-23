//go:build !windows

package main

import (
	"fmt"
	"os"
)

func defaultInstallDir() string { return "/opt/zvt-bridge" }
func defaultLogPath() string    { return "/var/log/zvt-bridge/zvt-bridge.log" }

func runSetup(*setupConfig, *prompter) int {
	fmt.Fprintln(os.Stderr, "the setup wizard is only available on Windows; on Linux see linux/zvt-bridge.service")
	return 2
}

func runUninstall(*uninstallConfig, *prompter) int {
	fmt.Fprintln(os.Stderr, "uninstall is only available on Windows; on Linux disable the systemd unit")
	return 2
}
