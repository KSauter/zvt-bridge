//go:build windows

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

func runningAsService() (bool, error) { return svc.IsWindowsService() }

func defaultLogPath() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = `C:\ProgramData`
	}
	return filepath.Join(pd, "zvt-bridge", "zvt-bridge.log")
}

type handler struct{ cfg *runConfig }

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- runBridge(ctx, h.cfg) }()
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				log.Println("service stopped")
				return false, 0
			}
		case err := <-done:
			s <- svc.Status{State: svc.StopPending}
			if err != nil {
				log.Printf("service exits with error: %v", err)
				return true, 1
			}
			return false, 0
		}
	}
}

func runAsService(cfg *runConfig) int {
	if cfg.LogFile == "" {
		cfg.LogFile = defaultLogPath()
	}
	closeLog, err := setupLogging(cfg.LogFile, false)
	if err != nil {
		if elog, e := eventlog.Open(serviceName); e == nil {
			elog.Error(1, err.Error())
			elog.Close()
		}
		return 1
	}
	defer closeLog()
	if elog, err := eventlog.Open(serviceName); err == nil {
		elog.Info(1, fmt.Sprintf("zvt-bridge %s starting: ECR %s, PT %s, log %s", version, cfg.Serial, cfg.addr(), cfg.LogFile))
		elog.Close()
	}
	if err := svc.Run(serviceName, &handler{cfg: cfg}); err != nil {
		log.Printf("service: %v", err)
		return 1
	}
	return 0
}

func cmdService(cmd string, args []string) int {
	switch cmd {
	case "install":
		return svcInstall(args)
	case "remove":
		return svcRemove()
	default:
		return svcControl(cmd)
	}
}

func svcInstall(args []string) int {
	fs, cfg := newRunFlags("install")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := cfg.validate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		usage()
		return 2
	}
	if cfg.LogFile == "" {
		cfg.LogFile = defaultLogPath()
	}
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.Abs(exe)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	m, err := mgr.Connect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "service control manager: %v (run as administrator)\n", err)
		return 1
	}
	defer m.Disconnect()
	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		fmt.Fprintf(os.Stderr, "service %s already exists, run 'zvt-bridge remove' first\n", serviceName)
		return 1
	}
	svcArgs := append([]string{"run"}, cfg.args()...)
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: "ZVT Bridge (serial ECR ↔ TCP payment terminal)",
		Description: "Translates the ZVT ECR protocol between a serial port and a TCP payment terminal.",
		StartType:   mgr.StartAutomatic,
	}, svcArgs...)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create service: %v\n", err)
		return 1
	}
	defer s.Close()
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, 86400); err != nil {
		fmt.Fprintf(os.Stderr, "warning: recovery actions not set: %v\n", err)
	}
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil && !strings.Contains(err.Error(), "exists") {
		fmt.Fprintf(os.Stderr, "warning: event log source not registered: %v\n", err)
	}
	os.MkdirAll(filepath.Dir(cfg.LogFile), 0o755)
	fmt.Printf("service %s installed:\n  %s %s\n", serviceName, exe, strings.Join(svcArgs, " "))
	if err := s.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start service: %v\n", err)
		return 1
	}
	fmt.Println("service started; log:", cfg.LogFile)
	return 0
}

func svcRemove() int {
	m, err := mgr.Connect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "service control manager: %v (run as administrator)\n", err)
		return 1
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "service %s is not installed\n", serviceName)
		return 1
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		if _, err := s.Control(svc.Stop); err == nil {
			waitState(s, svc.Stopped, 30*time.Second)
		}
	}
	if err := s.Delete(); err != nil {
		fmt.Fprintf(os.Stderr, "delete service: %v\n", err)
		return 1
	}
	eventlog.Remove(serviceName)
	fmt.Printf("service %s removed\n", serviceName)
	return 0
}

func svcControl(cmd string) int {
	m, err := mgr.Connect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "service control manager: %v (run as administrator)\n", err)
		return 1
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "service %s is not installed\n", serviceName)
		return 1
	}
	defer s.Close()
	switch cmd {
	case "start":
		if err := s.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "start service: %v\n", err)
			return 1
		}
		waitState(s, svc.Running, 30*time.Second)
	case "stop":
		if _, err := s.Control(svc.Stop); err != nil {
			fmt.Fprintf(os.Stderr, "stop service: %v\n", err)
			return 1
		}
		waitState(s, svc.Stopped, 30*time.Second)
	}
	st, err := s.Query()
	if err != nil {
		fmt.Fprintf(os.Stderr, "status: %v\n", err)
		return 1
	}
	cfg, _ := s.Config()
	fmt.Printf("service %s: %s\n", serviceName, stateName(st.State))
	if cfg.BinaryPathName != "" {
		fmt.Printf("command: %s\n", cfg.BinaryPathName)
	}
	return 0
}

func waitState(s *mgr.Service, want svc.State, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := s.Query(); err == nil && st.State == want {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.Paused:
		return "paused"
	}
	return fmt.Sprintf("state %d", s)
}
