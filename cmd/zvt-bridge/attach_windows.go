//go:build windows

package main

import (
	"context"
	"log"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const defaultUsbipExe = `C:\Program Files\USBip\usbip.exe`

// attachLoop asks the usbip-win2 host controller to plug our device in and
// unplugs it again when ctx ends. usbip.exe keeps retrying the connection in
// the driver itself once the first call succeeded.
func attachLoop(ctx context.Context, cfg *runConfig, listenAddr string) {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		log.Printf("usbip attach: bad listen address %q: %v", listenAddr, err)
		return
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	var hubPort string
	for {
		out, err := exec.CommandContext(ctx, cfg.USBIPExe, "--tcp-port", port, "attach", "--remote", host, "--bus-id", cfg.USBBusID, "--terse").CombinedOutput()
		text := strings.TrimSpace(string(out))
		if err == nil {
			if _, convErr := strconv.Atoi(text); convErr == nil {
				hubPort = text
			}
			log.Printf("usbip attach: device plugged into hub port %s", text)
			break
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("usbip attach failed (%v): %s; retrying in 5s", err, text)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	<-ctx.Done()
	if hubPort == "" {
		return
	}
	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(dctx, cfg.USBIPExe, "detach", "--port", hubPort).CombinedOutput(); err != nil {
		log.Printf("usbip detach: %v: %s", err, strings.TrimSpace(string(out)))
	} else {
		log.Printf("usbip detach: device unplugged from hub port %s", hubPort)
	}
}
