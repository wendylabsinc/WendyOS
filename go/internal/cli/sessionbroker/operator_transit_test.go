//go:build !windows

package sessionbroker

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh/carriers"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

type brokerTransitFixture struct {
	ctx        context.Context
	nodes      [2]*localmesh.Node
	networks   [2]*netmesh.Network
	candidates []carriers.Candidate
	connection net.Conn
}

func newBrokerTransitFixture(t *testing.T, parent context.Context, tenant meshid.Tenant, roots string, issue func(int64, string, crypto.Signer, x509.ExtKeyUsage) (tls.Certificate, string, string)) *brokerTransitFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	f := &brokerTransitFixture{ctx: ctx}
	var workers sync.WaitGroup
	var listeners []net.Listener
	// Register state cleanup before the worker cleanup below: the Node writes
	// its final checkpoint while it exits, including under the race detector.
	stateDirectory := t.TempDir()
	t.Cleanup(func() {
		cancel()
		if f.connection != nil {
			f.connection.Close()
		}
		for _, l := range listeners {
			l.Close()
		}
		workers.Wait()
		for _, network := range f.networks {
			if network != nil {
				_ = network.Close()
			}
		}
	})
	for i, id := range []string{"33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444"} {
		device, err := meshid.ParseDevice(tenant, id)
		if err != nil {
			t.Fatal(err)
		}
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		_, cert, private := issue(int64(10+i), device.Principal(), key, x509.ExtKeyUsageClientAuth)
		credentials, err := localmesh.NewCredentials(cert, roots, private)
		if err != nil {
			t.Fatal(err)
		}
		network, err := netmesh.NewNetwork(device)
		if err != nil {
			t.Fatal(err)
		}
		f.networks[i] = network
		node, err := localmesh.NewNodeWithNetwork(ctx, filepath.Join(stateDirectory, id, "node"), credentials, "transit fixture", 50052, network)
		if err != nil {
			network.Close()
			t.Fatal(err)
		}
		f.nodes[i] = node
		workers.Add(1)
		go func() { defer workers.Done(); _ = node.Run() }()
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, listener)
		f.candidates = append(f.candidates, carriers.Candidate{Device: device, Transport: carriers.TCP, Address: listener.Addr().String(), Cost: 128})
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				raw, err := listener.Accept()
				if err != nil {
					return
				}
				workers.Add(1)
				go func() {
					defer workers.Done()
					defer raw.Close()
					stop := context.AfterFunc(ctx, func() { raw.Close() })
					defer stop()
					secure := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{credentials.Certificate}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{meshoperatorlink.RouterALPN}})
					if secure.HandshakeContext(ctx) == nil {
						_ = node.AttachOperatorRouterStream(ctx, secure, 128)
					}
				}()
			}
		}()
	}
	return f
}
func (f *brokerTransitFixture) source(ctx context.Context, emit func([]carriers.Candidate)) error {
	emit(f.candidates)
	<-ctx.Done()
	return ctx.Err()
}
func (f *brokerTransitFixture) roundTrip(t *testing.T) {
	t.Helper()
	if f.connection == nil {
		// A new framed link can send its initial IHU before receiving a Hello,
		// advertising Infinity until the next IHU at 2 * DefaultHelloInterval.
		// Allow that full cycle plus one Hello interval for attachment, triggered
		// route propagation and race-detector scheduling; do not race the 8s IHU.
		const convergenceTimeout = 3 * babel.DefaultHelloInterval
		started := time.Now()
		until := time.NewTimer(convergenceTimeout)
		defer until.Stop()
		for {
			ready := false
			for _, r := range f.nodes[0].Snapshot().ForwardingRoutes {
				if r.Prefix.Addr() == f.networks[1].Address() && !r.Unreachable && !r.Local {
					ready = true
				}
			}
			if ready {
				t.Logf("broker transit route ready after %s (budget %s)", time.Since(started), convergenceTimeout)
				break
			}
			select {
			case <-f.ctx.Done():
				t.Fatal(f.ctx.Err())
			case <-until.C:
				for i, node := range f.nodes {
					snapshot := node.Snapshot()
					t.Logf("remote node %d: self=%s peers=%d routes=%+v forwarding=%+v", i, snapshot.Self.Principal(), snapshot.Peers, snapshot.Routes, snapshot.ForwardingRoutes)
				}
				t.Fatalf("broker did not become the remote routers' transit path within %s", convergenceTimeout)
			case <-time.After(20 * time.Millisecond):
			}
		}
		// A native listener belongs to the target's packet stack, never the host.
		listener, err := f.networks[1].ListenTCP(29000)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { listener.Close() })
		go func() {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			defer c.Close()
			stop := context.AfterFunc(f.ctx, func() { c.Close() })
			defer stop()
			_, _ = io.Copy(c, c)
		}()
		f.connection, err = f.networks[0].DialContext(f.ctx, net.JoinHostPort(f.networks[1].Address().String(), "29000"))
		if err != nil {
			t.Fatal(err)
		}
	}
	f.connection.SetDeadline(time.Now().Add(3 * time.Second))
	payload := bytes.Repeat([]byte("remote transit survives no local CLI lease"), 40)
	sent := make(chan error, 1)
	go func() { _, e := f.connection.Write(payload); sent <- e }()
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(f.connection, reply); err != nil {
		t.Fatal(err)
	}
	if err := <-sent; err != nil || !bytes.Equal(payload, reply) {
		t.Fatal("broker transit bytes", err)
	}
}
func (f *brokerTransitFixture) detached(t *testing.T) {
	t.Helper()
	until := time.NewTimer(3 * time.Second)
	defer until.Stop()
	for {
		live := false
		for _, r := range f.nodes[0].Snapshot().ForwardingRoutes {
			if r.Prefix.Addr() == f.networks[1].Address() && !r.Unreachable && !r.Local {
				live = true
			}
		}
		if !live {
			return
		}
		select {
		case <-until.C:
			t.Fatal("explicit disconnect retained transit FIB")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
