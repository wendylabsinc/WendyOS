package commands

import "testing"

func TestWendyPlatform(t *testing.T) {
	cases := []struct {
		name       string
		deviceType string
		gpuVendor  string
		jetpack    string
		want       string
	}{
		// Canonical single-token Jetson identifiers (back-compat).
		{"jetson-orin-nano canonical", "jetson-orin-nano", "nvidia", "6.1", "nvidia-jetson"},
		{"jetson-agx-orin canonical", "jetson-agx-orin", "nvidia", "6.1", "nvidia-jetson"},

		// Real WendyOS file format on Orin Nano NVMe — multi-line BOARD/MACHINE.
		{
			"orin nano nvme BOARD/MACHINE blob",
			"BOARD=jetson-orin-nano-nvme\nMACHINE=jetson-orin-nano-devkit-nvme-wendyos",
			"nvidia", "6.1",
			"nvidia-jetson",
		},

		// Future Jetson SKU recognized by prefix even without an entry.
		{"future jetson by prefix", "jetson-thor", "nvidia", "7.0", "nvidia-jetson"},
		{"future jetson via BOARD blob", "BOARD=jetson-future-sku\nMACHINE=irrelevant", "nvidia", "7.0", "nvidia-jetson"},

		// JetPack present but board name unrecognized — still treat as Jetson.
		{"jetpack fallback overrides board mismatch", "weirdo-board", "nvidia", "6.1", "nvidia-jetson"},

		// Non-Jetson NVIDIA: DGX Spark / x86_64 workstation running plain
		// Ubuntu with wendy-agent. /etc/wendyos/device-type may be missing.
		{"empty device type with nvidia GPU", "", "nvidia", "", "nvidia-cuda"},
		{"vendor case-insensitive", "", "Nvidia", "", "nvidia-cuda"},
		{"non-jetson nvidia with cuda", "dgx-spark", "nvidia", "", "nvidia-cuda"},

		// Raspberry Pi via WendyOS BOARD blob — no GPU vendor reported.
		{"pi5 BOARD blob", "BOARD=raspberry-pi-5\nMACHINE=raspberrypi5-wendyos", "", "", "generic"},
		{"raspberrypi5 canonical", "raspberrypi5", "", "", "generic"},

		// Truly unknown / generic.
		{"unknown device no gpu", "unknown-device", "", "", "generic"},
		{"all empty", "", "", "", "generic"},

		// Vendor we don't classify yet (AMD/Intel) falls to generic.
		{"amd gpu unsupported tier", "", "amd", "", "generic"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wendyPlatform(tc.deviceType, tc.gpuVendor, tc.jetpack)
			if got != tc.want {
				t.Fatalf("wendyPlatform(%q, %q, %q) = %q, want %q",
					tc.deviceType, tc.gpuVendor, tc.jetpack, got, tc.want)
			}
		})
	}
}

func TestParseDeviceBoard(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"plain token", "jetson-orin-nano", "jetson-orin-nano"},
		{"plain token uppercased", "Jetson-Orin-Nano", "jetson-orin-nano"},
		{
			"BOARD/MACHINE blob",
			"BOARD=jetson-orin-nano-nvme\nMACHINE=jetson-orin-nano-devkit-nvme-wendyos",
			"jetson-orin-nano-nvme",
		},
		{"BOARD only", "BOARD=raspberry-pi-5", "raspberry-pi-5"},
		{"MACHINE first", "MACHINE=foo\nBOARD=bar", "bar"},
		{"surrounding whitespace", "  BOARD=jetson-thor  \n  MACHINE=baz  ", "jetson-thor"},
		{"no BOARD line", "MACHINE=foo", "machine=foo"}, // no BOARD= → fall back to lowercased input
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDeviceBoard(tc.in); got != tc.want {
				t.Fatalf("parseDeviceBoard(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
