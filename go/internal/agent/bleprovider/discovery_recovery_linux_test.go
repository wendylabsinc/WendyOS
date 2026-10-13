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
	"go.uber.org/zap"
)

type recoveryTestNode struct{ links []localmesh.PeerLink }

func (*recoveryTestNode) AttachStream(context.Context, int32, net.Conn, uint16) error { return nil }
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
		t.Fatal("did not restart after mesh advertisement silence")
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

func TestDiscoveryRecoveryCountsDistinctActiveAndAuthenticatedPeers(t *testing.T) {
	node := &recoveryTestNode{links: []localmesh.PeerLink{{Asset: 10}, {Asset: 20}}}
	r := &runtime{cfg: Config{Node: node, TargetPeers: 3}, active: map[int32]struct{}{10: {}}}
	if !r.missingPeerSlots() {
		t.Fatal("two distinct peers did not leave a slot")
	}
	r.active[30] = struct{}{}
	if r.missingPeerSlots() {
		t.Fatal("an in-flight third peer triggered discovery restart")
	}
	delete(r.active, 30)
	node.links = append(node.links, localmesh.PeerLink{Asset: 30})
	if r.missingPeerSlots() {
		t.Fatal("three authenticated peers triggered discovery restart")
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
		cfg:    Config{Node: &recoveryTestNode{}, TargetPeers: 3, Logger: zap.NewNop()},
		active: map[int32]struct{}{},
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
