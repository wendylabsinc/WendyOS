//go:build linux

package bleprovider

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

type recoveryTestNode struct{ links []localmesh.PeerLink }

func (*recoveryTestNode) AttachStream(context.Context, meshid.Device, net.Conn, uint16) error {
	return nil
}
func (n *recoveryTestNode) Snapshot() localmesh.NodeSnapshot {
	return localmesh.NodeSnapshot{Links: n.links}
}

func TestDiscoveryRecoveryRequiresSilenceAndMissingPeers(t *testing.T) {
	base := time.Unix(1_000, 0)
	var recovery discoveryRecovery
	if recovery.restartDue(base.Add(time.Hour), true) {
		t.Fatal("restarted before any live Wendy advertisement")
	}
	recovery.observed(base)
	if recovery.restartDue(base.Add(discoverySilenceLimit-time.Nanosecond), true) {
		t.Fatal("restarted during normal advertisement gap")
	}
	if recovery.restartDue(base.Add(discoverySilenceLimit), false) {
		t.Fatal("restarted with all peer slots filled")
	}
	first := base.Add(discoverySilenceLimit)
	if !recovery.restartDue(first, true) {
		t.Fatal("did not restart after wendy net advertisement silence")
	}
	if recovery.restartDue(first.Add(discoveryRestartPeriod-time.Nanosecond), true) {
		t.Fatal("restarted again before cooldown")
	}
	if !recovery.restartDue(first.Add(discoveryRestartPeriod), true) {
		t.Fatal("did not retry after bounded cooldown")
	}
	recovery.observed(first.Add(discoveryRestartPeriod + time.Second))
	if recovery.restartDue(first.Add(discoveryRestartPeriod+time.Second+discoverySilenceLimit-time.Nanosecond), true) {
		t.Fatal("fresh advertisement did not suppress restart")
	}
}

func TestDiscoveryRecoveryCountsOnlyThisTransportReservations(t *testing.T) {
	r := &runtime{cfg: Config{Credentials: &localmesh.Credentials{Device: selfFixture}}}
	p := r.connectionPolicy()
	a := p.Reserve("first", numberedBLEDevice(10), true)
	if a == nil || !a.Ready(numberedBLEDevice(10), func() {}) {
		t.Fatal("fixture failed")
	}
	if !r.missingPeerSlots() {
		t.Fatal("one outgoing must leave a slot")
	}
	b := p.Reserve("second", numberedBLEDevice(20), true)
	if b == nil || r.missingPeerSlots() {
		t.Fatal("pending second outgoing not counted")
	}
	b.Release()
	if !r.missingPeerSlots() {
		t.Fatal("failed attempt did not release slot")
	}
}

func TestDiscoveryRestartReleasesOnlyCallerSessionAndRestoresFilter(t *testing.T) {
	var calls []string
	failedFilter := errors.New("filter unavailable")
	err := restartDiscoverySession(context.Background(), func(_ context.Context, method string, args ...any) error {
		calls = append(calls, method)
		if method == adapterInterface+".SetDiscoveryFilter" {
			if len(args) != 1 {
				t.Fatalf("filter arguments = %v", args)
			}
			filter, ok := args[0].(map[string]dbus.Variant)
			if !ok || filter["Transport"].Value() != "le" || filter["DuplicateData"].Value() != true {
				t.Fatalf("wrong discovery filter: %v", args)
			}
			return failedFilter
		}
		return nil
	})
	want := []string{adapterInterface + ".StopDiscovery", adapterInterface + ".SetDiscoveryFilter", adapterInterface + ".StartDiscovery"}
	if len(calls) != len(want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", calls, want)
		}
	}
	if !errors.Is(err, failedFilter) {
		t.Fatalf("restart did not return filter error: %v", err)
	}
}

func TestDiscoveryRestartIsBoundedAfterFailure(t *testing.T) {
	base := time.Unix(1_000, 0)
	var attempts int
	r := &runtime{
		cfg: Config{Node: &recoveryTestNode{}, Logger: zap.NewNop()},
		restartScan: func(context.Context, *dbus.Conn, dbus.ObjectPath) error {
			attempts++
			return errors.New("controller busy")
		},
	}
	r.discovery.observed(base)
	r.maybeRestartDiscovery(context.Background(), nil, "/org/bluez/hci0", base.Add(discoverySilenceLimit))
	r.maybeRestartDiscovery(context.Background(), nil, "/org/bluez/hci0", base.Add(discoverySilenceLimit+time.Second))
	if attempts != 1 {
		t.Fatalf("failed restart retried immediately: %d", attempts)
	}
	r.maybeRestartDiscovery(context.Background(), nil, "/org/bluez/hci0", base.Add(discoverySilenceLimit+discoveryRestartPeriod))
	if attempts != 2 {
		t.Fatalf("bounded retry did not occur: %d", attempts)
	}
}

func TestDiscoveryRestartFailureDegradesWithoutDroppingPeers(t *testing.T) {
	base := time.Unix(1_000, 0)
	peer := fixtureDevice("00000000-0000-4000-8000-000000000010")
	node := &recoveryTestNode{links: []localmesh.PeerLink{{Device: peer, Cost: LinkCost}}}
	failed := errors.New("controller busy")
	var updates []localmesh.CarrierStatus
	var attempts int
	r := &runtime{
		cfg: Config{Node: node, Logger: zap.NewNop(), Status: func(s localmesh.CarrierStatus) {
			updates = append(updates, s)
		}},

		restartScan: func(context.Context, *dbus.Conn, dbus.ObjectPath) error {
			attempts++
			if attempts == 1 {
				return failed
			}
			return nil
		},
	}
	held := r.connectionPolicy().Reserve("held", peer, false)
	defer held.Release()
	r.discovery.observed(base)
	first := base.Add(discoverySilenceLimit)
	r.maybeRestartDiscovery(context.Background(), nil, "/org/bluez/hci0", first)
	if len(updates) != 1 || !updates[0].Ready || !errors.Is(updates[0].Err, failed) {
		t.Fatalf("discovery failure did not degrade local readiness: %+v", updates)
	}
	if r.connectionPolicy().Counts().Total != 1 || r.blePeerCount() != 1 {
		t.Fatal("discovery failure removed an established peer")
	}
	// A normal governor pass does not clear the error, and the failed restart
	// retains its original cooldown rather than resetting the provider/ACLs.
	if !r.governDiscovery(context.Background(), nil, "/org/bluez/hci0") {
		t.Fatal("partial peer complement unexpectedly paused discovery")
	}
	r.maybeRestartDiscovery(context.Background(), nil, "/org/bluez/hci0", first.Add(time.Second))
	if attempts != 1 || len(updates) != 1 || !errors.Is(r.discoveryErr, failed) {
		t.Fatalf("failure disappeared or retried early: attempts=%d updates=%+v", attempts, updates)
	}
	// A deliberately paused provider neither attempts recovery nor falsely
	// clears the error. Unpausing permits the next bounded session restart.
	r.scanPaused = true
	r.maybeRestartDiscovery(context.Background(), nil, "/org/bluez/hci0", first.Add(discoveryRestartPeriod))
	if attempts != 1 || len(updates) != 1 {
		t.Fatal("paused discovery changed health or restarted")
	}
	r.scanPaused = false
	r.maybeRestartDiscovery(context.Background(), nil, "/org/bluez/hci0", first.Add(discoveryRestartPeriod))
	if attempts != 2 || len(updates) != 2 || !updates[1].Ready || updates[1].Err != nil || r.discoveryErr != nil {
		t.Fatalf("successful session restart did not restore readiness: %+v", updates)
	}
	if r.connectionPolicy().Counts().Total != 1 || r.blePeerCount() != 1 {
		t.Fatal("discovery recovery removed an established peer")
	}
}

func TestDiscoveryHealthIgnoresCancellationAndPeerAbsence(t *testing.T) {
	var updates []localmesh.CarrierStatus
	r := &runtime{cfg: Config{Node: &recoveryTestNode{}, Status: func(s localmesh.CarrierStatus) {
		updates = append(updates, s)
	}}}
	// No first live advertisement: do not turn a healthy isolated node into a
	// radio failure or attempt a restart based only on missing peers.
	r.maybeRestartDiscovery(context.Background(), nil, "/org/bluez/hci0", time.Unix(1_000, 0))
	if len(updates) != 0 {
		t.Fatalf("peer absence changed local health: %+v", updates)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.reportDiscoveryHealth(ctx, context.Canceled)
	if len(updates) != 0 || r.discoveryErr != nil {
		t.Fatal("provider cancellation reported a discovery failure")
	}
	failed := errors.New("scan unavailable")
	r.reportDiscoveryHealth(context.Background(), failed)
	r.reportDiscoveryHealth(ctx, nil)
	if len(updates) != 1 || !errors.Is(r.discoveryErr, failed) {
		t.Fatal("canceled recovery cleared the discovery failure")
	}
}
