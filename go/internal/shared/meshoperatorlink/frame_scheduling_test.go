package meshoperatorlink

import (
	"bytes"
	"context"
	"crypto/tls"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func waitFrameWriters(t *testing.T, m *frameWriteMutex, routing, ordinary int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		ready := m.waitingRouting == routing && m.waitingOrdinary == ordinary
		m.mu.Unlock()
		if ready {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("frame writers did not reach barrier")
}

func TestFrameWriterPrioritizesRoutingWithoutStarvingOrdinary(t *testing.T) {
	var m frameWriteMutex
	m.Lock()
	turns := make(chan string, 3)
	release := make(chan struct{})
	defer close(release)
	go func() { m.Lock(); turns <- "ordinary"; <-release; m.Unlock() }()
	waitFrameWriters(t, &m, 0, 1)
	go func() { m.lockRouting(); turns <- "routing"; <-release; m.Unlock() }()
	waitFrameWriters(t, &m, 1, 1)
	m.Unlock()
	if got := <-turns; got != "routing" {
		t.Fatal("queued ordinary write overtook Babel:", got)
	}
	// Queue another routing writer while the first holds the physical boundary.
	go func() { m.lockRouting(); turns <- "routing2"; <-release; m.Unlock() }()
	waitFrameWriters(t, &m, 1, 1)
	release <- struct{}{}
	if got := <-turns; got != "ordinary" {
		t.Fatal("routing flood starved ordinary frame:", got)
	}
	release <- struct{}{}
	if got := <-turns; got != "routing2" {
		t.Fatal(got)
	}
	release <- struct{}{}
	// Join the final unlock without sleeping.
	m.Lock()
	m.Unlock()
}

func schedulingTLS(t *testing.T) (*tls.Conn, *tls.Conn) {
	t.Helper()
	now := time.Now()
	certificate, _ := operatorClockFixture(t, now.Add(-time.Hour), now.Add(time.Hour))
	a, b := net.Pipe()
	client := tls.Client(a, &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true}) // Framing fixture; no admission-policy claim.
	server := tls.Server(b, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	errch := make(chan error, 1)
	go func() { errch <- server.HandshakeContext(ctx) }()
	if err := client.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-errch; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return client, server
}

func TestRouterBabelSurvivesIPFloodAndTakesNextFrameBoundary(t *testing.T) {
	client, server := schedulingTLS(t)
	l, err := routerReady(context.Background(), client, meshid.Device{}, meshid.Device{}, false, time.Now().Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close(); l.Wait() })
	l.writeMu.Lock()
	unlocked := false
	defer func() {
		if !unlocked {
			l.writeMu.Unlock()
		}
	}()
	for range 1000 {
		if err := l.SendDatagram([]byte{1, 7}); err != nil {
			t.Fatal(err)
		}
	}
	waitFrameWriters(t, &l.writeMu, 1, 0)
	hello := []byte{2, 42}
	if err := l.SendDatagram(hello); err != nil {
		t.Fatal(err)
	}
	ordinaryDone := make(chan error, 1)
	go func() { ordinaryDone <- l.WriteControlBytes([]byte("proof")) }()
	waitFrameWriters(t, &l.writeMu, 1, 1)
	l.writeMu.Unlock()
	unlocked = true
	_ = server.SetReadDeadline(time.Now().Add(time.Second))
	kind, data, err := readFrame(server)
	if err != nil || kind != 4 || !bytes.Equal(data, hello) {
		t.Fatalf("first boundary kind=%d data=%x err=%v", kind, data, err)
	}
	kind, data, err = readFrame(server)
	if err != nil || kind != 5 || string(data) != "proof" {
		t.Fatalf("ordinary fairness kind=%d data=%x err=%v", kind, data, err)
	}
	if err := <-ordinaryDone; err != nil {
		t.Fatal(err)
	}
	kind, data, err = readFrame(server)
	if err != nil || kind != 4 || !bytes.Equal(data, []byte{1, 7}) {
		t.Fatalf("IP fairness kind=%d data=%x err=%v", kind, data, err)
	}
}

func TestRouterReceiveIPOverloadDoesNotBlockBabelOrControls(t *testing.T) {
	client, server := schedulingTLS(t)
	l, err := routerReady(context.Background(), client, meshid.Device{}, meshid.Device{}, false, time.Now().Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close(); l.Wait() })
	written := make(chan error, 1)
	go func() {
		_ = server.SetWriteDeadline(time.Now().Add(time.Second))
		for range 1000 {
			if err := writeFrame(server, 4, []byte{1, 7}); err != nil {
				written <- err
				return
			}
		}
		if err := writeFrame(server, 4, []byte{2, 42}); err != nil {
			written <- err
			return
		}
		written <- writeFrame(server, 5, []byte("proof"))
	}()
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("IP consumer stalled frame parser")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := l.ReceiveDatagram(ctx)
	if err != nil || !bytes.Equal(got, []byte{2, 42}) {
		t.Fatalf("Babel did not pass IP overload: %x %v", got, err)
	}
	select {
	case got := <-l.controls:
		if string(got) != "proof" {
			t.Fatal(string(got))
		}
	case <-ctx.Done():
		t.Fatal("controls stalled")
	}
}

func TestRouterReservedLanesRemainBoundedAndFairUnderSustainedLoad(t *testing.T) {
	l := &RouterLink{Link: &Link{done: make(chan struct{})}, transmit: make(chan []byte, 32), routing: make(chan []byte, 32), receivedRouting: make(chan []byte, 32), packets: make(chan []byte, 32), transmitWake: make(chan struct{}, 1)}
	lastRouting := false
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for cycle := range 1000 {
		for range 64 {
			if err := l.SendDatagram([]byte{1, 7}); err != nil {
				t.Fatal(err)
			}
		}
		hello := []byte{2, byte(cycle)}
		if err := l.SendDatagram(hello); err != nil {
			t.Fatal(err)
		}
		// At most one waiting IP turn precedes Babel; both lanes make progress.
		gotHello, gotIP := false, false
		for range 2 {
			got, ok := l.nextDatagram(&lastRouting)
			if !ok {
				t.Fatal("open scheduler stopped")
			}
			if routerBabel(got) {
				gotHello = bytes.Equal(got, hello)
			} else {
				gotIP = true
			}
		}
		if !gotHello || !gotIP || len(l.transmit) > 32 || len(l.routing) != 0 {
			t.Fatalf("cycle %d: hello=%v IP=%v lanes=%d/%d", cycle, gotHello, gotIP, len(l.routing), len(l.transmit))
		}
		// Sustained routing input must not starve the other receive class either.
		l.receivedRouting <- hello
		l.receivedRouting <- hello
		l.packets <- []byte{1, 7}
		gotHello, gotIP = false, false
		for range 3 {
			got, err := l.ReceiveDatagram(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if routerBabel(got) {
				gotHello = true
			} else {
				gotIP = true
			}
		}
		if !gotHello || !gotIP {
			t.Fatal("receive class starved")
		}
	}
}

func TestRouterBabelGetsFrameBoundaryUnderIPLoad(t *testing.T) {
	client, server := schedulingTLS(t)
	l, err := routerReady(context.Background(), client, meshid.Device{}, meshid.Device{}, false, time.Now().Add(time.Minute), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close(); l.Wait() })
	l.writeMu.Lock()
	for range 1000 {
		if err := l.SendDatagram([]byte{1, 7}); err != nil {
			l.writeMu.Unlock()
			t.Fatal(err)
		}
	}
	hello := []byte{2, 42}
	if err := l.SendDatagram(hello); err != nil {
		l.writeMu.Unlock()
		t.Fatal(err)
	}
	l.writeMu.Unlock()
	_ = server.SetReadDeadline(time.Now().Add(time.Second))
	delivered := false
	for range 2 {
		kind, data, err := readFrame(server)
		if err != nil {
			t.Fatal(err)
		}
		if kind == 4 && bytes.Equal(data, hello) {
			delivered = true
			break
		}
	}
	if !delivered {
		t.Fatal("Babel was dropped or queued behind the IP backlog")
	}
}
