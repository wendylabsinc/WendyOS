package certs

import "testing"

func TestParseDevicePrincipal(t *testing.T) {
	base := "spiffe://wendy.sh/tenant/11111111-2222-4333-8444-555555555555/"
	for _, name := range []string{"lite-board", "fleet/board-1"} {
		if _, err := ParseDevicePrincipal(base + "device/" + name); err != nil {
			t.Fatal(err)
		}
	}
	for _, suffix := range []string{"operator/user", "service/asset-1", "device/", "device/a//b", "device/a/../b", "device/board\x1b[31m", "device/x?foo=1", "device/%61"} {
		if _, err := ParseDevicePrincipal(base + suffix); err == nil {
			t.Errorf("accepted %q", suffix)
		}
	}
}
