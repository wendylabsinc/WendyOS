package meshsession

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
)

type observedTLSWrite struct {
	net.Conn
	observe atomic.Bool
	started chan struct{}
}

func (c *observedTLSWrite) Write(b []byte) (int, error) {
	if c.observe.Load() {
		select {
		case c.started <- struct{}{}:
		default:
		}
	}
	return c.Conn.Write(b)
}

// One TLS flow with a full stream window must not stall the manager shared
// by unrelated application UDP flows. net.Pipe makes that window deterministic.
func TestTLSUDPBackpressureDoesNotBlockOtherFlows(t *testing.T) {
	a, b := fixtureCredentials(t)
	server, err := NewServer(b, allowPort(func(uint16) bool { return false }))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	rawClient, rawServer := net.Pipe()
	observed := &observedTLSWrite{Conn: rawClient, started: make(chan struct{}, 1)}
	cfg, err := a.PeerTLS(b.Device)
	if err != nil {
		t.Fatal(err)
	}
	cfg.NextProtos = []string{ALPN}
	secureClient, secureServer := tls.Client(observed, cfg), tls.Server(rawServer, server.TLSConfig())
	defer rawClient.Close()
	defer rawServer.Close()
	setup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	serverHandshake := make(chan error, 1)
	go func() { serverHandshake <- secureServer.HandshakeContext(setup) }()
	if err := secureClient.HandshakeContext(setup); err != nil {
		t.Fatal(err)
	}
	if err := <-serverHandshake; err != nil {
		t.Fatal(err)
	}
	observed.observe.Store(true)

	manager := udpmessage.NewManager()
	defer manager.Close()
	var ownership sync.RWMutex
	slow, err := manager.Open(b.Device, func(send func() error) error {
		ownership.RLock()
		defer ownership.RUnlock()
		return send()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer slow.Close()
	// Close the byte stream before joining endpoint retirement, including on
	// assertion failures: its peer intentionally never drains application data.
	defer rawClient.Close()
	if _, err := attachTLSUDPTransport(&tlsRecordConn{Conn: secureClient}, slow, MaxUDPMessageSize); err != nil {
		t.Fatal(err)
	}
	if err := slow.Send([]byte("blocked TLS request")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed.started:
	case <-time.After(time.Second):
		t.Fatal("TLS write did not start")
	}
	// The same admission guard also protects registry updates. Backpressure
	// must not pin its read lock while application ownership changes.
	revoked := make(chan struct{})
	go func() { ownership.Lock(); ownership.Unlock(); close(revoked) }()
	select {
	case <-revoked:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("backpressured TLS write retained the ownership guard")
	}
	// Saturate the bounded stream queue as well as its active write. The
	// shared scheduler must keep visiting other flows while it retries.
	for range 4 {
		if err := slow.Send([]byte("queued TLS request")); err != nil {
			t.Fatal(err)
		}
	}
	until := time.Now().Add(time.Second)
	for manager.Snapshot().TransportQueueRefusals == 0 {
		if time.Now().After(until) {
			t.Fatal("backpressure did not refuse bounded queue admission")
		}
		time.Sleep(time.Millisecond)
	}
	healthy, err := manager.Open(a.Device, func(send func() error) error { return send() })
	if err != nil {
		t.Fatal(err)
	}
	defer healthy.Close()
	admitted := make(chan struct{}, 1)
	if err := healthy.SetTransport(MaxUDPTransportChunk, true, func([]byte) error { admitted <- struct{}{}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := healthy.Send([]byte("healthy unrelated flow")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-admitted:
	case <-time.After(250 * time.Millisecond):
		t.Fatal("one backpressured TLS flow blocked unrelated UDP admission")
	}
}
