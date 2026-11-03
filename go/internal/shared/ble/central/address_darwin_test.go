//go:build darwin

package central

import (
	"strings"
	"testing"
)

func TestDarwinL2CAPAddressUsesNativePeripheralUUID(t *testing.T) {
	for _, input := range []string{"ee48983f-300c-3a22-395b-290a76741ca1", "EE48983F-300C-3A22-395B-290A76741CA1"} {
		got, err := darwinPeripheralAddress(input)
		if err != nil || got != "EE48983F-300C-3A22-395B-290A76741CA1" {
			t.Fatalf("native address %q: %q %v", input, got, err)
		}
	}
	for _, input := range []string{"AA:BB:CC:DD:EE:FF", "", "not an address", "00000000-0000-0000-0000-000000000000"} {
		err := ValidateL2CAPAddress(input)
		if err == nil || !strings.Contains(err.Error(), "CoreBluetooth peripheral UUID") || !strings.Contains(err.Error(), "wendy-net discover") {
			t.Fatalf("non-native address %q did not get actionable rejection: %v", input, err)
		}
	}
}
