package commands

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const forwardedRequest = "GET / HTTP/1.1\r\nHost: localhost\r\n\r\n"

// loopbackConn returns both ends of a TCP connection on 127.0.0.1: the local
// client's, and the one a forward accepted from it.
func loopbackConn(t *testing.T) (client, accepted net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	client, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	accepted, err = ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { accepted.Close() })
	return client, accepted
}

// relayPipe models a tunnel ending when the device closes. The test holds
// the pipe's other end as the device.
func relayPipe(t *testing.T) (tunnel, device net.Conn) {
	t.Helper()
	tunnel, device = net.Pipe()
	t.Cleanup(func() { device.Close() })
	return tunnel, device
}

// takeRequest reads the forwarded request on the device end of a relayPipe.
// Once it returns, the relay has passed the request on and is waiting for more
// from the local client.
func takeRequest(t *testing.T, device net.Conn) {
	t.Helper()
	_ = device.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(device, make([]byte, len(forwardedRequest))); err != nil {
		t.Fatalf("the request did not reach the device: %v", err)
	}
}

// legacyTunnel is a tunnel as the legacy broker path hands it out
// (pipeBrokerTunnel), over a broker stream that carries the request and then
// ends the way end describes. leave waits for the request and ends the stream.
func legacyTunnel(t *testing.T, end func() (payload []byte, halfClose bool, err error)) (tunnel net.Conn, leave func()) {
	t.Helper()
	sent := make(chan int, 16)
	ended := make(chan struct{})
	tunnel = pipeBrokerTunnel(
		func() ([]byte, bool, error) { <-ended; return end() },
		func(payload []byte, _ bool) error { sent <- len(payload); return nil },
		func() error { return nil },
	)
	return tunnel, func() {
		t.Helper()
		for got := 0; got < len(forwardedRequest); {
			select {
			case n := <-sent:
				got += n
			case <-time.After(5 * time.Second):
				t.Fatal("the request did not reach the broker")
			}
		}
		close(ended)
	}
}

// The ways a legacy broker stream ends: the stream itself ends cleanly, the
// device half-closes, or the broker ends it with a verdict.
func legacyStreamEnds() ([]byte, bool, error) { return nil, false, io.EOF }
func legacyHalfClose() ([]byte, bool, error)  { return nil, true, nil }
func legacyRefusal() ([]byte, bool, error) {
	return nil, false, status.Error(codes.Unavailable, "asset is offline")
}

// expectEOF fails unless the local client, waiting for a reply, sees its
// connection closed within a second.
func expectEOF(t *testing.T, client net.Conn) {
	t.Helper()
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the waiting local client's read ended with %v, want EOF as soon as the tunnel ends", err)
	}
}

// Nothing listens on the device's port: the device side takes the request and
// then ends the tunnel without an answer. The local client, still waiting for
// its reply, must see its connection close then, not when it gives up.
func TestRelayTunnelClosesLocalConnectionWhenTunnelEnds(t *testing.T) {
	tunnels := []struct {
		name string
		open func(t *testing.T) (tunnel net.Conn, leave func())
	}{
		{"v2 relay", func(t *testing.T) (net.Conn, func()) {
			tunnel, device := relayPipe(t)
			return tunnel, func() { takeRequest(t, device); device.Close() }
		}},
		{"legacy broker ends the stream", func(t *testing.T) (net.Conn, func()) { return legacyTunnel(t, legacyStreamEnds) }},
		{"legacy device half-closes", func(t *testing.T) (net.Conn, func()) { return legacyTunnel(t, legacyHalfClose) }},
		{"legacy broker refuses", func(t *testing.T) (net.Conn, func()) { return legacyTunnel(t, legacyRefusal) }},
	}
	for _, tt := range tunnels {
		t.Run(tt.name, func(t *testing.T) {
			tunnel, leave := tt.open(t)
			client, accepted := loopbackConn(t)
			relayed := make(chan struct{})
			go func() {
				defer close(relayed)
				relayTunnel(context.Background(), accepted, tunnel)
			}()
			if _, err := client.Write([]byte(forwardedRequest)); err != nil {
				t.Fatal(err)
			}
			leave()

			expectEOF(t, client)
			select {
			case <-relayed:
			case <-time.After(time.Second):
				t.Fatal("relay still running after the tunnel ended")
			}
		})
	}
}

// unansweredProbe serves forwarded connections with an unansweredNotice whose
// clock the test moves, and counts its notices.
type unansweredProbe struct {
	clock    fakeClock
	notice   *unansweredNotice
	notified int // read through notices, once a connection has been served
}

func newUnansweredProbe() *unansweredProbe {
	p := &unansweredProbe{clock: fakeClock{t: time.Unix(1_700_000_000, 0)}}
	p.notice = &unansweredNotice{
		window:   unansweredWindow,
		interval: unansweredNoticeInterval,
		now:      p.clock.now,
		notify:   func() { p.notified++ },
	}
	return p
}

// forwarded is one local connection served through a tunnel.
type forwarded struct {
	client net.Conn
	stop   context.CancelFunc // ends the forward's context
	// localEnded closes once the forward has read the end of the local
	// client's sending.
	localEnded <-chan struct{}
	served     <-chan struct{}
}

// readEndConn closes ended when a Read from the connection first fails.
type readEndConn struct {
	net.Conn
	once  sync.Once
	ended chan struct{}
}

func (c *readEndConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		c.once.Do(func() { close(c.ended) })
	}
	return n, err
}

// forward serves one local connection through tunnel, as serveTCPForward does
// for each connection it accepts, and sends the request from the local client.
func (p *unansweredProbe) forward(t *testing.T, tunnel net.Conn) *forwarded {
	t.Helper()
	client, accepted := loopbackConn(t)
	ctx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	local := &readEndConn{Conn: accepted, ended: make(chan struct{})}
	served := make(chan struct{})
	go func() {
		defer close(served)
		serveForwardedConn(ctx, local, func(context.Context) (net.Conn, error) { return tunnel, nil },
			func(err error) { t.Errorf("opening the tunnel failed: %v", err) }, p.notice)
	}()
	if _, err := client.Write([]byte(forwardedRequest)); err != nil {
		t.Fatal(err)
	}
	return &forwarded{client: client, stop: stop, localEnded: local.ended, served: served}
}

// notices waits until f has been served and returns the notices so far.
func (p *unansweredProbe) notices(t *testing.T, f *forwarded) int {
	t.Helper()
	select {
	case <-f.served:
	case <-time.After(5 * time.Second):
		t.Fatal("the forwarded connection was still being served 5s after its tunnel ended")
	}
	return p.notified
}

// The notice is for a connection the device ends within the window, without a
// byte, while the local client still waits, and for no other.
func TestForwardedConnReportsOnlyUnansweredDeviceClose(t *testing.T) {
	tests := []struct {
		name string
		// play acts on a connection whose request has reached the device.
		play func(t *testing.T, p *unansweredProbe, f *forwarded, device net.Conn)
		want int
	}{
		{"device closes without answering", func(t *testing.T, p *unansweredProbe, f *forwarded, device net.Conn) {
			device.Close()
		}, 1},
		{"device closes late in the window", func(t *testing.T, p *unansweredProbe, f *forwarded, device net.Conn) {
			p.clock.t = p.clock.t.Add(unansweredWindow - time.Second)
			device.Close()
		}, 1},
		{"device closes after the window", func(t *testing.T, p *unansweredProbe, f *forwarded, device net.Conn) {
			p.clock.t = p.clock.t.Add(unansweredWindow + time.Second)
			device.Close()
		}, 0},
		{"device answers, then closes", func(t *testing.T, p *unansweredProbe, f *forwarded, device net.Conn) {
			if _, err := device.Write([]byte("HTTP/1.1 204 No Content\r\n\r\n")); err != nil {
				t.Fatal(err)
			}
			device.Close()
		}, 0},
		{"local client closes first", func(t *testing.T, p *unansweredProbe, f *forwarded, device net.Conn) {
			f.client.Close()
			select {
			case <-f.localEnded:
			case <-time.After(5 * time.Second):
				t.Fatal("the forward did not see the local client close")
			}
			device.Close()
		}, 0},
		{"forward stops", func(t *testing.T, p *unansweredProbe, f *forwarded, device net.Conn) {
			f.stop()
			device.Close()
		}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newUnansweredProbe()
			tunnel, device := relayPipe(t)
			f := p.forward(t, tunnel)
			takeRequest(t, device)
			tt.play(t, p, f, device)
			if got := p.notices(t, f); got != tt.want {
				t.Fatalf("%d notices, want %d", got, tt.want)
			}
		})
	}
}

// The legacy tunnel ends the same way when its stream ends cleanly. A stream
// the broker ends with a verdict is a failed tunnel, not an unanswered port.
func TestForwardedConnLegacyTunnelUnanswered(t *testing.T) {
	tests := []struct {
		name string
		end  func() ([]byte, bool, error)
		want int
	}{
		{"broker ends the stream", legacyStreamEnds, 1},
		{"device half-closes", legacyHalfClose, 1},
		{"broker refuses", legacyRefusal, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newUnansweredProbe()
			tunnel, leave := legacyTunnel(t, tt.end)
			f := p.forward(t, tunnel)
			leave()
			if got := p.notices(t, f); got != tt.want {
				t.Fatalf("%d notices, want %d", got, tt.want)
			}
		})
	}
}

// A client that retries, or a page that opens several connections, gets one
// notice per interval, not one per connection.
func TestUnansweredNoticeAtMostOncePerInterval(t *testing.T) {
	p := newUnansweredProbe()
	unanswered := func() int {
		t.Helper()
		tunnel, device := relayPipe(t)
		f := p.forward(t, tunnel)
		takeRequest(t, device)
		device.Close()
		return p.notices(t, f)
	}
	if got := unanswered(); got != 1 {
		t.Fatalf("%d notices after the first unanswered connection, want 1", got)
	}
	if got := unanswered(); got != 1 {
		t.Fatalf("%d notices after a second unanswered connection at once, want still 1", got)
	}
	p.clock.t = p.clock.t.Add(unansweredNoticeInterval - time.Second)
	if got := unanswered(); got != 1 {
		t.Fatalf("%d notices after an unanswered connection %s later, want still 1", got, unansweredNoticeInterval-time.Second)
	}
	p.clock.t = p.clock.t.Add(time.Second)
	if got := unanswered(); got != 2 {
		t.Fatalf("%d notices after an unanswered connection %s after the first notice, want 2", got, unansweredNoticeInterval)
	}
}

// A connection through the forward's listener to a device port nothing
// listens on: the local client gets its connection closed at once, and the
// forward reports the reason.
func TestServeTCPForwardClosedDevicePort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	devices := make(chan net.Conn, 1)
	notified := make(chan struct{}, 1)
	served := make(chan error, 1)
	go func() {
		served <- serveTCPForward(ctx, ln, func(context.Context) (net.Conn, error) {
			tunnel, device := net.Pipe()
			devices <- device
			return tunnel, nil
		}, func(error) {}, newUnansweredNotice(func() { notified <- struct{}{} }))
	}()

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.Write([]byte(forwardedRequest)); err != nil {
		t.Fatal(err)
	}
	var device net.Conn
	select {
	case device = <-devices:
	case <-time.After(5 * time.Second):
		t.Fatal("the forward did not open a tunnel for the connection")
	}
	takeRequest(t, device)
	device.Close()

	expectEOF(t, client)
	select {
	case <-notified:
	case <-time.After(5 * time.Second):
		t.Fatal("the forward did not report the unanswered connection")
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("forwarding ended with %v", err)
	}
}
