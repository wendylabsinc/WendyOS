package commands

import (
	"strings"
	"testing"
)

func TestBluetoothPowerParsing(t *testing.T) {
	for _, s := range []string{"default", "maximum", "-15", "0", "20"} {
		if _, err := parseBluetoothPower(s); err != nil {
			t.Fatal(s, err)
		}
	}
	for _, s := range []string{"", "max", "20dBm", "21", "-128", "1.5"} {
		if _, err := parseBluetoothPower(s); err == nil {
			t.Fatal("accepted", s)
		}
	}
}

func TestBluetoothPowerRejectsInvalidInputBeforeConnecting(t *testing.T) {
	cmd := newBluetoothPowerCmd()
	cmd.SetArgs([]string{"--advertising", "nonsense"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "power must be") {
		t.Fatal(err)
	}
}
func TestMeshBLEModeRejectsInvalidInputBeforeConnecting(t *testing.T) {
	for _, args := range [][]string{{"configure", "--ble-mode="}, {"configure", "--ble-mode=wat"}, {"configure", "--ble-mode=normal", "--ble=true"}} {
		cmd := newDeviceLocalMeshCmd()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "ble-mode") {
			t.Fatal(args, err)
		}
	}
}
