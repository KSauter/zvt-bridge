//go:build windows

package main

import "testing"

func TestIsRepairTarget(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{`USB\VID_1209&PID_0001\ZVT-BRIDGE-1`, true},
		{`USB\VID_1209&PID_0001&MI_00\3&7E4E96E&0&0000`, true},
		{`usb\vid_1209&pid_0001&mi_01\3&7e4e96e&0&0001`, true},
		{`USB\VID_1209&PID_0002\OTHER`, false},
		{`USB\VID_046D&PID_C52B\5&1234`, false},
		// Unknown devices are only removed below the usbip-win2 controller.
		{`USB\VID_0000&PID_0000\2&3B7C36A2&0&1`, false},
	} {
		if got := isRepairTarget(tc.id, "1209", "0001", nil); got != tc.want {
			t.Errorf("isRepairTarget(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
