//go:build !windows

package main

import (
	"fmt"
	"os"
)

func runningAsService() (bool, error) { return false, nil }

func runAsService(*runConfig) int {
	fmt.Fprintln(os.Stderr, "service mode is only available on Windows")
	return 1
}

func cmdService(cmd string, _ []string) int {
	fmt.Fprintf(os.Stderr, "command %q is only available on Windows; on Linux see linux/zvt-bridge.service (systemd)\n", cmd)
	return 2
}
