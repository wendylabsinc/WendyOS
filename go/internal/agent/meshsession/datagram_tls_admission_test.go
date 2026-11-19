package meshsession

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
)

// Pause one accepted socket's writes after TLS/WAU3 setup. Everything above
// that byte-stream boundary uses the production server, registry and scheduler.
// Closing the connection always interrupts the blocked write, as a real socket
// close does, including when a failed assertion tears down the fixture.
type stalledUDPReplyConn struct {
	net.Conn
	stall   atomic.Bool
	started chan struct{}
	closed  chan struct{}
	start   sync.Once
	close   sync.Once
}

func (c *stalledUDPReplyConn) Write(p []byte) (int, error) {
	if c.stall.Load() {
		c.start.Do(func() { close(c.started) })
		<-c.closed
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}

func (c *stalledUDPReplyConn) Close() error {
	c.close.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type stalledUDPReplyListener struct {
	net.Listener
	claimed atomic.Bool
	first   chan *stalledUDPReplyConn
}

func (l *stalledUDPReplyListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil || l.claimed.Swap(true) {
		return conn, err
	}
	wrapped := &stalledUDPReplyConn{Conn: conn, started: make(chan struct{}), closed: make(chan struct{})}
	l.first <- wrapped
	return wrapped, nil
}

// Audit 2577: a stalled TLS consumer must not pin the shared app admission lock
// or UDP scheduler. All successful observations are complete TLS-to-UDP echo
// exchanges, including newly authenticated sessions after a registry update.
func TestTLSUDPStallPreservesAdmissionAndRecovery(t *testing.T) {
	a, b := fixtureCredentials(t)
	registry := meshingress.NewRegistry()
	slowPort := udpLifecycleApp(t, registry, "stalled-owner")
	healthyPort := udpLifecycleApp(t, registry, "healthy-owner")
	server, err := NewServer(b, testUDPAuthorization{registry})
	if err != nil {
		t.Fatal(err)
	}
	rawListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := &stalledUDPReplyListener{Listener: rawListener, first: make(chan *stalledUDPReplyConn, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		server.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("TLS server workers did not stop")
		}
	})
	address := netip.MustParseAddrPort(listener.Addr().String())
	client, err := NewClient(a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	dial := func(port uint16) *TLSDatagramFlow {
		t.Helper()
		setup, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		flow, err := client.DialUDPTLS(setup, b.Device, address, port)
		if err != nil {
			t.Fatalf("TLS UDP admission failed on port %d: %v", port, err)
		}
		return flow
	}
	exchange := func(flow *TLSDatagramFlow, marker string) {
		t.Helper()
		if err := flow.Send([]byte(marker)); err != nil {
			t.Fatalf("send %s: %v", marker, err)
		}
		read, stop := context.WithTimeout(ctx, 2*time.Second)
		defer stop()
		got, err := readUDPMessage(flow, read)
		if err != nil || string(got) != marker {
			t.Fatalf("TLS UDP echo stalled or changed: %s, got=%q err=%v", marker, got, err)
		}
	}
	slow := dial(slowPort)
	var wire *stalledUDPReplyConn
	select {
	case wire = <-listener.first:
	case <-ctx.Done():
		t.Fatal("first accepted TLS socket was not observed")
	}
	t.Cleanup(func() { wire.Close() })
	exchange(slow, "slow flow before backpressure")
	healthy := dial(healthyPort)
	exchange(healthy, "healthy flow before backpressure")
	wire.stall.Store(true)
	if err := slow.Send([]byte("response held at the TLS socket")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-wire.started:
	case <-ctx.Done():
		t.Fatal("the TLS reply never reached the stalled socket")
	}
	for range 4 {
		if err := slow.Send([]byte("queued behind blocked reply")); err != nil {
			t.Fatal(err)
		}
	}
	// This requires real app delivery through the shared server scheduler,
	// rather than replacing the healthy flow with a successful send callback.
	exchange(healthy, "healthy flow while unrelated TLS consumer is stalled")
	deadline := time.Now().Add(time.Second)
	for server.udpManager.Snapshot().TransportQueueRefusals == 0 {
		if time.Now().After(deadline) {
			t.Fatal("stalled TLS transport did not exercise bounded queue refusal")
		}
		time.Sleep(time.Millisecond)
	}
	change := func(update func() error) {
		t.Helper()
		result, finished := make(chan error, 1), make(chan struct{})
		go func() { defer close(finished); result <- update() }()
		t.Cleanup(func() {
			wire.Close()
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Error("registry update remained blocked after socket close")
			}
		})
		select {
		case err := <-result:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("stalled TLS reply pinned the production registry admission lock")
		}
	}
	oldHealthyToken := registry.UDPToken(healthyPort)
	change(func() error {
		registry.Release("healthy-owner")
		return registry.ClaimUDPForApp("healthy-replacement", "healthy-replacement", healthyPort, "10.3.0.2", healthyPort)
	})
	if registry.UDPToken(healthyPort) == oldHealthyToken {
		t.Fatal("replacement retained the revoked app token")
	}
	fresh := dial(healthyPort)
	exchange(fresh, "new TLS session after unrelated registry replacement")
	select {
	case <-wire.closed:
		t.Fatal("progress depended on discarding the stalled consumer")
	default:
	}
	change(func() error { registry.Release("stalled-owner"); return nil })
	select {
	case <-wire.closed:
	case <-time.After(time.Second):
		t.Fatal("revocation did not close the blocked TLS transport")
	}
	read, stop := context.WithTimeout(ctx, time.Second)
	message, readErr := slow.Receive(read)
	stop()
	if message != nil {
		message.Release()
		t.Fatal("revoked owner delivered a queued reply")
	}
	if readErr == nil || errors.Is(readErr, context.DeadlineExceeded) {
		t.Fatalf("revoked TLS flow did not terminate: %v", readErr)
	}
	if err := registry.ClaimUDPForApp("recovered-owner", "recovered-owner", slowPort, "10.3.0.2", slowPort); err != nil {
		t.Fatal(err)
	}
	recovered := dial(slowPort)
	exchange(recovered, "same service after stalled owner retirement")
	exchange(fresh, "other service survives stalled owner retirement")
	t.Log("live TLS UDP exchanges, registry replacement, revocation and recovery passed under a blocked TLS reply")
}
