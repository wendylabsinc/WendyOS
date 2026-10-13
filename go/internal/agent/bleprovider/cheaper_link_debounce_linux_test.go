//go:build linux

package bleprovider

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"go.uber.org/zap"
)

// closeFlagConn records Close without needing a live peer: watchCheaperLink
// only ever calls Close on the connection.
type closeFlagConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *closeFlagConn) Close() error {
	c.closed.Store(true)
	return nil
}

func (c *closeFlagConn) Read([]byte) (int, error)         { return 0, net.ErrClosed }
func (c *closeFlagConn) Write([]byte) (int, error)        { return 0, net.ErrClosed }
func (c *closeFlagConn) LocalAddr() net.Addr              { return nil }
func (c *closeFlagConn) RemoteAddr() net.Addr             { return nil }
func (c *closeFlagConn) SetDeadline(time.Time) error      { return nil }
func (c *closeFlagConn) SetReadDeadline(time.Time) error  { return nil }
func (c *closeFlagConn) SetWriteDeadline(time.Time) error { return nil }

// vetoFixture drives AllowRadio deterministically: while vetoed the snapshot
// holds three distinct peers that exclude the asset under test, so
// AllowRadio returns false; clearing the snapshot lifts the veto.
type vetoFixture struct {
	mu       sync.Mutex
	snapshot localmesh.NodeSnapshot
	sel      *localmesh.PeerSelection
}

func newVetoFixture(vetoed bool) *vetoFixture {
	f := &vetoFixture{}
	if vetoed {
		f.snapshot = localmesh.NodeSnapshot{Links: []localmesh.PeerLink{
			{Asset: 1, Cost: 512}, {Asset: 2, Cost: 512}, {Asset: 3, Cost: 512},
		}}
	}
	f.sel = localmesh.NewPeerSelection(func() localmesh.NodeSnapshot {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.snapshot
	})
	return f
}

func (f *vetoFixture) setVeto(vetoed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vetoed {
		f.snapshot = localmesh.NodeSnapshot{Links: []localmesh.PeerLink{
			{Asset: 1, Cost: 512}, {Asset: 2, Cost: 512}, {Asset: 3, Cost: 512},
		}}
	} else {
		f.snapshot = localmesh.NodeSnapshot{}
	}
}

func waitForClosed(t *testing.T, c *closeFlagConn, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.closed.Load() == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("closed=%v, want %v after %v", c.closed.Load(), want, timeout)
}

// A veto that clears before the 3s re-verify elapses must not kill the link.
func TestCheaperLinkDebounceRidesTransientVeto(t *testing.T) {
	f := newVetoFixture(true)
	if testing.Short() {
		t.Skip("debounce timing test needs real time")
	}
	r := &runtime{cfg: Config{Selection: f.sel, Logger: zap.NewNop()}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &closeFlagConn{}
	stop := r.watchCheaperLink(ctx, 358, conn)
	defer stop()
	// Sanity: the fixture really vetoes before we lift it.
	if r.hasCheaperLink(358) != true {
		t.Fatal("fixture does not veto; test setup wrong")
	}
	time.Sleep(1500 * time.Millisecond)
	f.setVeto(false)
	// The ticker fires ~1s in while vetoed, starting a 3s re-verify that
	// elapses ~4s after start. Only an assertion past that deadline proves
	// the transient veto did not kill the link.
	time.Sleep(5 * time.Second)
	if conn.closed.Load() {
		t.Fatal("transient veto killed an established link")
	}
}

// A veto that persists through re-verify must still shed the link (~1s
// ticker + 3s re-verify; allow generous headroom for loaded CI).
func TestCheaperLinkDebounceStillShedsPersistentVeto(t *testing.T) {
	f := newVetoFixture(true)
	if testing.Short() {
		t.Skip("debounce timing test needs real time")
	}
	r := &runtime{cfg: Config{Selection: f.sel, Logger: zap.NewNop()}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := &closeFlagConn{}
	stop := r.watchCheaperLink(ctx, 358, conn)
	defer stop()
	waitForClosed(t, conn, true, 10*time.Second)
}

func TestAdmitDialPacesDeviceWide(t *testing.T) {
	r := &runtime{cfg: Config{Logger: zap.NewNop()}}
	now := time.Now()
	if !r.admitDial(now) {
		t.Fatal("first dial denied")
	}
	if r.admitDial(now.Add(4 * time.Second)) {
		t.Fatal("second dial admitted inside 5s window")
	}
	if !r.admitDial(now.Add(5 * time.Second)) {
		t.Fatal("dial denied after window elapsed")
	}
	// A second runtime (another device) is unaffected.
	other := &runtime{cfg: Config{Logger: zap.NewNop()}}
	if !other.admitDial(now) {
		t.Fatal("independent device paced by another")
	}
}

type countSnapshotNode struct{ links []localmesh.PeerLink }

func (n *countSnapshotNode) Snapshot() localmesh.NodeSnapshot {
	return localmesh.NodeSnapshot{Links: n.links}
}

func (n *countSnapshotNode) AttachStream(context.Context, int32, net.Conn, uint16) error {
	return nil
}

func TestBlePeerCountExcludesPendingClaims(t *testing.T) {
	r := &runtime{cfg: Config{Logger: zap.NewNop()}, active: map[int32]struct{}{10: {}, 11: {}}}
	if got := r.blePeerCount(); got != 0 {
		t.Fatalf("pending claims counted as established links: %d", got)
	}
	r.cfg.Node = &countSnapshotNode{links: []localmesh.PeerLink{{Asset: 11, Cost: LinkCost}, {Asset: 12, Cost: LinkCost}, {Asset: 13, Cost: 512}}}
	if got := r.blePeerCount(); got != 2 {
		t.Fatalf("established BLE links only: %d", got)
	}
}

func TestBlePeerCountKeepsScanningForPendingThirdPeer(t *testing.T) {
	node := &countSnapshotNode{links: []localmesh.PeerLink{
		{Asset: 358, Cost: LinkCost}, {Asset: 536, Cost: LinkCost},
		{Asset: 577, Cost: 512}, // A different carrier cannot fill the BLE target.
	}}
	r := &runtime{cfg: Config{Node: node, TargetPeers: 3, Logger: zap.NewNop()},
		active: map[int32]struct{}{358: {}, 536: {}, 577: {}}}
	if r.blePeerCount() >= r.cfg.TargetPeers {
		t.Fatal("governor would pause discovery with the third BLE dial still pending")
	}
	// Capacity admission remains bounded while discovery continues.
	if r.claim(460) {
		t.Fatal("excluding claims from the scan count allowed an extra dial")
	}
	node.links = append(node.links, localmesh.PeerLink{Asset: 577, Cost: LinkCost},
		localmesh.PeerLink{Asset: 536, Cost: LinkCost})
	if got := r.blePeerCount(); got != r.cfg.TargetPeers {
		t.Fatalf("full BLE complement, counting each asset once: %d", got)
	}
}
