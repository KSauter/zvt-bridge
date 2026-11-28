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

// reattachDelay is the pause before the device is plugged in again after the
// host controller released it (Windows restarts the device when its COM name
// changes, and usbip-win2 may reconnect on its own in that time).
const reattachDelay = 5 * time.Second

// attachLoop asks the usbip-win2 host controller to plug our device in, plugs
// it in again whenever the controller releases it while the bridge runs, and
// unplugs it when ctx ends. It returns after the device has been unplugged, so
// the caller must wait for it before the process exits: a device left on the
// virtual hub stays there as "Unknown USB Device" (code 43) and blocks the
// next attach.
func attachLoop(ctx context.Context, cfg *runConfig, listenAddr string, released <-chan struct{}, attached func() bool) {
	if err := checkTrustedExe(cfg.USBIPExe); err != nil {
		log.Printf("usbip attach: usbip-exe: %v", err)
		return
	}
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		log.Printf("usbip attach: bad listen address %q: %v", listenAddr, err)
		return
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	var hubPort string
	defer func() { detach(cfg, hubPort) }()
	for {
		p, ok := attach(ctx, cfg, host, port)
		if !ok {
			return
		}
		hubPort = p
		// Wait until the controller lets go of the device or we stop.
		select {
		case <-ctx.Done():
			return
		case <-released:
		}
		log.Printf("usbip attach: the host controller released the device, plugging it in again in %s", reattachDelay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(reattachDelay):
		}
		if attached() {
			log.Printf("usbip attach: the host controller has reconnected on its own")
			select {
			case <-ctx.Done():
				return
			case <-released:
				continue
			}
		}
		// The old hub port is gone or stale; free it before plugging in again.
		detach(cfg, hubPort)
		hubPort = ""
	}
}

// attach runs "usbip attach" until it succeeds or ctx ends and returns the
// hub port.
func attach(ctx context.Context, cfg *runConfig, host, port string) (hubPort string, ok bool) {
	for {
		out, err := exec.CommandContext(ctx, cfg.USBIPExe, "--tcp-port", port, "attach", "--remote", host, "--bus-id", cfg.USBBusID, "--terse").CombinedOutput()
		text := strings.TrimSpace(string(out))
		if err == nil {
			if _, convErr := strconv.Atoi(text); convErr == nil {
				hubPort = text
			}
			log.Printf("usbip attach: device plugged into hub port %s", text)
			return hubPort, true
		}
		if ctx.Err() != nil {
			return "", false
		}
		log.Printf("usbip attach failed (%v): %s; retrying in 5s", err, text)
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(5 * time.Second):
		}
	}
}

func detach(cfg *runConfig, hubPort string) {
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
