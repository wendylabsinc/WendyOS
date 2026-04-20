package commands

import "testing"

func TestWendyPlatform(t *testing.T) {
	cases := []struct {
		deviceType string
		want       string
	}{
		{"jetson-agx-orin", "nvidia-jetson"},
		{"jetson-orin-nano", "nvidia-jetson"},
		{"raspberrypi5", "generic"},
		{"unknown-device", "generic"},
		{"", "generic"},
	}
	for _, tc := range cases {
		t.Run(tc.deviceType, func(t *testing.T) {
			if got := wendyPlatform(tc.deviceType); got != tc.want {
				t.Fatalf("wendyPlatform(%q) = %q, want %q", tc.deviceType, got, tc.want)
			}
		})
	}
}
