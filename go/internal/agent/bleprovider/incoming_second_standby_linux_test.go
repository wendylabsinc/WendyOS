//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// This actual accept-loop/TLS progression exercises a different standby from
// the retired victim. Two normal owner releases permit one recovery, while the
// unrelated standby must leave the final slot for the leaf's normal retry.
func TestBLEIncomingDemandHoldRejectsUnrelatedStandbyAfterOrdinaryRelease(t *testing.T) {
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
				links: []localmesh.PeerLink{{ID: 100, Device: devices[0], Cost: 128, ForwardingReady: true}, {ID: 101, Device: devices[1], Cost: 128, ForwardingReady: true}}}
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
			// A different surviving standby completes an ordinary lifetime release.
			// This makes TWO free slots: one may be used by existing-peer recovery,
			// while only the last remains reserved for the unserved arriving leaf.
			node.stopPeer(devices[1])
			if !waitCount(target-2, time.Second) {
				t.Fatal("second standby did not complete its ordinary owner release")
			}
			// The original victim now loses its remaining direct path. Its real
			// authenticated recovery must be allowed without erasing the receipt
			// merely because it currently has no other direct snapshot presence.
			node.mu.Lock()
			for i, link := range node.links {
				if link.ID == 100 {
					node.links = append(node.links[:i], node.links[i+1:]...)
					break
				}
			}
			node.mu.Unlock()
			// A real authenticated old identity supplies this cache entry. Its
			// radio hint only schedules an attempt; it does not authorize a route.
			old := candidate{address: "AA:BB:CC:DD:EE:02", addressType: "public", psm: DefaultPSM, hint: deviceHint(devices[1].UUID())}
			r.mu.Lock()
			r.known = map[string]meshid.Device{old.key(): devices[1]}
			r.mu.Unlock()
			timer := time.NewTimer(time.Until(failedAt.Add(time.Second)))
			select {
			case <-timer.C:
			case <-ctx.Done():
				t.Fatal("old scan opportunity did not arrive")
			}
			if err := dial(0, time.Second); err != nil {
				t.Fatalf("victim recovery could not use the extra unreserved slot: %v", err)
			}
			select {
			case got := <-node.attached:
				if got != devices[0] {
					t.Fatalf("wrong first spare-slot recovery: %v", got)
				}
			case <-ctx.Done():
				t.Fatal("victim recovery did not authenticate and Bind")
			}
			if counts := r.connectionPolicy().Counts(); counts.Total != target-1 || counts.Ready != target-1 {
				t.Fatalf("recovery did not leave exactly the held final slot: %+v", counts)
			}
			// This competitor is NOT the retired victim: it is the unrelated
			// standby whose ordinary Release above completed before recovery.
			stoleSlot := false
			if direction == "outgoing" {
				if stolen := r.reserveCandidate(old); stolen != nil {
					// Keep the reservation like an in-flight outbound TLS attempt,
					// so the delayed leaf must contend with the actual stolen slot.
					defer stolen.Release()
					stoleSlot = true
				}
			} else {
				_ = dial(1, time.Second)
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
				t.Fatalf("normal delayed leaf retry lost its released slot (unrelated_%s_stole=%v): %v", direction, stoleSlot, err)
			}
			select {
			case got := <-node.attached:
				if got != devices[target] {
					t.Fatalf("unrelated standby stole incoming opportunity after victim recovery: %v", got)
				}
			case <-ctx.Done():
				t.Fatal("normal delayed leaf did not authenticate and bind")
			}
			if stoleSlot {
				t.Fatalf("unrelated %s redundancy consumed the last incoming-demand slot", direction)
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
