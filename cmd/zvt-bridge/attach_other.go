//go:build !windows

package main

import (
	"context"
	"log"
	"net"
)

const defaultUsbipExe = "usbip"

// attachLoop only prints how to attach on Linux; attaching needs root there.
func attachLoop(ctx context.Context, cfg *runConfig, listenAddr string, _ <-chan struct{}, _ func() bool) {
	host, port, _ := net.SplitHostPort(listenAddr)
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	log.Printf("to plug the device in on Linux: sudo modprobe vhci-hcd && sudo usbip --tcp-port %s attach -r %s -b %s", port, host, cfg.USBBusID)
	<-ctx.Done()
}
