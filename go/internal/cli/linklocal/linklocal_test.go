package linklocal

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// ipNet builds the net.Addr an interface reports for ip/prefix.
func ipNet(t *testing.T, cidr string) net.Addr {
	t.Helper()
	ip, n, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	n.IP = ip
	return n
}

// linkA holds the preferred 169.254/16 route; the device is on linkB.
var (
	linkA = link{ifi: net.Interface{Index: 7, Name: "enxaabbccddee01"}, ip: net.ParseIP("169.254.68.104")}
	linkB = link{ifi: net.Interface{Index: 9, Name: "enxaabbccddee02"}, ip: net.ParseIP("169.254.39.227")}
)

const deviceAddr = "169.254.198.132:50051"

// stubLinks swaps the host's links for the test's lifetime; the routing table
// sends 169.254/16 out the first of them unless the test says otherwise.
func stubLinks(t *testing.T, links []link) {
	t.Helper()
	origLinks, origRouted := linksFn, routedSourceFn
	t.Cleanup(func() { linksFn, routedSourceFn = origLinks, origRouted })
	linksFn = func() []link { return links }
	routedSourceFn = func(string) net.IP {
		if len(links) == 0 {
			return nil
		}
		return links[0].ip
	}
}

// stubSeams swaps every package seam for the test's lifetime.
func stubSeams(t *testing.T, links []link, plain func(context.Context, string) (net.Conn, error), onLink func(context.Context, link, string) (net.Conn, error)) {
	t.Helper()
	stubLinks(t, links)
	origPlain, origOnLink := plainDialFn, linkDialFn
	t.Cleanup(func() { plainDialFn, linkDialFn = origPlain, origOnLink })
	plainDialFn = plain
	linkDialFn = onLink
}

func pipeConn(t *testing.T) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return a
}

func dialErr(errno syscall.Errno) error {
	return &net.OpError{Op: "dial", Net: "tcp4", Err: errno}
}

func failPlain(t *testing.T) func(context.Context, string) (net.Conn, error) {
	return func(context.Context, string) (net.Conn, error) {
		t.Error("plain dial used; want an interface-bound dial")
		return nil, errors.New("unexpected plain dial")
	}
}

func failOnLink(t *testing.T) func(context.Context, link, string) (net.Conn, error) {
	return func(context.Context, link, string) (net.Conn, error) {
		t.Error("interface-bound dial used; want a plain dial")
		return nil, errors.New("unexpected bound dial")
	}
}

func TestLinkLocalLinksFrom(t *testing.T) {
	addrs := map[string][]net.Addr{
		"lo":              {ipNet(t, "127.0.0.1/8")},
		"wlp0s20f3":       {ipNet(t, "192.168.2.10/24"), ipNet(t, "fe80::1/64")},
		"enxaabbccddee01": {ipNet(t, "fe80::b5b5/64"), ipNet(t, "169.254.68.104/16")},
		"enxaabbccddee02": {ipNet(t, "169.254.39.227/16")},
		"enxaabbccddee03": {ipNet(t, "169.254.1.2/16")},
		"nodelocaldns":    {ipNet(t, "169.254.20.10/32")},
	}
	ifaces := []net.Interface{
		{Index: 1, Name: "lo", Flags: net.FlagUp | net.FlagLoopback},
		{Index: 3, Name: "wlp0s20f3", Flags: net.FlagUp},
		{Index: 7, Name: "enxaabbccddee01", Flags: net.FlagUp},
		{Index: 9, Name: "enxaabbccddee02", Flags: net.FlagUp},
		{Index: 11, Name: "enxaabbccddee03"},                 // down
		{Index: 12, Name: "nodelocaldns", Flags: net.FlagUp}, // a /32 host address, not a link
	}

	got := linkLocalLinksFrom(ifaces, func(ifi *net.Interface) ([]net.Addr, error) { return addrs[ifi.Name], nil })

	want := []link{linkA, linkB}
	if len(got) != len(want) {
		t.Fatalf("links = %v, want %v", got, want)
	}
	for i := range want {
		if got[i].ifi.Name != want[i].ifi.Name || got[i].ifi.Index != want[i].ifi.Index || !got[i].ip.Equal(want[i].ip) {
			t.Errorf("links[%d] = {%s %d %s}, want {%s %d %s}", i,
				got[i].ifi.Name, got[i].ifi.Index, got[i].ip, want[i].ifi.Name, want[i].ifi.Index, want[i].ip)
		}
	}
}

func TestDialRoutableAddressUsesPlainDial(t *testing.T) {
	want := pipeConn(t)
	stubSeams(t, []link{linkA, linkB},
		func(_ context.Context, addr string) (net.Conn, error) {
			if addr != "192.168.2.239:50051" {
				t.Errorf("plain dial addr = %q", addr)
			}
			return want, nil
		},
		failOnLink(t))

	got, err := Dial(context.Background(), "192.168.2.239:50051")
	if err != nil || got != want {
		t.Fatalf("Dial = %v, %v; want the plain connection", got, err)
	}
}

func TestDialSingleLinkUsesPlainDial(t *testing.T) {
	want := pipeConn(t)
	stubSeams(t, []link{linkB},
		func(context.Context, string) (net.Conn, error) { return want, nil },
		failOnLink(t))

	got, err := Dial(context.Background(), deviceAddr)
	if err != nil || got != want {
		t.Fatalf("Dial = %v, %v; want the plain connection", got, err)
	}
}

// macOS routes 169.254/16 out its primary interface even when only a USB link
// carries a link-local address, so a lone link is pinned too.
func TestDialPinsALinkTheRoutingTableBypasses(t *testing.T) {
	want := pipeConn(t)
	stubSeams(t, []link{linkB},
		func(context.Context, string) (net.Conn, error) { return nil, dialErr(syscall.EHOSTUNREACH) },
		func(context.Context, link, string) (net.Conn, error) { return want, nil })
	routedSourceFn = func(string) net.IP { return net.ParseIP("192.168.2.10") }

	got, err := Dial(context.Background(), deviceAddr)
	if err != nil || got != want {
		t.Fatalf("Dial = %v, %v; want the pinned connection", got, err)
	}
}

// Every link is tried; the one the device is on wins.
func TestDialPicksTheLinkTheDeviceIsOn(t *testing.T) {
	want := pipeConn(t)
	stubSeams(t, []link{linkA, linkB}, failPlain(t),
		func(ctx context.Context, l link, addr string) (net.Conn, error) {
			if addr != deviceAddr {
				t.Errorf("bound dial addr = %q", addr)
			}
			if l.ifi.Name == linkB.ifi.Name {
				return want, nil
			}
			<-ctx.Done() // ARP on linkA never resolves
			return nil, ctx.Err()
		})

	got, err := Dial(context.Background(), deviceAddr)
	if err != nil || got != want {
		t.Fatalf("Dial = %v, %v; want linkB's connection", got, err)
	}
}

func TestDialClosesConnectionsFromLosingLinks(t *testing.T) {
	winner := pipeConn(t)
	loser := newCloseRecorder(pipeConn(t))
	stubSeams(t, []link{linkA, linkB}, failPlain(t),
		func(_ context.Context, l link, _ string) (net.Conn, error) {
			if l.ifi.Name == linkB.ifi.Name {
				return winner, nil
			}
			time.Sleep(20 * time.Millisecond)
			return loser, nil
		})

	got, err := Dial(context.Background(), deviceAddr)
	if err != nil || got != winner {
		t.Fatalf("Dial = %v, %v; want the first connection", got, err)
	}
	if !loser.waitClosed(time.Second) {
		t.Fatal("the losing link's connection was never closed")
	}
}

// A refusal proves the device is on that link, so Dial must not wait out ARP
// on the others: port probes would otherwise cost seconds each.
func TestDialReturnsARefusalWithoutWaitingForOtherLinks(t *testing.T) {
	stubSeams(t, []link{linkA, linkB}, failPlain(t),
		func(ctx context.Context, l link, _ string) (net.Conn, error) {
			if l.ifi.Name == linkB.ifi.Name {
				return nil, dialErr(syscall.ECONNREFUSED)
			}
			<-ctx.Done()
			return nil, ctx.Err()
		})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	conn, err := Dial(ctx, deviceAddr)
	if conn != nil || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("Dial = %v, %v; want ECONNREFUSED", conn, err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Dial took %v; want it to return on the refusal", elapsed)
	}
}

// A link without the device reports no route (EHOSTDOWN on darwin), so any
// other answer is the one to report, whatever order the links answer in.
func TestDialReportsAnAnswerOverNoRoute(t *testing.T) {
	for _, noRoute := range []syscall.Errno{syscall.EHOSTUNREACH, syscall.EHOSTDOWN} {
		t.Run(noRoute.Error(), func(t *testing.T) {
			stubSeams(t, []link{linkA, linkB}, failPlain(t),
				func(_ context.Context, l link, _ string) (net.Conn, error) {
					if l.ifi.Name == linkB.ifi.Name {
						time.Sleep(20 * time.Millisecond)
						return nil, dialErr(syscall.ETIMEDOUT)
					}
					return nil, dialErr(noRoute)
				})

			conn, err := Dial(context.Background(), deviceAddr)
			if conn != nil || !errors.Is(err, syscall.ETIMEDOUT) {
				t.Fatalf("Dial = %v, %v; want ETIMEDOUT", conn, err)
			}
		})
	}
}

// The routing table sends the address out one of the links that just failed,
// so asking it again would only wait out ARP a second time.
func TestDialDoesNotRetryTheRoutedLinkWhenNoLinkAnswers(t *testing.T) {
	stubSeams(t, []link{linkA, linkB}, failPlain(t),
		func(context.Context, link, string) (net.Conn, error) { return nil, dialErr(syscall.EHOSTUNREACH) })

	conn, err := Dial(context.Background(), deviceAddr)
	if conn != nil || !errors.Is(err, syscall.EHOSTUNREACH) {
		t.Fatalf("Dial = %v, %v; want EHOSTUNREACH", conn, err)
	}
}

// A route on an interface without a link-local address is one more way to the
// device, raced with the links rather than tried after them.
func TestDialRacesTheRoutingTableWhenItAvoidsTheLinks(t *testing.T) {
	want := pipeConn(t)
	stubSeams(t, []link{linkA, linkB},
		func(context.Context, string) (net.Conn, error) { return want, nil },
		func(ctx context.Context, _ link, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	routedSourceFn = func(string) net.IP { return net.ParseIP("192.168.2.10") }
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	got, err := Dial(ctx, deviceAddr)
	if err != nil || got != want || time.Since(start) > time.Second {
		t.Fatalf("Dial = %v, %v after %v; want the routed connection at once", got, err, time.Since(start))
	}
}

// The routed link could not be pinned (an old kernel, say), so the routing
// table has not tried the device's likeliest link yet.
func TestDialFallsBackToPlainDialWhenTheRoutedLinkCannotBeBound(t *testing.T) {
	for _, other := range []syscall.Errno{syscall.EHOSTUNREACH, syscall.ETIMEDOUT} {
		t.Run(other.Error(), func(t *testing.T) {
			want := pipeConn(t)
			stubSeams(t, []link{linkA, linkB},
				func(context.Context, string) (net.Conn, error) { return want, nil },
				func(_ context.Context, l link, _ string) (net.Conn, error) {
					if l.ifi.Name == linkA.ifi.Name {
						return nil, &bindError{err: syscall.EPERM}
					}
					return nil, dialErr(other)
				})

			got, err := Dial(context.Background(), deviceAddr)
			if err != nil || got != want {
				t.Fatalf("Dial = %v, %v; want the plain fallback connection", got, err)
			}
		})
	}
}

// Another link vanishing between listing and dialing says nothing new about
// the routed link, which already answered no route.
func TestDialDoesNotRetryTheRoutedLinkWhenAnotherCannotBeBound(t *testing.T) {
	stubSeams(t, []link{linkA, linkB}, failPlain(t),
		func(_ context.Context, l link, _ string) (net.Conn, error) {
			if l.ifi.Name == linkB.ifi.Name {
				return nil, &bindError{err: syscall.ENODEV}
			}
			return nil, dialErr(syscall.EHOSTUNREACH)
		})

	conn, err := Dial(context.Background(), deviceAddr)
	if conn != nil || !errors.Is(err, syscall.EHOSTUNREACH) {
		t.Fatalf("Dial = %v, %v; want EHOSTUNREACH", conn, err)
	}
}

// When the routing table's path is raced, an unpinnable link must not make
// Dial try that path a second time.
func TestDialDialsTheRoutingTablesPathOnce(t *testing.T) {
	var plainDials atomic.Int32
	stubSeams(t, []link{linkA, linkB},
		func(context.Context, string) (net.Conn, error) {
			plainDials.Add(1)
			return nil, dialErr(syscall.EHOSTUNREACH)
		},
		func(_ context.Context, l link, _ string) (net.Conn, error) {
			if l.ifi.Name == linkA.ifi.Name {
				return nil, &bindError{err: syscall.ENODEV}
			}
			return nil, dialErr(syscall.EHOSTUNREACH)
		})
	routedSourceFn = func(string) net.IP { return net.ParseIP("192.168.2.10") }

	_, _ = Dial(context.Background(), deviceAddr)
	if n := plainDials.Load(); n != 1 {
		t.Fatalf("routing table's path dialed %d times, want 1", n)
	}
}

// A routing-table connection that arrives after a link won must be closed too.
func TestDialClosesTheRoutingTablesLateConnection(t *testing.T) {
	winner := pipeConn(t)
	late := newCloseRecorder(pipeConn(t))
	stubSeams(t, []link{linkA, linkB},
		func(context.Context, string) (net.Conn, error) {
			time.Sleep(20 * time.Millisecond)
			return late, nil
		},
		func(ctx context.Context, l link, _ string) (net.Conn, error) {
			if l.ifi.Name == linkB.ifi.Name {
				return winner, nil
			}
			return nil, dialErr(syscall.EHOSTUNREACH)
		})
	routedSourceFn = func(string) net.IP { return net.ParseIP("192.168.2.10") }

	if got, err := Dial(context.Background(), deviceAddr); err != nil || got != winner {
		t.Fatalf("Dial = %v, %v; want linkB's connection", got, err)
	}
	if !late.waitClosed(time.Second) {
		t.Fatal("the routing table's late connection was never closed")
	}
}

func TestDialHonorsContextCancellation(t *testing.T) {
	stubSeams(t, []link{linkA, linkB}, failPlain(t),
		func(ctx context.Context, _ link, _ string) (net.Conn, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	conn, err := Dial(ctx, deviceAddr)
	if conn != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial = %v, %v; want context.DeadlineExceeded", conn, err)
	}
}

func TestSourceIPUsesTheLinkThatReachesTheDevice(t *testing.T) {
	stubSeams(t, []link{linkA, linkB}, failPlain(t),
		func(ctx context.Context, l link, _ string) (net.Conn, error) {
			if l.ifi.Name != linkB.ifi.Name {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return pipeConn(t), nil
		})

	ip, ok := SourceIP(context.Background(), "169.254.198.132", 50051)
	if !ok || ip != linkB.ip.String() {
		t.Fatalf("SourceIP = %q, %v; want %s", ip, ok, linkB.ip)
	}
}

// An enrolled agent refuses its plaintext port, which already names the link.
func TestSourceIPIdentifiesTheLinkFromARefusal(t *testing.T) {
	stubSeams(t, []link{linkA, linkB}, failPlain(t),
		func(_ context.Context, l link, addr string) (net.Conn, error) {
			if addr != "169.254.198.132:50051" {
				t.Errorf("dialed %s; want only the first port", addr)
			}
			if l.ifi.Name != linkB.ifi.Name {
				return nil, dialErr(syscall.EHOSTUNREACH)
			}
			return nil, dialErr(syscall.ECONNREFUSED)
		})

	ip, ok := SourceIP(context.Background(), "169.254.198.132", 50051, 50052)
	if !ok || ip != linkB.ip.String() {
		t.Fatalf("SourceIP = %q, %v; want %s", ip, ok, linkB.ip)
	}
}

// When the routing table already uses the device's link, nothing is dialed.
func TestSourceIPLeavesTheRoutingTableAloneWhenItIsRight(t *testing.T) {
	for _, tc := range []struct {
		name  string
		links []link
		host  string
	}{
		{"single routed link", []link{linkB}, "169.254.198.132"},
		{"routable host", []link{linkA, linkB}, "192.168.2.239"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubSeams(t, tc.links, failPlain(t), failOnLink(t))
			if ip, ok := SourceIP(context.Background(), tc.host, 50051); ok {
				t.Fatalf("SourceIP = %q, true; want false", ip)
			}
		})
	}
}

// A failed ARP does not depend on the port, so a second port cannot help.
func TestSourceIPStopsWhenNoPathReachesTheHost(t *testing.T) {
	stubSeams(t, []link{linkA, linkB}, failPlain(t),
		func(_ context.Context, _ link, addr string) (net.Conn, error) {
			if addr != "169.254.198.132:50051" {
				t.Errorf("dialed %s; want only the first port", addr)
			}
			return nil, dialErr(syscall.EHOSTUNREACH)
		})

	if ip, ok := SourceIP(context.Background(), "169.254.198.132", 50051, 50052); ok {
		t.Fatalf("SourceIP = %q, true; want false", ip)
	}
}

// Once the routing table's own path answers, its source address is the right
// one and probing further ports would only repeat the race.
func TestSourceIPDefersToTheRoutingTableWhenItsPathAnswers(t *testing.T) {
	stubSeams(t, []link{linkB},
		func(_ context.Context, addr string) (net.Conn, error) {
			if addr != "169.254.198.132:50051" {
				t.Errorf("plain dial to %s; want only the first port", addr)
			}
			return nil, dialErr(syscall.ECONNREFUSED)
		},
		func(context.Context, link, string) (net.Conn, error) { return nil, dialErr(syscall.EHOSTUNREACH) })
	routedSourceFn = func(string) net.IP { return net.ParseIP("192.168.2.10") }

	if ip, ok := SourceIP(context.Background(), "169.254.198.132", 50051, 50052); ok {
		t.Fatalf("SourceIP = %q, true; want false", ip)
	}
}

func stubListen(t *testing.T, links []link, fn func(context.Context, link, string) (net.Listener, error)) {
	t.Helper()
	stubLinks(t, links)
	origListen := linkListenFn
	t.Cleanup(func() { linkListenFn = origListen })
	linkListenFn = fn
}

// Replies must leave through the link that owns the address, whatever the
// routing table prefers, so a listener on a link's address is always pinned.
func TestListenPinsToTheLinkOwningTheAddress(t *testing.T) {
	for _, tc := range []struct {
		name  string
		links []link
	}{
		{"several links", []link{linkA, linkB}},
		{"single link", []link{linkB}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pinned string
			stubListen(t, tc.links, func(_ context.Context, l link, address string) (net.Listener, error) {
				pinned = l.ifi.Name
				if address != "169.254.39.227:0" {
					t.Errorf("listen address = %q", address)
				}
				return nil, errors.New("stub")
			})

			_, _ = Listen(context.Background(), "169.254.39.227:0")
			if pinned != linkB.ifi.Name {
				t.Fatalf("listener pinned to %q, want %q", pinned, linkB.ifi.Name)
			}
		})
	}
}

func TestListenFallsBackToPlainListenerWhenBindingIsUnavailable(t *testing.T) {
	stubListen(t, nil, func(context.Context, link, string) (net.Listener, error) {
		return nil, &net.OpError{Op: "listen", Net: "tcp4", Err: &bindError{err: syscall.EPERM}}
	})

	// Loopback stands in for the link-local address: only the fallback is under test.
	ln, err := listenPinned(context.Background(), linkB, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listenPinned: %v; want the plain fallback listener", err)
	}
	ln.Close()
}

func TestListenLeavesOtherAddressesUnpinned(t *testing.T) {
	stubListen(t, []link{linkA, linkB}, func(context.Context, link, string) (net.Listener, error) {
		t.Error("listener pinned; want a plain listener")
		return nil, errors.New("stub")
	})

	ln, err := Listen(context.Background(), "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ln.Close()
}

type closeRecorder struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func newCloseRecorder(c net.Conn) *closeRecorder {
	return &closeRecorder{Conn: c, closed: make(chan struct{})}
}

func (c *closeRecorder) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func (c *closeRecorder) waitClosed(d time.Duration) bool {
	select {
	case <-c.closed:
		return true
	case <-time.After(d):
		return false
	}
}
