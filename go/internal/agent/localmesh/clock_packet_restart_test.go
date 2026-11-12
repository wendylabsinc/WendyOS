package localmesh_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh"
)

type clockPacketRuntime struct {
	node    *localmesh.Node
	network *netmesh.Network
	clock   *localmesh.ClockRuntimeFixture
	cancel  context.CancelFunc
	done    chan error
	once    sync.Once
}

func startClockPacketRuntime(t *testing.T, directory string, ordinal int, anchor, wall time.Time) *clockPacketRuntime {
	t.Helper()
	fixture, credentials := localmesh.OpenClockRuntimeFixture(t, directory, ordinal, anchor, wall)
	snapshot := fixture.Clock.Snapshot()
	if snapshot.Source != "systemd-saved-clock" || !snapshot.Uncertain || snapshot.Anchor.Before(anchor) || snapshot.Anchor.After(anchor.Add(time.Second)) {
		t.Fatalf("persisted local clock trust did not survive runtime construction: %+v", snapshot)
	}
	network, err := netmesh.NewNetwork(credentials.Device)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	node := localmesh.NewClockRuntimeNode(t, ctx, directory, credentials, network, fixture)
	runtime := &clockPacketRuntime{node: node, network: network, clock: fixture, cancel: cancel, done: make(chan error, 1)}
	go func() { runtime.done <- node.Run() }()
	t.Cleanup(func() { runtime.stop(t) })
	return runtime
}

func (r *clockPacketRuntime) stop(t *testing.T) {
	t.Helper()
	r.once.Do(func() {
		r.cancel()
		select {
		case err := <-r.done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("clock runtime stop: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("clock runtime did not drain")
		}
		if err := r.network.Close(); err != nil {
			t.Error(err)
		}
	})
}

func connectClockPacketRuntimes(t *testing.T, a, b *clockPacketRuntime) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
	go func() { ends <- a.node.AttachStream(ctx, b.node.Credentials.Device, client, 256) }()
	go func() { ends <- b.node.AttachStream(ctx, a.node.Credentials.Device, server, 256) }()
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

func clockEchoServer(t *testing.T, network *netmesh.Network) func() {
	t.Helper()
	listener, err := network.ListenTCP(18475)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	var mu sync.Mutex
	connections := make(map[net.Conn]bool)
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections[conn] = true
			mu.Unlock()
			workers.Add(1)
			go func() { defer workers.Done(); defer conn.Close(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	var once sync.Once
	closeServer := func() {
		once.Do(func() {
			listener.Close()
			<-acceptDone
			mu.Lock()
			for conn := range connections {
				conn.Close()
			}
			mu.Unlock()
			workers.Wait()
		})
	}
	t.Cleanup(closeServer)
	return closeServer
}

func clockRoundTrip(t *testing.T, conn net.Conn, marker string) {
	t.Helper()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	payload := []byte(marker)
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("wall clock jump disrupted live mesh TCP: %s: %v", marker, err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("wall clock jump disrupted live mesh TCP: %s: received %q: %v", marker, got, err)
	}
}

func clockWait(t *testing.T, condition func() bool, description string) {
	t.Helper()
	deadline := time.Now().Add(12 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal(description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func clockHasPeerRoute(a, b *clockPacketRuntime) bool {
	for _, route := range a.node.Snapshot().ForwardingRoutes {
		if !route.Local && !route.Unreachable && route.Prefix.Addr() == b.node.Credentials.Device.MeshAddress() {
			return true
		}
	}
	return false
}

// No host clock changes, TUN devices, radio, GUI or OS reboot. Both complete
// runtimes use the production gVisor network, TLS, Babel, Directory and disk
// stores. Advancing the existing receiver clock models elapsed suspend time.
func TestClockTrustRuntimeRestartPreservesPacketExpiryAndRecovery(t *testing.T) {
	if os.Getenv("WENDY_E2E_CLOCK_PACKET_RESTART") != "1" {
		t.Skip("run through test-clock-packet-restart.sh in the existing network job")
	}
	anchor := time.Now().Truncate(time.Second)
	dirs := []string{filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")}
	a := startClockPacketRuntime(t, dirs[0], 0, anchor, anchor.Add(-10*time.Minute))
	b := startClockPacketRuntime(t, dirs[1], 1, anchor, anchor.Add(-10*time.Minute))
	closeEchoA, closeEchoB := clockEchoServer(t, a.network), clockEchoServer(t, b.network)
	closeLink := connectClockPacketRuntimes(t, a, b)
	clockWait(t, func() bool { return clockHasPeerRoute(a, b) && clockHasPeerRoute(b, a) }, "two authenticated nodes did not install packet routes")
	dial := func(from, to *clockPacketRuntime) net.Conn {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn, err := from.network.DialContext(ctx, net.JoinHostPort(to.node.Credentials.Device.MeshAddress().String(), "18475"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	ab, ba := dial(a, b), dial(b, a)
	clockRoundTrip(t, ab, "before-clock-jump-a-to-b")
	clockRoundTrip(t, ba, "before-clock-jump-b-to-a")
	for _, wall := range []time.Time{anchor.Add(30 * time.Minute), time.Unix(1, 0)} {
		changed := time.Now()
		a.clock.SetWall(wall)
		b.clock.SetWall(wall)
		// Wait for both real actors to project a snapshot after the clock jump.
		clockWait(t, func() bool {
			return a.node.Snapshot().CapturedAt.After(changed) && b.node.Snapshot().CapturedAt.After(changed)
		}, "clock jump was not observed by both running actors")
		clockRoundTrip(t, ab, "after-wall-jump-a-to-b-"+wall.Format(time.RFC3339))
		clockRoundTrip(t, ba, "after-wall-jump-b-to-a-"+wall.Format(time.RFC3339))
	}
	// Preserve the exact full production 15-minute lease. Only the already
	// injectable receiver/boot clocks advance; transport and packet I/O stay live.
	a.clock.AdvanceElapsed(localmesh.MaxLease + time.Second)
	b.clock.AdvanceElapsed(localmesh.MaxLease + time.Second)
	for _, runtime := range []*clockPacketRuntime{a, b} {
		snapshot := runtime.clock.Clock.Snapshot()
		if snapshot.Anchor.Before(anchor.Add(localmesh.MaxLease+time.Second)) || snapshot.Wall.After(time.Unix(60, 0)) || !snapshot.Uncertain {
			t.Fatalf("elapsed clock did not progress persisted trust without changing wall or certainty: %+v", snapshot)
		}
	}
	clockWait(t, func() bool {
		return !clockHasPeerRoute(a, b) && !clockHasPeerRoute(b, a) && len(a.node.Snapshot().Devices) == 0 && len(b.node.Snapshot().Devices) == 0
	}, "elapsed suspend retained expired announcement authority or forwarding")
	if a.node.Snapshot().Peers != 1 || b.node.Snapshot().Peers != 1 {
		t.Fatal("expiry assertion lost the carrier instead of expiring receiver leases")
	}
	if err := ab.SetDeadline(time.Now().Add(250 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := ab.Write([]byte("expired-route-must-not-echo")); err != nil {
		t.Fatal(err)
	}
	var reply [1]byte
	n, err := ab.Read(reply[:])
	var timeout net.Error
	if n != 0 || !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatal("expired receiver lease did not block application bytes until deadline", n, err)
	}
	ab.Close()
	ba.Close()
	closeEchoA()
	closeEchoB()
	closeLink()
	a.stop(t)
	b.stop(t)
	revisions := []uint64{localmesh.ClockRuntimeRevision(t, a.node), localmesh.ClockRuntimeRevision(t, b.node)}
	floors := make([][32]byte, 2)
	for i, dir := range dirs {
		data, err := os.ReadFile(filepath.Join(dir, "clock_floor"))
		if err != nil {
			t.Fatal(err)
		}
		floors[i] = sha256.Sum256(data)
	}
	// Reconstruct both runtimes from their same private files with a reboot-like
	// elapsed clock reset and an ancient RTC. No foreign RAM lease is restored.
	a = startClockPacketRuntime(t, dirs[0], 0, anchor, time.Unix(1, 0))
	b = startClockPacketRuntime(t, dirs[1], 1, anchor, time.Unix(1, 0))
	for i, r := range []*clockPacketRuntime{a, b} {
		clockWait(t, func() bool { return len(r.node.Snapshot().Devices) == 1 }, "restart restored foreign presence or failed own publication")
		if localmesh.ClockRuntimeRevision(t, r.node) <= revisions[i] {
			t.Fatal("restart did not advance the durable origin revision")
		}
		data, err := os.ReadFile(filepath.Join(dirs[i], "clock_floor"))
		if err != nil || sha256.Sum256(data) != floors[i] {
			t.Fatal("runtime changed persisted clock floor", err)
		}
	}
	clockEchoServer(t, a.network)
	clockEchoServer(t, b.network)
	connectClockPacketRuntimes(t, a, b)
	clockWait(t, func() bool { return clockHasPeerRoute(a, b) && clockHasPeerRoute(b, a) }, "fresh post-restart proof did not restore packet routes")
	clockRoundTrip(t, dial(a, b), "recovered-after-persisted-clock-restart-a-to-b")
	clockRoundTrip(t, dial(b, a), "recovered-after-persisted-clock-restart-b-to-a")
}
