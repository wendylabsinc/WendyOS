//go:build darwin

package commands

import (
	"os"
	"strings"
	"testing"
)

func TestPinnedBLEHardwareMACRejectedBeforeProfileOrBroker(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	cmd := newDeviceOperatorMeshConnectCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs([]string{"--transport", "ble", "--address", "AA:BB:CC:DD:EE:FF", "--router", "22222222-2222-4222-8222-222222222222"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "CoreBluetooth peripheral UUID") {
		t.Fatalf("unsupported Mac address did not get direct guidance: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("invalid address touched profile/broker: %v %v", entries, err)
	}
}
