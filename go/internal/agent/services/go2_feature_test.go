package services

import (
	"slices"
	"testing"
)

func TestGo2AgentFeatureOnlyAdvertisedByLinuxWendyOSVMs(t *testing.T) {
	for _, tc := range []struct {
		os, device string
		want       bool
	}{
		{"linux", "vm-arm64", true},
		{"linux", "jetson-orin-nano", false},
		{"linux", "unitree-go2", false},
		{"linux", "unitree-g1", false},
		{"linux", "vm-arm64-untrusted", false},
		{"linux", "", false},
		{"darwin", "vm-arm64", false},
		{"linux", "vm-x86-64", true},
		{"linux", "vm-x86-64-untrusted", false},
		{"linux", "generic-x86-64", false},
		{"linux", "x86_64", false},
		{"darwin", "vm-x86-64", false},
	} {
		got := appendGo2AgentFeature([]string{"gpu"}, tc.os, tc.device)
		if slices.Contains(got, "go2-virtual-robot") != tc.want ||
			slices.Contains(got, "g1-virtual-robot") != tc.want || slices.Contains(got, "so101-virtual-robot") != tc.want || slices.Contains(got, "rosmaster-r2-virtual-robot") != tc.want || !slices.Contains(got, "gpu") {
			t.Fatalf("features for %s/%s: %v", tc.os, tc.device, got)
		}
	}
}
