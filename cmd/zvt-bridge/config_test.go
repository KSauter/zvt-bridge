package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestConfigFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "zvt-bridge.conf")
	_, want := newRunFlags("x")
	want.Terminal = "192.168.10.50:20008"
	want.USB = true
	want.USBSerial = "ZVT-7"
	want.USBIPExe = `C:\Program Files\USBip\usbip.exe`
	want.LogFile = `C:\ProgramData\zvt-bridge\zvt-bridge.log`
	want.AckTimeout = 3 * time.Second
	if err := writeConfigFile(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", *got, *want)
	}
	if err := got.validate(); err != nil {
		t.Error(err)
	}
	if a := got.args(); slices.Contains(a, "-serial") || !slices.Contains(a, "-usb") {
		t.Errorf("usb config produced args %q", a)
	}
}

func TestConfigFilePrecedenceAndErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.conf")
	os.WriteFile(path, []byte("\uFEFF# comment\n; another\n\nterminal = 10.0.0.1\r\nbaud = \"19200\"\nserial=COM3\n"), 0o644)
	fs, cfg := newRunFlags("x")
	if err := parseRunFlags(fs, cfg, []string{"-config", path, "-baud", "4800"}); err != nil {
		t.Fatal(err)
	}
	if cfg.Terminal != "10.0.0.1" || cfg.Serial != "COM3" || cfg.Baud != 4800 {
		t.Errorf("got %+v", *cfg)
	}
	if args := strings.Join(cfg.args(), " "); args != "-config "+path {
		t.Errorf("service args with config file: %q", args)
	}
	for name, body := range map[string]string{"unknown": "nope = 1\n", "syntax": "terminal\n", "value": "baud = abc\n"} {
		os.WriteFile(path, []byte(body), 0o644)
		fs, cfg := newRunFlags("x")
		if err := parseRunFlags(fs, cfg, []string{"-config", path}); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
