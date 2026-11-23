package main

import "testing"

func TestTerminalAddr(t *testing.T) {
	good := map[string]string{
		"192.168.10.50":       "192.168.10.50:20007",
		" pt.local ":          "pt.local:20007",
		"192.168.10.50:20008": "192.168.10.50:20008",
		"[fe80::1]:20007":     "[fe80::1]:20007",
	}
	for in, want := range good {
		if got, err := terminalAddr(in); err != nil || got != want {
			t.Errorf("terminalAddr(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "host:", "host:abc", "host:70000", "[::1]", "a b", "http://x"} {
		if got, err := terminalAddr(in); err == nil {
			t.Errorf("terminalAddr(%q) = %q, want error", in, got)
		}
	}
}

func TestParseComName(t *testing.T) {
	for in, want := range map[string]string{"COM7": "COM7", "com12": "COM12", "7": "COM7", " COM256 ": "COM256"} {
		name, _, err := parseComName(in)
		if err != nil || name != want {
			t.Errorf("parseComName(%q) = %q, %v; want %q", in, name, err, want)
		}
	}
	for _, in := range []string{"", "COM0", "COM257", "COM", "LPT1", "COM 7", "7a"} {
		if name, _, err := parseComName(in); err == nil {
			t.Errorf("parseComName(%q) = %q, want error", in, name)
		}
	}
	if !isAutoCom("auto") || !isAutoCom("") || isAutoCom("COM7") {
		t.Error("isAutoCom")
	}
}
