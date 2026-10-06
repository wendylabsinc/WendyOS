package timesync

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
	"golang.org/x/net/ipv4"
)

type multicastRead struct {
	packet []byte
	err    error
	read   chan struct{}
}

type fakeMulticastConn struct {
	mu          sync.Mutex
	joins       []net.Interface
	leaves      []net.Interface
	joinErrors  map[int]error
	leaveErr    error
	deadline    time.Time
	deadlineErr error
	packets     chan multicastRead
	reading     chan struct{}
	joined      chan net.Interface
	closed      chan struct{}
	closeOnce   sync.Once
}

func newFakeMulticastConn() *fakeMulticastConn {
	return &fakeMulticastConn{
		packets: make(chan multicastRead, 8),
		reading: make(chan struct{}, 1),
		joined:  make(chan net.Interface, 16),
		closed:  make(chan struct{}),
	}
}

func (c *fakeMulticastConn) JoinGroup(iface *net.Interface, _ net.Addr) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.joins = append(c.joins, *iface)
	if err := c.joinErrors[iface.Index]; err != nil {
		return err
	}
	c.joined <- *iface
	return nil
}

func (c *fakeMulticastConn) LeaveGroup(iface *net.Interface, _ net.Addr) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.leaves = append(c.leaves, *iface)
	return c.leaveErr
}

func (c *fakeMulticastConn) SetReadDeadline(deadline time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = deadline
	return c.deadlineErr
}

func (c *fakeMulticastConn) ReadFrom(buf []byte) (int, *ipv4.ControlMessage, net.Addr, error) {
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case c.reading <- struct{}{}:
	default:
	}
	select {
	case <-c.closed:
		return 0, nil, nil, net.ErrClosed
	case <-timer.C:
		return 0, nil, nil, &net.OpError{Op: "read", Net: "udp", Err: timeoutError{}}
	case packet := <-c.packets:
		if packet.read != nil {
			close(packet.read)
		}
		return copy(buf, packet.packet), nil, &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 5887}, packet.err
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "read deadline exceeded" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func (c *fakeMulticastConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func multicastInterface(index int, name string) net.Interface {
	return net.Interface{Index: index, Name: name, Flags: net.FlagUp | net.FlagMulticast}
}

func TestRefreshMulticastMemberships(t *testing.T) {
	ethernet := multicastInterface(2, "eth0")
	wifi := multicastInterface(3, "wlan0")
	usb := multicastInterface(4, "usb0")
	loopback := multicastInterface(1, "lo")
	loopback.Flags |= net.FlagLoopback
	down := multicastInterface(5, "down0")
	down.Flags &^= net.FlagUp
	unicast := multicastInterface(6, "tun0")
	unicast.Flags &^= net.FlagMulticast
	ifaces := []net.Interface{ethernet, wifi, loopback, down, unicast}
	var listErr error
	m := &Manager{multicastInterfaces: func() ([]net.Interface, error) { return ifaces, listErr }}
	conn := newFakeMulticastConn()
	conn.joinErrors = map[int]error{wifi.Index: errors.New("IPv4 not configured")}
	joined := make(map[int]net.Interface)
	group := &net.UDPAddr{IP: net.ParseIP(multicastGroup), Port: multicastPort}

	m.refreshMulticast(conn, group, joined)
	if !reflect.DeepEqual(joined, map[int]net.Interface{ethernet.Index: ethernet}) {
		t.Fatalf("memberships after partial failure: %v", joined)
	}
	if len(conn.joins) != 2 {
		t.Fatalf("join attempts = %v; want only Ethernet and WiFi", conn.joins)
	}

	// Retry the failed join, without rejoining an already working interface.
	delete(conn.joinErrors, wifi.Index)
	m.refreshMulticast(conn, group, joined)
	if len(conn.joins) != 3 || len(joined) != 2 {
		t.Fatalf("retry: joins = %v, memberships = %v", conn.joins, joined)
	}

	// A transient enumeration failure must not discard working subscriptions.
	listErr = errors.New("interface enumeration failed")
	m.refreshMulticast(conn, group, joined)
	if len(conn.leaves) != 0 || len(joined) != 2 {
		t.Fatalf("enumeration failure discarded memberships: %v", joined)
	}
	listErr = nil

	// A down interface is left; a new USB link is joined.
	wifi.Flags &^= net.FlagUp
	ifaces = []net.Interface{ethernet, wifi, usb}
	m.refreshMulticast(conn, group, joined)
	if len(conn.leaves) != 1 || conn.leaves[0].Name != "wlan0" || len(joined) != 2 || joined[usb.Index].Name != usb.Name {
		t.Fatalf("link change: leaves = %v, memberships = %v", conn.leaves, joined)
	}

	// Removal and reuse of an interface index must replace the old membership.
	replacement := multicastInterface(ethernet.Index, "eth1")
	ifaces = []net.Interface{replacement}
	conn.leaveErr = errors.New("interface already removed")
	m.refreshMulticast(conn, group, joined)
	if !reflect.DeepEqual(joined, map[int]net.Interface{replacement.Index: replacement}) || len(conn.leaves) != 3 {
		t.Fatalf("replacement: leaves = %v, memberships = %v", conn.leaves, joined)
	}
}

func TestRunMulticastRefreshesDuringTraffic(t *testing.T) {
	conn := newFakeMulticastConn()
	lists := 0
	m := &Manager{
		multicastListen: func() (multicastPacketConn, error) { return conn, nil },
		multicastInterfaces: func() ([]net.Interface, error) {
			lists++
			ifaces := []net.Interface{multicastInterface(2, "eth0")}
			if lists > 1 {
				ifaces = append(ifaces, multicastInterface(4, "usb0"))
			}
			return ifaces, nil
		},
		multicastInterval: 10 * time.Millisecond,
	}
	// Keep ReadFrom ready. Refreshing only on a read timeout would postpone
	// subscribing to the new interface indefinitely under sustained traffic.
	stopTraffic := make(chan struct{})
	trafficDone := make(chan struct{})
	packet := roughtime.Encode(roughtime.Datagram{MsgType: 0x42})
	conn.packets <- multicastRead{packet: packet}
	go func() {
		defer close(trafficDone)
		for {
			select {
			case conn.packets <- multicastRead{packet: packet}:
			case <-stopTraffic:
				return
			}
		}
	}()
	defer func() {
		close(stopTraffic)
		<-trafficDone
	}()
	startMulticastTest(t, m)
	waitMulticastJoin(t, conn, "eth0")
	waitMulticastJoin(t, conn, "usb0")
}

func startMulticastTest(t *testing.T, m *Manager) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.RunMulticast(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("multicast receiver did not stop on cancellation")
		}
	})
	return cancel
}

func waitMulticastJoin(t *testing.T, conn *fakeMulticastConn, name string) {
	t.Helper()
	select {
	case iface := <-conn.joined:
		if iface.Name != name {
			t.Fatalf("joined %s, want %s", iface.Name, name)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("receiver did not join %s", name)
	}
}

func testMulticastProof(t *testing.T) ([]byte, time.Time) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	original := Servers
	Servers = []roughtime.Server{{Name: "test", Address: "unused", PublicKey: pub}}
	t.Cleanup(func() { Servers = original })
	midpoint := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	return buildRelayPacket(t, 0, priv, uint64(midpoint.UnixMicro())), midpoint
}

func TestRunMulticastReceivesVerifiedProofAfterInterfaceAppears(t *testing.T) {
	proof, midpoint := testMulticastProof(t)
	conn := newFakeMulticastConn()
	var mu sync.Mutex
	var ifaces []net.Interface
	listed := make(chan struct{}, 1)
	applied := make(chan time.Time, 8)
	m := &Manager{
		multicastListen: func() (multicastPacketConn, error) { return conn, nil },
		multicastInterfaces: func() ([]net.Interface, error) {
			mu.Lock()
			defer mu.Unlock()
			select {
			case listed <- struct{}{}:
			default:
			}
			return ifaces, nil
		},
		multicastInterval: 10 * time.Millisecond,
		apply:             func(t time.Time) { applied <- t },
	}
	cancel := startMulticastTest(t, m)
	select {
	case <-listed:
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not enumerate interfaces")
	}
	mu.Lock()
	ifaces = []net.Interface{multicastInterface(4, "usb0")}
	mu.Unlock()
	// No packet arrives to wake the listener: its read deadline must refresh
	// memberships after the USB link becomes available.
	waitMulticastJoin(t, conn, "usb0")

	corrupt := append([]byte(nil), proof...)
	corrupt[len(corrupt)/2] ^= 0xff
	for _, packet := range [][]byte{
		{1, 2, 3},
		corrupt,
		roughtime.Encode(roughtime.Datagram{MsgType: 0x42}),
		proof,
	} {
		conn.packets <- multicastRead{packet: packet}
	}
	// Reading this barrier proves that all preceding packets were processed.
	// Otherwise cancellation after the first application could hide an invalid
	// proof being applied before the valid proof with the same signed midpoint.
	processed := make(chan struct{})
	conn.packets <- multicastRead{
		packet: roughtime.Encode(roughtime.Datagram{MsgType: 0x42}),
		read:   processed,
	}
	select {
	case <-processed:
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not process queued packets")
	}
	select {
	case got := <-applied:
		if !got.Equal(midpoint) {
			t.Fatalf("applied %v, want signed midpoint %v", got, midpoint)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("valid signed multicast proof was not applied")
	}
	cancel()
	select {
	case <-conn.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not close blocked reader")
	}
	select {
	case got := <-applied:
		t.Fatalf("invalid or unknown packet applied time %v", got)
	default:
	}
}

func TestRunMulticastCancellationClosesBlockedReader(t *testing.T) {
	conn := newFakeMulticastConn()
	m := &Manager{
		multicastListen: func() (multicastPacketConn, error) { return conn, nil },
		multicastInterfaces: func() ([]net.Interface, error) {
			return []net.Interface{multicastInterface(2, "eth0")}, nil
		},
	}
	cancel := startMulticastTest(t, m)
	select {
	case <-conn.reading:
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not enter ReadFrom")
	}
	// Use the production five-second deadline. Cancellation must close the
	// blocked socket, rather than wait for that deadline to expire.
	cancel()
	select {
	case <-conn.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation waited for the read deadline instead of closing the socket")
	}
}

func TestRunMulticastReopensFailedSocket(t *testing.T) {
	for _, failure := range []string{"read", "deadline"} {
		t.Run(failure, func(t *testing.T) {
			failed := newFakeMulticastConn()
			working := newFakeMulticastConn()
			if failure == "read" {
				failed.packets <- multicastRead{err: errors.New("broken socket")}
			} else {
				failed.deadlineErr = errors.New("broken socket")
			}
			opens, waits := 0, 0
			m := &Manager{
				multicastListen: func() (multicastPacketConn, error) {
					opens++
					switch opens {
					case 1:
						return nil, errors.New("bind failed")
					case 2:
						return failed, nil
					default:
						select {
						case <-failed.closed:
						default:
							t.Error("failed socket not closed before reopening")
						}
						return working, nil
					}
				},
				multicastInterfaces: func() ([]net.Interface, error) {
					return []net.Interface{multicastInterface(2, "eth0")}, nil
				},
				sleep: func(d time.Duration) <-chan time.Time {
					if d != multicastRefreshInterval {
						t.Errorf("reconnect delay = %v", d)
					}
					waits++
					ready := make(chan time.Time, 1)
					ready <- time.Now()
					return ready
				},
			}
			startMulticastTest(t, m)
			waitMulticastJoin(t, working, "eth0")
			if opens != 3 || waits != 2 {
				t.Fatalf("opens = %d, retry waits = %d; want 3 and 2", opens, waits)
			}
		})
	}
}
