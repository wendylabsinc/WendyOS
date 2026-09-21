//go:build linux

package rtps

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestParticipantRestrictsMulticastToJoinedInterfaces(t *testing.T) {
	// Verify the socket created by NewParticipant, so this also guards the
	// platform helper's wiring into the real discovery setup. No DDS traffic is
	// sent because the participant is never run.
	p, err := NewParticipant(Config{DomainID: 232, Interface: "lo"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	raw, err := p.mcast.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var value int
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		value, socketErr = unix.GetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MULTICAST_ALL)
	}); err != nil {
		t.Fatal(err)
	}
	if socketErr != nil {
		t.Fatal(socketErr)
	}
	if value != 0 {
		t.Fatalf("IP_MULTICAST_ALL = %d, want 0 so other discovery interfaces cannot leak cameras", value)
	}
}
