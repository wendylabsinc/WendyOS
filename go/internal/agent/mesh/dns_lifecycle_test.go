package mesh

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
)

func TestDNSReleaseDrainsRequestWithoutHoldingStateLock(t *testing.T) {
	server := NewDNSServer(zap.NewNop(), "")
	packets, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	entered, continueRequest, started, shutdownEntered := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	engine := &dns.Server{PacketConn: packets, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) { close(entered); <-continueRequest; server.handle(w, r) })}
	server.listeners["127.0.0.1"] = &gatewayListener{refs: 1, srv: engine, addr: packets.LocalAddr().String()}
	server.port = packets.LocalAddr().(*net.UDPAddr).Port
	serveDone := make(chan error, 1)
	go func() { serveDone <- engine.ActivateAndServe() }()
	<-started
	original := shutdownDNSGateway
	t.Cleanup(func() { shutdownDNSGateway = original })
	shutdownDNSGateway = func(s *dns.Server) error { close(shutdownEntered); return original(s) }
	queryDone := make(chan error, 1)
	go func() {
		q := new(dns.Msg)
		q.SetQuestion("device-22222222-2222-4222-8222-222222222222.mesh.wendy.internal.", dns.TypeA)
		_, _, err := (&dns.Client{Timeout: 3 * time.Second}).Exchange(q, packets.LocalAddr().String())
		queryDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not reach handler")
	}
	released := make(chan struct{})
	go func() { server.ReleaseListener("127.0.0.1"); close(released) }()
	<-shutdownEntered
	// Starting another user of this gateway must wait for the retiring listener,
	// without retaining the state lock that the draining handler needs.
	replacement := make(chan error, 1)
	go func() { replacement <- server.EnsureListener("127.0.0.1") }()
	close(continueRequest)
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("DNS release deadlocked with its own in-flight handler")
	}
	if err := <-queryDone; err != nil {
		t.Fatal(err)
	}
	if err := <-replacement; err != nil {
		t.Fatalf("replacement listener raced a still-bound socket: %v", err)
	}
	shutdownDNSGateway = original
	server.ReleaseListener("127.0.0.1")
	if err := <-serveDone; err != nil {
		t.Fatal(err)
	}
}

func TestDNSImmediateReleaseReturnsItsSocket(t *testing.T) {
	for range 20 {
		server := NewDNSServer(zap.NewNop(), "")
		server.port = 0
		if err := server.EnsureListener("127.0.0.1"); err != nil {
			t.Fatal(err)
		}
		server.mu.Lock()
		packets := server.listeners["127.0.0.1"].srv.PacketConn
		server.mu.Unlock()
		server.ReleaseListener("127.0.0.1")
		// Inspect the actual owned socket. An unrelated test process can
		// legitimately acquire its ephemeral port as soon as release closes it.
		if err := packets.SetReadDeadline(time.Now()); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("immediate start/stop retained an open DNS socket: %v", err)
		}
	}
}
