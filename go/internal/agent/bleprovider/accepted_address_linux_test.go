//go:build linux

package bleprovider

import (
	"golang.org/x/sys/unix"
	"testing"
)

func TestAcceptedSockaddrL2CanonicalAddress(t *testing.T) {
	// Kernel bdaddr_t for Pi5 2C:CF:67:C6:70:AE, matching the actual accepted
	// v0.47.0 SockaddrL2 decoder (raw.Bdaddr copied without reversal).
	raw := unix.RawSockaddrL2{Family: unix.AF_BLUETOOTH, Bdaddr: [6]byte{0xAE, 0x70, 0xC6, 0x67, 0xCF, 0x2C}, Psm: 0x009d}
	for _, kind := range []uint8{btAddrLEPublic, btAddrLERandom} {
		raw.Bdaddr_type = kind
		accepted := &unix.SockaddrL2{Addr: raw.Bdaddr, AddrType: raw.Bdaddr_type, PSM: raw.Psm}
		got := acceptedL2Addr(accepted, 0x81)
		typeName := "public"
		if kind == btAddrLERandom {
			typeName = "random"
		}
		expected, canonical, err := parseAddress("2C:CF:67:C6:70:AE", typeName, 0x81)
		if err != nil {
			t.Fatal(err)
		}
		if got != expected {
			t.Fatalf("accepted address %+v, expected %+v", got, expected)
		}
		if got.String() != "2C:CF:67:C6:70:AE/"+typeName+"/0081" {
			t.Fatal("noncanonical accepted address", got)
		}
		if canonical != [6]byte{0x2C, 0xCF, 0x67, 0xC6, 0x70, 0xAE} {
			t.Fatal("outbound byte order changed")
		}
		if accepted.Addr != raw.Bdaddr {
			t.Fatal("normalization mutates syscall address")
		}
	}
}
