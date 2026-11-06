//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

func TestBLESkippedCandidatesAreNotPenalizedAndFailureTriesAnother(t *testing.T) {
	attempted := make(chan string, 10)
	r := &runtime{cfg: Config{Credentials: &localmesh.Credentials{Device: selfFixture}, Logger: zap.NewNop()}, dialSlot: make(chan struct{}, 1), dialCoC: func(_ context.Context, address, kind string, psm uint16) (net.Conn, error) {
		attempted <- address
		return nil, errors.New("fixture dial failure")
	}}
	var candidates []candidate
	for i := 1; i <= 5; i++ {
		candidates = append(candidates, candidate{address: fmt.Sprintf("02:00:00:00:00:%02X", i), addressType: "public", hint: deviceHint(lowDevice), psm: DefaultPSM})
	}
	r.startCandidates(context.Background(), candidates, time.Now())
	r.links.Wait()
	first := <-attempted
	if len(r.radioRetry) != 1 {
		t.Fatal("skipped candidates received failure cooldown")
	}
	// Pacing prevents another attempt, without changing any candidate penalty.
	r.startCandidates(context.Background(), candidates, time.Now())
	r.links.Wait()
	if len(r.radioRetry) != 1 || len(attempted) != 0 {
		t.Fatal("pacing changed penalties or admitted another attempt")
	}
	r.nextDial = time.Time{}
	r.startCandidates(context.Background(), candidates, time.Now())
	r.links.Wait()
	if second := <-attempted; second == first {
		t.Fatal("failed peer beat an unattempted healthy candidate")
	}
}

func TestBLEConsumersSharePhysicalAdapterBudget(t *testing.T) {
	p := localmesh.NewConnectionPolicy(selfFixture, false, localmesh.ConnectionLimits{Outgoing: localmesh.BLEMaxOutgoing, Attempts: localmesh.BLEMaxOutgoingAttempts, Total: localmesh.BLEMaxConnections})
	a := &runtime{cfg: Config{Policy: p}}
	b := &runtime{cfg: Config{Policy: p}}
	for i := 0; i < localmesh.BLEMaxConnections; i++ {
		if a.connectionPolicy().Reserve(fmt.Sprint(i), numberedBLEDevice(i+1), false) == nil {
			t.Fatal("shared reservation denied")
		}
	}
	if b.connectionPolicy().Reserve("extra", lowFixture, false) != nil {
		t.Fatal("second consumer created a separate adapter budget")
	}
}

type duplicateTLSNode struct {
	self   meshid.Device
	active atomic.Int32
}

func (n *duplicateTLSNode) AttachStream(ctx context.Context, peer meshid.Device, conn net.Conn, _ uint16) error {
	if err := localmesh.OpenStreamHello(ctx, conn, n.self, peer); err != nil {
		return err
	}
	if !localmesh.ConnectionReady(ctx, peer, func() { _ = conn.Close() }) {
		return nil
	}
	n.active.Add(1)
	defer n.active.Add(-1)
	_, err := io.Copy(io.Discard, conn)
	return err
}

// Two independent TLS streams stand in for two CoCs on a shared Bluetooth ACL.
// Exercise real dial/accept authentication, mesh hello and duplicate retirement;
// closing the loser must leave the winning stream alive at both endpoints.
func TestBLECrossedTLSConnectionsRetireOnlyLosingStream(t *testing.T) {
	low, high := fixtureDevice("12345678-1234-4000-8000-000000000001"), fixtureDevice("87654321-1234-4000-8000-000000000002")
	lowCert, _ := testTLSIdentity(t)
	highCert, _ := testTLSIdentity(t)
	identities := map[string]meshid.Device{string(lowCert.Certificate[0]): low, string(highCert.Certificate[0]): high}
	makeRuntime := func(self meshid.Device, cert tls.Certificate) *runtime {
		n := &duplicateTLSNode{self: self}
		credentials := &localmesh.Credentials{Device: self, Certificate: cert, Verify: func(chain [][]byte, _ time.Time) (localmesh.Identity, error) {
			if len(chain) != 1 {
				return localmesh.Identity{}, errors.New("missing certificate")
			}
			peer, ok := identities[string(chain[0])]
			if !ok {
				return localmesh.Identity{}, errors.New("untrusted certificate")
			}
			return localmesh.Identity{Device: peer}, nil
		}}
		r := &runtime{cfg: Config{Credentials: credentials, Node: n, NetworkName: "fixture", Logger: zap.NewNop()}, dialSlot: make(chan struct{}, 1), tlsGate: newTLSHandshakeAdmission(), advertisementRefresh: make(chan struct{}, 1)}
		r.serverTLS = r.makeServerTLS()
		return r
	}
	a, b := makeRuntime(low, lowCert), makeRuntime(high, highCert)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	// Reserve both outbound attempts before either endpoint sees incoming
	// progress: this deliberately exercises the remaining simultaneous race.
	aPeer := candidate{address: "02:00:00:00:00:02", addressType: "public", hint: deviceHint(high.UUID()), psm: DefaultPSM}
	bPeer := candidate{address: "02:00:00:00:00:01", addressType: "public", hint: deviceHint(low.UUID()), psm: DefaultPSM}
	aLease := a.connectionPolicy().Reserve(aPeer.key(), high, true)
	bLease := b.connectionPolicy().Reserve(bPeer.key(), low, true)
	if aLease == nil || bLease == nil {
		t.Fatal("outgoing reservation denied")
	}
	aRaw, aServer := tcpConnectionPair(t)
	bRaw, bServer := tcpConnectionPair(t)
	a.dialCoC = func(context.Context, string, string, uint16) (net.Conn, error) { return aRaw, nil }
	b.dialCoC = func(context.Context, string, string, uint16) (net.Conn, error) { return bRaw, nil }
	workers.Add(4)
	go func() { defer workers.Done(); b.acceptLink(ctx, aServer) }()
	go func() { defer workers.Done(); a.acceptLink(ctx, bServer) }()
	go func() {
		defer workers.Done()
		defer aLease.Release()
		a.dialLink(localmesh.WithConnectionLease(ctx, aLease), aPeer)
	}()
	go func() {
		defer workers.Done()
		defer bLease.Release()
		b.dialLink(localmesh.WithConnectionLease(ctx, bLease), bPeer)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ac, bc := a.connectionPolicy().Counts(), b.connectionPolicy().Counts()
		if ac.Total == 1 && bc.Total == 1 && ac.Ready == 1 && bc.Ready == 1 {
			if ac.Outgoing != 0 || bc.Outgoing != 1 {
				t.Fatalf("endpoints retained different direction: %+v %+v", ac, bc)
			}
			// Ensure closure propagation cannot subsequently kill both streams.
			time.Sleep(100 * time.Millisecond)
			if a.cfg.Node.(*duplicateTLSNode).active.Load() != 1 || b.cfg.Node.(*duplicateTLSNode).active.Load() != 1 {
				t.Fatal("closing losing CoC killed winning stream")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("crossed links did not converge: %+v %+v", a.connectionPolicy().Counts(), b.connectionPolicy().Counts())
}

func tcpConnectionPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b, err := listener.Accept()
	if err != nil {
		a.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}
