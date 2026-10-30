package commands

import (
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"testing"
)

func TestLiteBleProfiles(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		enabled bool
		profile litepb.WendyComBleProfile
	}{
		{"normal", true, litepb.WendyComBleProfile_WENDY_COM_BLE_NORMAL},
		{"coded-s2", true, litepb.WendyComBleProfile_WENDY_COM_BLE_CODED_S2},
		{"coded-s8", true, litepb.WendyComBleProfile_WENDY_COM_BLE_CODED_S8},
		{"off", false, litepb.WendyComBleProfile_WENDY_COM_BLE_CODED_S8},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			root := newDeviceLocalMeshCmd()
			cmd, _, err := root.Find([]string{"configure"})
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Flags().Set("ble-mode", tc.mode); err != nil {
				t.Fatal(err)
			}
			c := &litepb.WendyComNetConfig{Enabled: true, BleProfile: litepb.WendyComBleProfile_WENDY_COM_BLE_CODED_S8, MaxLinks: 3}
			if err := updateLiteNetConfig(cmd, c); err != nil {
				t.Fatal(err)
			}
			if c.Ble != tc.enabled || c.BleProfile != tc.profile || !c.Enabled || c.MaxLinks != 3 {
				t.Fatalf("unexpected policy: %v", c)
			}
		})
	}
	for _, args := range [][]string{{"--ble-mode=coded-s2,coded-s8"}, {"--ble-mode=coded-s8", "--ble=true"}} {
		root := newDeviceLocalMeshCmd()
		cmd, _, _ := root.Find([]string{"configure"})
		if err := cmd.ParseFlags(args); err != nil {
			t.Fatal(err)
		}
		if err := updateLiteNetConfig(cmd, &litepb.WendyComNetConfig{}); err == nil {
			t.Fatalf("accepted conflicting/invalid selection %v", args)
		}
	}
}
