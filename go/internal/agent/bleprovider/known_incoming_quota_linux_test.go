//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// Two outgoing native owners and one incoming native owner fill the quota,
// while only one of the separate three incoming lifetime workers is occupied.
// Calling acceptLink directly exercises its real ALPN ClientHello quota gate;
// the outer accept-loop gate cannot hide an inappropriate reclamation here.
func TestKnownIncomingClientHelloUsesOnlySpareNativeCapacity(t *testing.T) {
	for _, mode := range []string{"full-known-working", "full-unknown", "spare-known-working", "spare-cache-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			self := fixtureDevice("00000001-1234-4000-8000-000000000001")
			serverCert, _ := testTLSIdentity(t)
			certificates := make([]tls.Certificate, 5)
			devices := make([]meshid.Device, len(certificates))
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
			node := &incomingHoldNode{attached: make(chan meshid.Device, 8), stop: make(map[meshid.Device]chan struct{}), links: []localmesh.PeerLink{
				{ID: 100, Device: devices[0], Cost: 128, ForwardingReady: true},
				{ID: 101, Device: devices[3], Cost: 128, ForwardingReady: true},
			}}
			r := &runtime{cfg: Config{Credentials: credentials, Node: node, NetworkName: "known-native-quota-fixture", Logger: zap.NewNop()}, advertisementRefresh: make(chan struct{}, 1)}
			if err := r.cfg.defaults(); err != nil {
				t.Fatal(err)
			}
			r.serverTLS = r.makeServerTLS()
			ctx, cancel := context.WithCancel(context.Background())
			var workers sync.WaitGroup
			t.Cleanup(func() { cancel(); workers.Wait() })
			policy := r.connectionPolicy()
			// These are authenticated outgoing owners. Their exact bindings and
			// snapshots provide the same evidence the real Node publishes.
			var outgoing []*localmesh.ConnectionLease
			for i, index := range []int{0, 1} {
				if mode == "spare-cache-mismatch" && i == 1 {
					index = 3
				}
				peer := devices[index]
				lease := policy.Reserve(peer.Principal(), peer, true)
				if lease == nil || !lease.Ready(peer, func() { t.Error("reclaimer called physical closer instead of owned retirement") }) {
					t.Fatal("outgoing native fixture missing")
				}
				id := babel.LinkID(500 + i)
				var once sync.Once
				localmesh.BindConnectionLink(localmesh.WithConnectionLease(ctx, lease), id, LinkCost, func() { once.Do(func() { node.retired.Add(1) }) })
				node.mu.Lock()
				node.links = append(node.links, localmesh.PeerLink{ID: id, Device: peer, Cost: LinkCost, Transport: "ble"})
				node.mu.Unlock()
				outgoing = append(outgoing, lease)
				t.Cleanup(lease.Release)
			}
			remote := l2addr{Address: "AA:BB:CC:DD:EE:03", Type: btAddrLEPublic, PSM: DefaultPSM}
			dial := func(index int, radio l2addr) (error, <-chan struct{}) {
				fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, 0)
				if err != nil {
					t.Fatal(err)
				}
				server := newPacketConn(fds[0], l2addr{}, radio)
				clientRaw := newPacketConn(fds[1], radio, l2addr{})
				t.Cleanup(func() { _ = server.Close(); _ = clientRaw.Close() })
				done := make(chan struct{})
				workers.Add(1)
				go func() { defer workers.Done(); defer close(done); r.acceptLink(ctx, server) }()
				client := tls.Client(clientRaw, &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificates[index]}, InsecureSkipVerify: true, NextProtos: []string{ALPN}, CurvePreferences: []tls.CurveID{tls.X25519}})
				budget, stop := context.WithTimeout(ctx, time.Second)
				defer stop()
				return client.HandshakeContext(budget), done
			}
			full := mode == "full-known-working" || mode == "full-unknown"
			if full {
				if err, _ := dial(2, remote); err != nil {
					t.Fatalf("initial incoming native TLS: %v", err)
				}
				select {
				case peer := <-node.attached:
					if peer != devices[2] {
						t.Fatal("wrong initial incoming identity")
					}
				case <-time.After(time.Second):
					t.Fatal("initial incoming native owner missing")
				}
			}
			wantBefore := 2
			if full {
				wantBefore = 3
			}
			if counts := policy.Counts(); counts.Total != wantBefore || counts.Ready != wantBefore || counts.Outgoing != 2 {
				t.Fatalf("wrong native/outer setup %+v", counts)
			}
			probeRadio := l2addr{Address: "AA:BB:CC:DD:EE:B0", Type: btAddrLEPublic, PSM: DefaultPSM}
			if mode != "full-unknown" {
				r.mu.Lock()
				if r.known == nil {
					r.known = make(map[string]meshid.Device)
				}
				r.known[(candidate{address: probeRadio.Address, addressType: "public"}).key()] = devices[3]
				r.mu.Unlock()
			}
			actual := 3
			if mode == "spare-cache-mismatch" {
				actual = 4
			}
			err, done := dial(actual, probeRadio)
			if full {
				if err == nil {
					t.Fatal("full native quota admitted another owner")
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("rejected ClientHello worker did not finish")
				}
				if mode == "full-known-working" {
					if node.retired.Load() != 0 || outgoing[0].Retired() || outgoing[1].Retired() {
						t.Fatal("known working peer ClientHello evicted unrelated safe backup without spare capacity")
					}
				} else if node.retired.Load() != 1 || !outgoing[0].Retired() || outgoing[1].Retired() {
					t.Fatal("unknown full-quota probe did not reach only the safe owned backup")
				}
				if policy.Counts().Total != 3 {
					t.Fatal("rejected probe borrowed or prematurely released native capacity")
				}
				return
			}
			if err != nil {
				t.Fatalf("spare native quota rejected authenticated identity: %v", err)
			}
			select {
			case peer := <-node.attached:
				if peer != devices[actual] {
					t.Fatalf("cache hint substituted TLS identity: got%v want%v", peer, devices[actual])
				}
			case <-time.After(time.Second):
				t.Fatal("spare incoming identity did not bind")
			}
			if counts := policy.Counts(); counts.Total != 3 || counts.Ready != 3 || node.retired.Load() != 0 {
				t.Fatalf("spare admission changed caps or retired an owner: %+v retires%d", counts, node.retired.Load())
			}
		})
	}
}
