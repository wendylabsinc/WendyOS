package services

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
)

func usePreferredSource(t *testing.T, src string) {
	t.Helper()
	orig := preferredSourceFn
	t.Cleanup(func() { preferredSourceFn = orig })
	preferredSourceFn = func(*net.TCPAddr) (net.IP, error) { return net.ParseIP(src), nil }
}

func brokerPeer(local, remote string) *peer.Peer {
	return &peer.Peer{
		LocalAddr: &net.TCPAddr{IP: net.ParseIP(local), Port: 40000},
		Addr:      &net.TCPAddr{IP: net.ParseIP(remote), Port: 443},
	}
}

func TestBrokerPathMovedWhenTheKernelPrefersAnotherSource(t *testing.T) {
	usePreferredSource(t, "10.42.0.120")

	preferred, moved := brokerPathMoved(brokerPeer("192.168.2.216", "34.1.2.3"))

	if !moved || !preferred.Equal(net.ParseIP("10.42.0.120")) {
		t.Errorf("brokerPathMoved = %v, %v; want 10.42.0.120, true", preferred, moved)
	}
}

func TestBrokerPathStaysWhenTheSourceMatches(t *testing.T) {
	usePreferredSource(t, "192.168.2.216")

	if _, moved := brokerPathMoved(brokerPeer("192.168.2.216", "34.1.2.3")); moved {
		t.Error("brokerPathMoved = true for a stream already on the preferred source")
	}
}

// Without a usable answer the connection is left to its keepalive.
func TestBrokerPathStaysWhenItCannotTell(t *testing.T) {
	usePreferredSource(t, "10.42.0.120")

	for name, p := range map[string]*peer.Peer{
		"no peer":        nil,
		"non-TCP local":  {LocalAddr: &net.UnixAddr{Name: "@x"}, Addr: &net.TCPAddr{IP: net.ParseIP("34.1.2.3"), Port: 443}},
		"non-TCP remote": {LocalAddr: &net.TCPAddr{IP: net.ParseIP("192.168.2.216")}, Addr: &net.UnixAddr{Name: "@x"}},
		"missing local":  {Addr: &net.TCPAddr{IP: net.ParseIP("34.1.2.3"), Port: 443}},
		"missing remote": {LocalAddr: &net.TCPAddr{IP: net.ParseIP("192.168.2.216")}},
	} {
		if _, moved := brokerPathMoved(p); moved {
			t.Errorf("%s: brokerPathMoved = true", name)
		}
	}

	orig := preferredSourceFn
	t.Cleanup(func() { preferredSourceFn = orig })
	preferredSourceFn = func(*net.TCPAddr) (net.IP, error) {
		return nil, &net.OpError{Op: "dial", Err: net.UnknownNetworkError("no route")}
	}
	if _, moved := brokerPathMoved(brokerPeer("192.168.2.216", "34.1.2.3")); moved {
		t.Error("route lookup failed: brokerPathMoved = true")
	}
}

// An IPv4 address in its 16-byte form is the same source.
func TestBrokerPathStaysAcrossAddressForms(t *testing.T) {
	orig := preferredSourceFn
	t.Cleanup(func() { preferredSourceFn = orig })
	preferredSourceFn = func(*net.TCPAddr) (net.IP, error) { return net.IPv4(192, 168, 2, 216).To4(), nil }

	if _, moved := brokerPathMoved(brokerPeer("192.168.2.216", "34.1.2.3")); moved {
		t.Error("brokerPathMoved = true for the same IPv4 source in another form")
	}
}

func TestPreferredSourceAsksTheKernel(t *testing.T) {
	src, err := preferredSource(&net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443})
	if err != nil {
		t.Fatal(err)
	}
	if !src.IsLoopback() {
		t.Errorf("preferredSource(127.0.0.1) = %v, want a loopback source", src)
	}
}

func TestBrokerPathWatchRedialsOnceTheMoveHolds(t *testing.T) {
	var w brokerPathWatch

	if w.shouldRedial(true, false) {
		t.Fatal("re-dialled on the first sighting of a move")
	}
	if !w.shouldRedial(true, false) {
		t.Error("did not re-dial once the move held across two checks")
	}
}

// A link that flaps back before the next check never costs a reconnect.
func TestBrokerPathWatchForgetsAMoveThatReverts(t *testing.T) {
	var w brokerPathWatch

	w.shouldRedial(true, false)
	w.shouldRedial(false, false)

	if w.shouldRedial(true, false) {
		t.Error("re-dialled although the earlier move had reverted")
	}
}

// Closing the connection would cut every tunnel session riding on it.
func TestBrokerPathWatchWaitsForSessionsToEnd(t *testing.T) {
	var w brokerPathWatch

	w.shouldRedial(true, true)
	if w.shouldRedial(true, true) {
		t.Fatal("re-dialled under a live tunnel session")
	}
	if !w.shouldRedial(true, false) {
		t.Error("did not re-dial once the sessions ended")
	}
}

func TestBrokerBusy(t *testing.T) {
	for _, tc := range []struct {
		name           string
		sessions       int32
		queued         int
		sinceHeartbeat time.Duration
		want           bool
	}{
		{"idle", 0, 0, time.Minute, false},
		{"live session", 1, 0, time.Minute, true},
		{"queued dial request", 0, 1, time.Minute, true},
		// The broker may still be recording that heartbeat.
		{"heartbeat just sent", 0, 0, time.Second, true},
		{"heartbeat settled", 0, 0, brokerHeartbeatSettle, false},
		{"no heartbeat yet", 0, 0, time.Since(time.Time{}), false},
	} {
		if got := brokerBusy(tc.sessions, tc.queued, tc.sinceHeartbeat); got != tc.want {
			t.Errorf("%s: brokerBusy = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A wired port without internet must not cost the working connection, nor be
// probed on every check.
func TestBrokerPathWatchBacksOffAnUnreachablePath(t *testing.T) {
	var w brokerPathWatch
	checksUntilRedial := func() int {
		for n := 1; n <= 100; n++ {
			if w.shouldRedial(true, false) {
				return n
			}
		}
		return -1
	}

	first := checksUntilRedial()
	w.unreachable()
	second := checksUntilRedial()
	w.unreachable()
	third := checksUntilRedial()

	if first != 2 || second <= first || third <= second {
		t.Errorf("checks between probes = %d, %d, %d; want them to grow from 2", first, second, third)
	}
}

func TestBrokerPathWatchCapsTheBackOff(t *testing.T) {
	var w brokerPathWatch
	for i := 0; i < 20; i++ {
		w.unreachable()
	}

	checks := 0
	for !w.shouldRedial(true, false) {
		checks++
	}
	if checks > brokerPathMaxWait+2 {
		t.Errorf("waited %d checks after repeated failures, want at most %d", checks, brokerPathMaxWait+2)
	}
}

// Once the preference flips back, an earlier failure says nothing about the
// next move.
func TestBrokerPathWatchForgetsTheBackOffWhenThePathSettles(t *testing.T) {
	var w brokerPathWatch
	for i := 0; i < 5; i++ {
		w.unreachable()
	}

	w.shouldRedial(false, false)

	w.shouldRedial(true, false)
	if !w.shouldRedial(true, false) {
		t.Error("an old back-off delayed the re-dial for a new move")
	}
}

func TestBrokerPathReachableNeedsTheBrokerToAnswer(t *testing.T) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	if !brokerPathReachable(context.Background(), ln.Addr().String(), opts) {
		t.Error("brokerPathReachable = false for a serving broker")
	}

	// Accepts TCP and hangs up, as a portal in front of the broker would.
	portal, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = portal.Close() })
	go func() {
		for {
			c, err := portal.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if brokerPathReachable(context.Background(), portal.Addr().String(), opts) {
		t.Error("brokerPathReachable = true for an endpoint that only accepts TCP")
	}
}
