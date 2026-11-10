//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

type incomingHoldNode struct {
	mu       sync.Mutex
	links    []localmesh.PeerLink
	stop     map[meshid.Device]chan struct{}
	next     atomic.Uint64
	attached chan meshid.Device
	retired  atomic.Int32
}

func (n *incomingHoldNode) Snapshot() localmesh.NodeSnapshot {
	n.mu.Lock()
	defer n.mu.Unlock()
	return localmesh.NodeSnapshot{Links: append([]localmesh.PeerLink(nil), n.links...)}
}

func (n *incomingHoldNode) AttachStream(ctx context.Context, peer meshid.Device, conn net.Conn, cost uint16) error {
	if !localmesh.ConnectionReady(ctx, peer, func() { _ = conn.Close() }) {
		return nil
	}
	id := babel.LinkID(n.next.Add(1))
	retire, stop := make(chan struct{}), make(chan struct{})
	var once sync.Once
	localmesh.BindConnectionLink(ctx, id, cost, func() {
		once.Do(func() { n.retired.Add(1); close(retire) })
	})
	n.mu.Lock()
	n.links = append(n.links, localmesh.PeerLink{ID: id, Device: peer, Cost: cost, Transport: "ble"})
	n.stop[peer] = stop
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		for i, link := range n.links {
			if link.ID == id {
				n.links = append(n.links[:i], n.links[i+1:]...)
				break
			}
		}
		if n.stop[peer] == stop {
			delete(n.stop, peer)
		}
	}()
	n.attached <- peer
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-retire:
		return nil
	case <-stop:
		return nil
	}
}

func (n *incomingHoldNode) stopPeer(peer meshid.Device) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if stop := n.stop[peer]; stop != nil {
		close(stop)
		delete(n.stop, peer)
	}
}

// Run the actual bounded accept loop and TLS admission. The arriving scan-only
// leaf uses the existing five-second failed-dial retry, while an old redundant
// carrier gets its earlier one-second scan opportunity in either direction.
func TestBLEIncomingDemandHoldProtectsNormalLeafRetry(t *testing.T) {
	for _, direction := range []string{"outgoing", "incoming"} {
		t.Run(direction, func(t *testing.T) {
			const target = localmesh.BLEMaxConnections
			self := fixtureDevice("00000001-1234-4000-8000-000000000001")
			serverCert, _ := testTLSIdentity(t)
			certificates := make([]tls.Certificate, target+1)
			devices := make([]meshid.Device, target+1)
			identities := make(map[string]meshid.Device)
			for i := range certificates {
				certificates[i], _ = testTLSIdentity(t)
				id := self.UUID()
				id[0] = byte(i + 1)
				devices[i] = meshid.NewDevice(testTenant, id)
				identities[string(certificates[i].Certificate[0])] = devices[i]
			}
			credentials := &localmesh.Credentials{Device: self, Certificate: serverCert, Verify: func(chain [][]byte, _ time.Time) (localmesh.Identity, error) {
				if len(chain) == 1 {
					if peer, ok := identities[string(chain[0])]; ok {
						return localmesh.Identity{Device: peer}, nil
					}
				}
				return localmesh.Identity{}, errors.New("untrusted fixture identity")
			}}
			node := &incomingHoldNode{attached: make(chan meshid.Device, 16), stop: make(map[meshid.Device]chan struct{}),
				links: []localmesh.PeerLink{{ID: 100, Device: devices[0], Cost: 128, ForwardingReady: true}}}
			r := &runtime{cfg: Config{Credentials: credentials, Node: node, NetworkName: "incoming-hold-fixture", Logger: zap.NewNop()}, advertisementRefresh: make(chan struct{}, 1)}
			if err := r.cfg.defaults(); err != nil {
				t.Fatal(err)
			}
			r.serverTLS = r.makeServerTLS()
			fd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
			if err != nil {
				t.Fatal(err)
			}
			listener := &l2Listener{fd: fd}
			address := &unix.SockaddrUnix{Name: "@wendy-incoming-hold-" + uuid.NewString()}
			if err := unix.Bind(fd, address); err != nil {
				t.Fatal(err)
			}
			if err := unix.Listen(fd, target+1); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*globalDialPace)
			done := make(chan error, 1)
			go func() { done <- r.acceptLoop(ctx, listener) }()
			t.Cleanup(func() {
				cancel()
				_ = listener.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("accept loop did not stop")
				}
				r.links.Wait()
			})
			dial := func(index int, budget time.Duration) error {
				clientFD, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
				if err != nil {
					return err
				}
				raw := newPacketConn(clientFD, l2addr{}, l2addr{})
				t.Cleanup(func() { _ = raw.Close() })
				if err := unix.Connect(clientFD, address); err != nil {
					return err
				}
				client := tls.Client(raw, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificates[index]}, InsecureSkipVerify: true, NextProtos: []string{ALPN}, CurvePreferences: []tls.CurveID{tls.X25519}})
				attempt, stop := context.WithTimeout(ctx, budget)
				defer stop()
				return client.HandshakeContext(attempt)
			}
			waitCount := func(want int, budget time.Duration) bool {
				until := time.Now().Add(budget)
				for time.Now().Before(until) {
					if r.connectionPolicy().Counts().Total == want {
						return true
					}
					time.Sleep(5 * time.Millisecond)
				}
				return r.connectionPolicy().Counts().Total == want
			}
			for i := 0; i < target; i++ {
				if err := dial(i, time.Second); err != nil {
					t.Fatalf("initial peer%d TLS: %v", i, err)
				}
				select {
				case got := <-node.attached:
					if got != devices[i] {
						t.Fatalf("wrong initial peer %v", got)
					}
				case <-ctx.Done():
					t.Fatal("initial peer did not attach")
				}
			}
			if counts := r.connectionPolicy().Counts(); counts.Total != target || counts.Ready != target {
				t.Fatalf("initial capacity %+v", counts)
			}
			if err := dial(target, 500*time.Millisecond); err == nil {
				t.Fatal("full incoming gate admitted a fourth worker")
			}
			failedAt := time.Now()
			if !waitCount(target-1, time.Second) || node.retired.Load() != 1 {
				t.Fatalf("unknown demand did not normally release one safe standby: %+v retires=%d", r.connectionPolicy().Counts(), node.retired.Load())
			}
			// A real authenticated old identity supplies this cache entry. Its
			// radio hint only schedules an attempt; it does not authorize a route.
			old := candidate{address: "AA:BB:CC:DD:EE:01", addressType: "public", psm: DefaultPSM, hint: deviceHint(devices[0].UUID())}
			r.mu.Lock()
			r.known = map[string]meshid.Device{old.key(): devices[0]}
			r.mu.Unlock()
			timer := time.NewTimer(time.Until(failedAt.Add(time.Second)))
			select {
			case <-timer.C:
			case <-ctx.Done():
				t.Fatal("old scan opportunity did not arrive")
			}
			stoleSlot := false
			if direction == "outgoing" {
				if stolen := r.reserveCandidate(old); stolen != nil {
					// Keep the reservation like an in-flight outbound TLS attempt,
					// so the delayed leaf must contend with the actual stolen slot.
					defer stolen.Release()
					stoleSlot = true
				}
			} else {
				_ = dial(0, time.Second)
				if !waitCount(target-1, 500*time.Millisecond) {
					stoleSlot = true
				}
			}
			// This is the ordinary failed-dial retry floor already used by BLE,
			// not an immediate manual retry that can outrun backup rediscovery.
			timer = time.NewTimer(time.Until(failedAt.Add(globalDialPace)))
			select {
			case <-timer.C:
			case <-ctx.Done():
				t.Fatal("normal leaf retry did not arrive")
			}
			if err := dial(target, time.Second); err != nil {
				t.Fatalf("normal delayed leaf retry lost its released slot (old_%s_stole=%v): %v", direction, stoleSlot, err)
			}
			select {
			case got := <-node.attached:
				if got != devices[target] {
					t.Fatalf("redundant old peer stole incoming opportunity before delayed leaf: %v", got)
				}
			case <-ctx.Done():
				t.Fatal("normal delayed leaf did not authenticate and bind")
			}
			if stoleSlot {
				t.Fatalf("old %s redundancy consumed the bounded incoming-demand hold", direction)
			}
			if counts := r.connectionPolicy().Counts(); counts.Total != target || counts.Ready != target || node.retired.Load() != 1 {
				t.Fatalf("leaf admission expanded caps or churned another owner: %+v retires=%d", counts, node.retired.Load())
			}
			// Actual new-peer Bind clears the opportunity hold. Once an unrelated
			// owner releases, spare redundancy is eligible again immediately.
			node.stopPeer(devices[2])
			if !waitCount(target-1, time.Second) {
				t.Fatal("unrelated owner did not normally release")
			}
			if spare := r.reserveCandidate(old); spare == nil {
				t.Fatal("genuine new-peer admission did not clear the bounded hold")
			} else {
				spare.Release()
			}
		})
	}
}
