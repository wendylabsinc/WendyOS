package localmesh_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

func connectPublicationLossRuntimes(t *testing.T, a, b *clockPacketRuntime, outgoingA, outgoingB func(context.Context, localmesh.ControlMessage, string) bool) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer listener.Close()
	rawA, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	rawB, err := listener.Accept()
	if err != nil {
		rawA.Close()
		cancel()
		t.Fatal(err)
	}
	aConfig, err := a.node.Credentials.PeerTLS(b.node.Credentials.Device)
	if err != nil {
		rawA.Close()
		rawB.Close()
		cancel()
		t.Fatal(err)
	}
	bConfig, err := b.node.Credentials.PeerTLS(a.node.Credentials.Device)
	if err != nil {
		rawA.Close()
		rawB.Close()
		cancel()
		t.Fatal(err)
	}
	aConfig.NextProtos, bConfig.NextProtos = []string{localmesh.StreamALPN}, []string{localmesh.StreamALPN}
	client, server := tls.Client(rawA, aConfig), tls.Server(rawB, bConfig)
	handshakes := make(chan error, 2)
	go func() { handshakes <- client.HandshakeContext(ctx) }()
	go func() { handshakes <- server.HandshakeContext(ctx) }()
	for range 2 {
		if err := <-handshakes; err != nil {
			client.Close()
			server.Close()
			cancel()
			t.Fatal("clock fixture TLS", err)
		}
	}
	ends := make(chan error, 2)
	go func() {
		ends <- localmesh.AttachPublicationLossStream(ctx, a.node, b.node.Credentials.Device, client, outgoingA)
	}()
	go func() {
		ends <- localmesh.AttachPublicationLossStream(ctx, b.node, a.node.Credentials.Device, server, outgoingB)
	}()
	var once sync.Once
	closeLink := func() {
		once.Do(func() {
			cancel()
			client.Close()
			server.Close()
			for range 2 {
				select {
				case <-ends:
				case <-time.After(5 * time.Second):
					t.Error("clock TLS carrier did not drain")
				}
			}
		})
	}
	t.Cleanup(closeLink)
	return closeLink
}

// Four actual TLS/packet runtimes, with loss only at the final control-frame
// write boundary. No clock shortening, injected manifests, TUN or radio.
func TestPublicationLossDiamondRecoversWithCurrentLeaf(t *testing.T) {
	if os.Getenv("WENDY_E2E_PUBLICATION_LOSS") != "1" {
		t.Skip("run test-publication-loss-packets.sh")
	}
	credentials := localmesh.PublicationLossCredentials(t)
	principal := credentials[0].Device.Principal()
	directory := filepath.Join(t.TempDir(), "publisher")
	a := startServicePacketRuntime(t, directory, credentials[0])
	b := startServicePacketRuntime(t, filepath.Join(t.TempDir(), "primary"), credentials[1])
	c := startServicePacketRuntime(t, filepath.Join(t.TempDir(), "alternate"), credentials[2])
	d := startServicePacketRuntime(t, filepath.Join(t.TempDir(), "receiver"), credentials[3])
	remote := serviceCatalog(t, d)
	spec := meshcatalog.PublishSpec{AppID: "com.wendy.publication-regression", ServiceID: "temperature", Type: "_http._tcp", Instance: "Diamond publication", HostPort: 18475}
	publish := func(r *clockPacketRuntime) {
		t.Helper()
		catalog := serviceCatalog(t, r)
		if _, err := catalog.Publish(spec, time.Now()); err != nil {
			t.Fatal(err)
		}
		clockWait(t, func() bool { return len(catalog.Snapshot(time.Now())) == 1 }, "publisher did not finish its full service manifest")
	}
	publish(a)
	closeEcho := clockEchoServer(t, a.network)
	clockEchoServer(t, b.network)
	var mu sync.Mutex
	armed := false
	dropped := make(chan struct{})
	var dropOnce sync.Once
	var lossTime time.Time
	digest := ""
	primaryWants, alternateWants := make(map[string]int), make(map[string]int)
	dataDrops := 0
	alternateData := make(map[string]int)
	var alternateAt time.Time
	target := func(m localmesh.ControlMessage, origin string) bool {
		return m.Gossip != nil && m.Gossip.Type == netgossip.Manifest && origin == principal
	}
	primaryFilter := func(_ context.Context, m localmesh.ControlMessage, origin string) bool {
		mu.Lock()
		defer mu.Unlock()
		if armed && target(m, origin) && m.Gossip.Op == "data" {
			if digest == "" {
				digest = string(m.Gossip.Digest)
				lossTime = time.Now()
				dropOnce.Do(func() { close(dropped) })
			}
			dataDrops++
			return true // No other primary publication may bypass this loss window.
		}
		return false
	}
	alternateFilter := func(ctx context.Context, m localmesh.ControlMessage, origin string) bool {
		mu.Lock()
		hold := armed && target(m, origin) && m.Gossip.Op == "have"
		if armed && target(m, origin) && m.Gossip.Op == "data" {
			alternateData[string(m.Gossip.Digest)]++
		}
		mu.Unlock()
		if hold {
			select {
			case <-dropped:
			case <-ctx.Done():
				return true
			}
		}
		return false
	}
	observer := func(primary bool) func(context.Context, localmesh.ControlMessage, string) bool {
		return func(_ context.Context, m localmesh.ControlMessage, _ string) bool {
			mu.Lock()
			defer mu.Unlock()
			if armed && m.Gossip != nil && m.Gossip.Op == "want" {
				id := string(m.Gossip.Digest)
				if primary {
					primaryWants[id]++
				} else {
					alternateWants[id]++
					if id == digest && alternateAt.IsZero() {
						alternateAt = time.Now()
					}
				}
			}
			return false
		}
	}
	connectPublicationLossRuntimes(t, b, d, primaryFilter, observer(true))
	connectPublicationLossRuntimes(t, c, d, alternateFilter, observer(false))
	ab := connectPublicationLossRuntimes(t, a, b, nil, nil)
	ac := connectPublicationLossRuntimes(t, a, c, nil, nil)
	waitAccepted := func(leaf []byte) bool {
		rows := remote.Snapshot(time.Now())
		return len(rows) == 1 && rows[0].Key.Device == principal && rows[0].HostPort == 18475 && bytes.Equal(localmesh.ServiceRuntimeAcceptedLeaf(d.node, principal), leaf) && clockHasPeerRoute(d, a)
	}
	clockWait(t, func() bool { return waitAccepted(credentials[0].Certificate.Certificate[0]) }, "diamond did not propagate original publication")
	dial := func(to *clockPacketRuntime) (net.Conn, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return d.network.DialContext(ctx, net.JoinHostPort(to.node.Credentials.Device.MeshAddress().String(), "18475"))
	}
	echo := func(to *clockPacketRuntime, marker string) {
		t.Helper()
		conn, err := dial(to)
		if err != nil {
			t.Fatalf("diamond live TCP %s: %v", marker, err)
		}
		defer conn.Close()
		clockRoundTrip(t, conn, marker)
	}
	echo(a, "old-key-through-diamond")
	// Both suppliers must own the old publication before the durable restart.
	clockWait(t, func() bool {
		return bytes.Equal(localmesh.ServiceRuntimeAcceptedLeaf(b.node, principal), credentials[0].Certificate.Certificate[0]) && bytes.Equal(localmesh.ServiceRuntimeAcceptedLeaf(c.node, principal), credentials[0].Certificate.Certificate[0])
	}, "both relay suppliers did not warm")
	closeEcho()
	ab()
	ac()
	a.stop(t)
	revision := localmesh.ClockRuntimeRevision(t, a.node)
	rotated := localmesh.RotatePublicationLossKey(t, credentials[0])
	oldLeaf, _ := x509.ParseCertificate(credentials[0].Certificate.Certificate[0])
	newLeaf, _ := x509.ParseCertificate(rotated.Certificate.Certificate[0])
	if rotated.Device != credentials[0].Device || bytes.Equal(oldLeaf.RawSubjectPublicKeyInfo, newLeaf.RawSubjectPublicKeyInfo) || !newLeaf.NotAfter.After(oldLeaf.NotAfter) {
		t.Fatal("fixture did not rotate the same principal's signing key")
	}
	a = startServicePacketRuntime(t, directory, rotated)
	publish(a)
	closeEcho = clockEchoServer(t, a.network)
	mu.Lock()
	armed = true
	mu.Unlock()
	ab = connectPublicationLossRuntimes(t, a, b, nil, nil)
	ac = connectPublicationLossRuntimes(t, a, c, nil, nil)
	select {
	case <-dropped:
	case <-time.After(12 * time.Second):
		t.Fatal("primary supplier did not encounter controlled DATA loss")
	}
	clockWait(t, func() bool {
		return bytes.Equal(localmesh.PublicationLossCurrentLeaf(d.node, principal), rotated.Certificate.Certificate[0])
	}, "receiver never verified new key before lost publication")
	// A current identity alone cannot authorize an old-key signed publication.
	if leaf := localmesh.ServiceRuntimeAcceptedLeaf(d.node, principal); len(leaf) != 0 {
		t.Fatal("obsolete publication retained authority after verified key rotation")
	}
	clockWait(t, func() bool { return len(remote.Snapshot(time.Now())) == 0 && !clockHasPeerRoute(d, a) }, "obsolete publication service/route remained after key rotation")
	if conn, err := dial(a); err == nil {
		conn.Close()
		t.Fatal("old-key publication still allowed application TCP")
	}
	echo(b, "unrelated-neighbor-during-publication-loss")
	t.Log("new key verified; old publication authority and application TCP withdrawn while unrelated TCP works")
	mu.Lock()
	started := lossTime
	mu.Unlock()
	deadline := started.Add(netgossip.IdleTimeout + 12*time.Second)
	recovered := func() bool {
		mu.Lock()
		expected := digest
		supplied := alternateData[digest] > 0
		mu.Unlock()
		return supplied && bytes.Equal(localmesh.PublicationLossAcceptedDigest(d.node, principal), []byte(expected)) && waitAccepted(rotated.Certificate.Certificate[0])
	}
	for !recovered() {
		if time.Now().After(deadline) {
			t.Fatal("timed-out primary claim did not recover current-leaf publication through alternate supplier")
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	elapsed := alternateAt.Sub(started)
	pw, aw, drops := primaryWants[digest], alternateWants[digest], dataDrops
	mu.Unlock()
	if aw != 1 || pw != 1 || drops == 0 || elapsed < netgossip.IdleTimeout-time.Second || elapsed > netgossip.IdleTimeout+10*time.Second {
		t.Fatalf("unbounded/shared claim ownership: primary wants=%d alternate wants=%d drops=%d alternate delay=%v", pw, aw, drops, elapsed)
	}
	if localmesh.ClockRuntimeRevision(t, a.node) <= revision {
		t.Fatal("publisher restart did not advance durable origin")
	}
	echo(a, "new-key-after-alternate-download")
	closeEcho()
	ab()
	ac()
	t.Logf("production idle timeout recovered exact current-leaf publication and live TCP: delay=%v primary=%d alternate=%d", elapsed, pw, aw)
}
