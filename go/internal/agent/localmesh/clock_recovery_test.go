package localmesh

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

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
	"github.com/wendylabsinc/wendy/go/internal/testfixture/clockproof"
)

// clockPacketNetwork creates no host interfaces, routes or clocks. A drained
// pipe supplies the packet interface exercised by two real Node event loops.
type clockPacketNetwork struct {
	mu    sync.Mutex
	peers map[babel.LinkID]net.Conn
	added atomic.Int32
}

func (*clockPacketNetwork) ReconcileBase() error { return nil }
func (n *clockPacketNetwork) AddPeer(id babel.LinkID, _ meshid.Device) (io.ReadWriteCloser, error) {
	a, b := net.Pipe()
	n.mu.Lock()
	if n.peers == nil {
		n.peers = make(map[babel.LinkID]net.Conn)
	}
	n.peers[id] = b
	n.mu.Unlock()
	n.added.Add(1)
	go func() { _, _ = io.Copy(io.Discard, b); _ = b.Close() }()
	return a, nil
}
func (n *clockPacketNetwork) RemovePeer(id babel.LinkID) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if c := n.peers[id]; c != nil {
		_ = c.Close()
		delete(n.peers, id)
	}
	return nil
}
func (*clockPacketNetwork) Apply([]babel.Route) error { return nil }
func (n *clockPacketNetwork) Stop() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	for id, c := range n.peers {
		_ = c.Close()
		delete(n.peers, id)
	}
	return nil
}
func (n *clockPacketNetwork) Close() error { return n.Stop() }

func clockWait(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(time.Millisecond)
	}
}

func clockAttachPair(t *testing.T, ctx context.Context, a, b *Node) <-chan error {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	serverTLS, err := b.Credentials.PeerTLS(a.Credentials.Device)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS.NextProtos = []string{StreamALPN}
	clientTLS, err := a.Credentials.PeerTLS(b.Credentials.Device)
	if err != nil {
		t.Fatal(err)
	}
	clientTLS.NextProtos = []string{StreamALPN}
	ended := make(chan error, 2)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			ended <- err
			return
		}
		conn := tls.Server(raw, serverTLS)
		if err = conn.HandshakeContext(ctx); err == nil {
			err = b.AttachStream(ctx, a.Credentials.Device, conn, 1024)
		}
		_ = conn.Close()
		ended <- err
	}()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	conn := tls.Client(raw, clientTLS)
	if err = conn.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	go func() { ended <- a.AttachStream(ctx, b.Credentials.Device, conn, 1024) }()
	return ended
}

// Both fresh and stale Nodes really boot, sign their own leases, mutually
// authenticate TLS and exchange control messages. Only wall UTC and the public
// authority transport are simulated. The healthy end sees an expired lease;
// the stale end sees a future lease. Ordinary expiry rules stay unchanged.
func TestClockRecoveryColdBootTwoNodes(t *testing.T) {
	for _, skew := range []time.Duration{598 * time.Second, 1200 * time.Second} {
		for _, mode := range []struct{ recoverClock, noAdvance bool }{{false, false}, {true, false}, {true, true}} {
			recoverClock := mode.recoverClock
			t.Run(fmt.Sprintf("skew=%s/recovery=%t/noAdvance=%t", skew, recoverClock, mode.noAdvance), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				staleCredentials, healthyCredentials := testCredentials(t)
				var offset atomic.Int64
				offset.Store(int64(-skew))
				staleWall := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
				staleNetwork, healthyNetwork := &clockPacketNetwork{}, &clockPacketNetwork{}
				stale, err := newNodeWithClock(ctx, t.TempDir(), staleCredentials, "stale", 50052, func(meshid.Device) (PacketNetwork, error) { return staleNetwork, nil }, staleWall)
				if err != nil {
					t.Fatal(err)
				}
				healthy, err := newNodeWithClock(ctx, t.TempDir(), healthyCredentials, "healthy", 50052, func(meshid.Device) (PacketNetwork, error) { return healthyNetwork, nil }, nil)
				if err != nil {
					t.Fatal(err)
				}
				proofReady, release := make(chan struct{}), make(chan struct{})
				var requests atomic.Int32
				if recoverClock {
					authorities := make([]clockproof.Authority, len(roughtime.Servers))
					servers := make([]roughtime.Server, len(authorities))
					for i := range authorities {
						authorities[i] = clockproof.New(t, fmt.Sprintf("authority%d", i), time.Now())
						servers[i] = authorities[i].Server
					}
					healthy.clockQuery = func(ctx context.Context, index uint8, nonce []byte) ([]byte, error) {
						requests.Add(1)
						return authorities[index].ReplyAt(nonce, time.Now()), nil
					}
					healthy.clockSync = func(context.Context, roughtime.RelayQuery) error {
						return errors.New("healthy node must not recover from an expired lease")
					}
					stale.clockSync = func(ctx context.Context, relay roughtime.RelayQuery) error {
						consensus, err := timesync.QueryConsensusVia(ctx, servers, relay)
						if err != nil {
							return err
						}
						if consensus.Quorum < 2 {
							return errors.New("no independent authority quorum")
						}
						close(proofReady)
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
						// Manager.SyncVia's real advance-only apply is tested separately.
						// Here advance only this Node's fake UTC after verified quorum.
						if !mode.noAdvance {
							boot, err := timesync.BootTime()
							if err != nil {
								return err
							}
							target := time.Unix(0, boot+consensus.LowerOffsetNanos+(consensus.UpperOffsetNanos-consensus.LowerOffsetNanos)/2)
							offset.Store(int64(target.Sub(time.Now())))
						}
						return nil
					}
				}
				runDone := make(chan error, 2)
				go func() { runDone <- stale.Run() }()
				go func() { runDone <- healthy.Run() }()
				t.Cleanup(func() {
					cancel()
					for range 2 {
						select {
						case <-runDone:
						case <-time.After(2 * time.Second):
							t.Error("Node did not stop")
						}
					}
				})
				clockWait(t, "both own signed leases", func() bool { return len(stale.Snapshot().Devices) == 1 && len(healthy.Snapshot().Devices) == 1 })
				ended := clockAttachPair(t, ctx, stale, healthy)
				if !recoverClock {
					select {
					case <-ended:
					case <-ctx.Done():
						t.Fatal("baseline did not reject future manifest")
					}
					if len(stale.Snapshot().Devices) != 1 || offset.Load() == 0 {
						t.Fatal("baseline admitted future claim or corrected its clock")
					}
					return
				}
				select {
				case <-proofReady:
				case err := <-ended:
					t.Fatalf("carrier ended before public proof: %v", err)
				case <-ctx.Done():
					t.Fatal("no authenticated authority proof")
				}
				if requests.Load() < 2 {
					t.Fatal("proof did not traverse the healthy pre-admission relay")
				}
				clockWait(t, "healthy ordinary lease handling", func() bool {
					return len(healthy.Snapshot().Devices) == map[bool]int{true: 1, false: 2}[skew >= MaxLease]
				})
				if len(stale.Snapshot().Devices) != 1 || (skew >= MaxLease && len(healthy.Snapshot().Devices) != 1) {
					t.Fatalf("time-invalid lease admitted before clock recovery: stale=%+v healthy=%+v", stale.Snapshot().Devices, healthy.Snapshot().Devices)
				}
				select {
				case err := <-ended:
					t.Fatalf("carrier ended before correction: %v", err)
				default:
				}
				close(release)
				if mode.noAdvance {
					clockWait(t, "no-op callback completion", func() bool {
						for _, m := range stale.Snapshot().Devices {
							if m.Device == staleCredentials.Device.UUID() {
								return m.Revision >= 2
							}
						}
						return false
					})
					if len(stale.Snapshot().Devices) != 1 || offset.Load() == 0 {
						t.Fatal("successful callback without UTC correction admitted future claim")
					}
					return
				}
				clockWait(t, "mutual ordinary manifest admission after correction", func() bool { return len(stale.Snapshot().Devices) == 2 && len(healthy.Snapshot().Devices) == 2 })
				if staleNetwork.added.Load() != 1 || healthyNetwork.added.Load() != 1 {
					t.Fatal("clock recovery recreated the carrier interface")
				}
				select {
				case err := <-ended:
					t.Fatalf("carrier ended after correction: %v", err)
				default:
				}
			})
		}
	}
}
