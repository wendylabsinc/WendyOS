//go:build linux

package bleprovider

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// This exercises the board's actual Bluetooth socket API without connecting a
// peer, transmitting, changing the controller, or taking the agent's PSM.
func TestLEReceiveMTUNativeSocket(t *testing.T) {
	if os.Getenv("WENDY_BLE_SOCKET_TEST") != "1" {
		t.Skip("requires Linux Bluetooth socket support")
	}
	fd, err := newL2Socket()
	if err != nil {
		t.Fatal(err)
	}
	conn := newPacketConn(fd, l2addr{}, l2addr{})
	t.Cleanup(func() { _ = conn.Close() })
	if err := unix.Bind(fd, &unix.SockaddrL2{AddrType: btAddrLEPublic}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []int{2048, 8192, 65535} {
		if err := setL2CAPReceiveMTU(fd, want); err != nil {
			t.Fatalf("set LE receive MTU %d: %v", want, err)
		}
		if got := CoCReceiveMTU(conn); got != want {
			t.Fatalf("LE receive MTU readback=%d, want%d", got, want)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := CoCReceiveMTU(conn); got != 0 {
		t.Fatalf("closed receive MTU=%d", got)
	}
}
