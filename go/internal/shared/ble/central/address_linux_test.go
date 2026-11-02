//go:build linux

package central

import "testing"

func TestLinuxL2CAPAddressAcceptsHardwareMAC(t *testing.T) {
	if err := ValidateL2CAPAddress("AA:BB:CC:DD:EE:FF"); err != nil {
		t.Fatal(err)
	}
}
