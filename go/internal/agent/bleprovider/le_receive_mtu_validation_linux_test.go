//go:build linux

package bleprovider

import (
	"strings"
	"testing"
)

func TestLEReceiveMTURejectsInvalidAndUnsupportedValues(t *testing.T) {
	// Reject values before uint16 conversion, which would silently wrap them.
	for _, mtu := range []int{-1, 0, 22, 65536, 1 << 20} {
		if err := setL2CAPReceiveMTU(-1, mtu); err == nil || !strings.Contains(err.Error(), "invalid LE receive MTU") {
			t.Fatalf("MTU %d: %v", mtu, err)
		}
	}
	socket, _ := packetPair(t)
	if err := setL2CAPReceiveMTU(socket.fd, 65535); err == nil {
		t.Fatal("non-Bluetooth socket accepted LE tuning")
	}
	if got := CoCReceiveMTU(socket); got != 0 {
		t.Fatalf("unsupported receive MTU=%d", got)
	}
}
