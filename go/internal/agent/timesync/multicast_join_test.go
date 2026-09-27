package timesync

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type recordingJoiner struct{ joined []string }

func (r *recordingJoiner) JoinGroup(iface *net.Interface, _ net.Addr) error {
	r.joined = append(r.joined, iface.Name)
	return nil
}

// A USB gadget link is never the default multicast interface, so it must be
// joined explicitly — and only once, even though the join runs every cycle.
func TestJoinMulticastInterfacesJoinsEveryMulticastLinkOnce(t *testing.T) {
	ifaces := []net.Interface{
		{Name: "lo", Flags: net.FlagUp | net.FlagLoopback | net.FlagMulticast},
		{Name: "wlan0", Flags: net.FlagUp | net.FlagMulticast},
		{Name: "usb0", Flags: net.FlagUp | net.FlagMulticast},
		{Name: "wwan0", Flags: net.FlagUp | net.FlagPointToPoint}, // no multicast
		{Name: "eth0", Flags: net.FlagMulticast},                  // down
	}
	orig := listMulticastInterfacesFn
	listMulticastInterfacesFn = func() ([]net.Interface, error) { return ifaces, nil }
	t.Cleanup(func() { listMulticastInterfacesFn = orig })

	group := &net.UDPAddr{IP: net.ParseIP(multicastGroup), Port: multicastPort}
	rec := &recordingJoiner{}
	joined := map[string]bool{}
	joinMulticastInterfaces(rec, group, joined, nil)
	joinMulticastInterfaces(rec, group, joined, nil)
	if len(rec.joined) != 2 || rec.joined[0] != "wlan0" || rec.joined[1] != "usb0" {
		t.Fatalf("joined %v, want [wlan0 usb0] exactly once", rec.joined)
	}

	// usb0 unplugged, then back: it must be joined again.
	ifaces = ifaces[:2]
	joinMulticastInterfaces(rec, group, joined, nil)
	ifaces = append(ifaces, net.Interface{Name: "usb0", Flags: net.FlagUp | net.FlagMulticast})
	joinMulticastInterfaces(rec, group, joined, nil)
	if len(rec.joined) != 3 || rec.joined[2] != "usb0" {
		t.Fatalf("joined %v, want usb0 re-joined after it returned", rec.joined)
	}
}

// The CLI sends proofs straight to fe80::5741:1 on USB links, so the UDP6
// listener must take unicast, not only its multicast group.
func TestListenUDP6ReceivesUnicastProof(t *testing.T) {
	want := time.Date(2026, 9, 27, 20, 0, 0, 0, time.UTC)
	origProcess := processPacketFn
	processPacketFn = func(pkt []byte) (time.Time, error) {
		if string(pkt) != "proof" {
			return time.Time{}, errors.New("unexpected packet")
		}
		return want, nil
	}
	t.Cleanup(func() { processPacketFn = origProcess })

	applied := make(chan time.Time, 1)
	m := &Manager{apply: func(tm time.Time) {
		select {
		case applied <- tm:
		default: // retries can deliver more than one proof
		}
	}}
	ctx, cancel := context.WithCancel(context.Background())
	exited := make(chan struct{})
	go func() { defer close(exited); m.listenUDP6(ctx) }()
	// Wait for the socket to close so a repeated run can bind the port again.
	t.Cleanup(func() { cancel(); <-exited })

	// Unconnected, so an ICMP port-unreachable from a send that beat the
	// listener's bind does not fail the next write.
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	defer conn.Close()
	dst := &net.UDPAddr{IP: net.IPv6loopback, Port: DatagramPort}

	// The listener may not be bound yet on the first send; retry briefly.
	deadline := time.After(3 * time.Second)
	for {
		_, _ = conn.WriteTo([]byte("proof"), dst)
		select {
		case got := <-applied:
			if !got.Equal(want) {
				t.Fatalf("applied %v, want %v", got, want)
			}
			return
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("unicast proof over UDP6 was never applied")
		}
	}
}
