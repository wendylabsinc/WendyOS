//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

type firstFlightConn struct {
	net.Conn
	once    sync.Once
	started chan struct{}
}

func (c *firstFlightConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.started) })
	return c.Conn.Write(p)
}

func TestEqualHintCrossedHandshakesAuthenticateBothDirections(t *testing.T) {
	low := fixtureDevice("12345678-1234-4000-8000-000000000001")
	high := fixtureDevice("12345678-1234-4000-8000-000000000002")

	lowCert, _ := testTLSIdentity(t)
	highCert, _ := testTLSIdentity(t)
	identities := map[string]meshid.Device{string(lowCert.Certificate[0]): low, string(highCert.Certificate[0]): high}
	makeRuntime := func(device meshid.Device, cert tls.Certificate) *runtime {
		credentials := &localmesh.Credentials{Device: device, Certificate: cert, Verify: func(chain [][]byte, _ time.Time) (localmesh.Identity, error) {
			// The PKI suites cover chain trust. Here real TLS proves possession
			// of each distinct fixture key in both directions.
			if len(chain) != 1 {
				return localmesh.Identity{}, errors.New("missing fixture certificate")
			}
			peer, ok := identities[string(chain[0])]
			if !ok {
				return localmesh.Identity{}, errors.New("untrusted fixture certificate")
			}
			return localmesh.Identity{Device: peer}, nil
		}}
		r := &runtime{cfg: Config{Credentials: credentials, Node: &refreshTestNode{attached: make(chan struct{})}, Logger: zap.NewNop()},
			tlsGate: newTLSHandshakeAdmission(), advertisementRefresh: make(chan struct{}, 1)}
		r.serverTLS = r.makeServerTLS()
		return r
	}
	a, b := makeRuntime(low, lowCert), makeRuntime(high, highCert)
	aServer, bClient := packetPair(t)
	bServer, aClient := packetPair(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	type outcome struct {
		device meshid.Device
		peer   meshid.Device
		err    error
	}
	results := make(chan outcome, 2)
	var clients sync.WaitGroup
	firstFlights := []chan struct{}{make(chan struct{}), make(chan struct{})}
	startClient := func(r *runtime, raw net.Conn, peer meshid.Device, first chan struct{}) {
		clients.Go(func() {
			cfg, authenticated, err := r.clientTLS(candidate{hint: deviceHint(peer.UUID())})
			if err == nil {
				cfg.CurvePreferences = []tls.CurveID{tls.X25519}
				secure := tls.Client(&firstFlightConn{Conn: raw, started: first}, cfg)
				_, err = r.handshakeOutbound(ctx, secure)
			}
			result := outcome{device: r.cfg.Credentials.Device, err: err}
			if authenticated != nil {
				result.peer = *authenticated
			}
			results <- result
		})
	}
	startClient(a, aClient, high, firstFlights[0])
	startClient(b, bClient, low, firstFlights[1])
	// Both outgoing first flights are already active when inbound admission
	// starts. A single gate shared by both directions deadlocks this schedule.
	for _, first := range firstFlights {
		select {
		case <-first:
		case <-ctx.Done():
			t.Fatal("outgoing first flight never started")
		}
	}
	var servers sync.WaitGroup
	servers.Go(func() { a.acceptLink(ctx, aServer) })
	servers.Go(func() { b.acceptLink(ctx, bServer) })
	for range 2 {
		result := <-results
		expected := low
		if result.device == low {
			expected = high
		}
		if result.err != nil || result.peer != expected {
			t.Errorf("crossed authentication: peer=%s err=%v", result.peer.Principal(), result.err)
		}
	}
	cancel()
	clients.Wait()
	servers.Wait()

}
