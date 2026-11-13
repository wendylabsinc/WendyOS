package network

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestGetWiFiStatusAdapterPresence(t *testing.T) {
	for _, tc := range []struct {
		name, output, ssid string
		connected, wantErr bool
	}{
		{name: "VM with ethernet only", output: "ethernet:connected:Wired\nloopback:connected:lo", wantErr: true},
		{name: "no devices", wantErr: true},
		{name: "disconnected adapter", output: "wifi:disconnected:"},
		{name: "unavailable adapter", output: "wifi:unavailable:"},
		{name: "connected adapter", output: "wifi:connected:Home", connected: true, ssid: "Home"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nmcli")
			if err := os.WriteFile(path, []byte("#!/bin/sh\ncat <<'OUTPUT'\n"+tc.output+"\nOUTPUT\n"), 0700); err != nil {
				t.Fatal(err)
			}
			manager := &NMCLINetworkManager{nmcliPath: path}
			connected, ssid, err := manager.GetWiFiStatus(context.Background())
			if (err != nil) != tc.wantErr || connected != tc.connected || ssid != tc.ssid {
				t.Fatalf("got connected=%v ssid=%q err=%v", connected, ssid, err)
			}
		})
	}
}
