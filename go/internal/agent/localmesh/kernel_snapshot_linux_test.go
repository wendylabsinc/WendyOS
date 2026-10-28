//go:build linux

package localmesh

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/vishvananda/netlink"
)

func TestKernelSnapshotDuringAddressChanges(t *testing.T) {
	// A dump can contain useful-looking partial state when an unrelated
	// interface changes. It must not reach the repair/ownership decisions.
	reads := 0
	live := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Index: 7}}
	state, err := readConsistentBase(func() (baseState, error) {
		reads++
		if reads < 3 {
			return baseState{address: true}, fmt.Errorf("inspect wendy net address: %w", netlink.ErrDumpInterrupted)
		}
		return baseState{link: live, up: true, rule: true, guard: true, operatorRule: true}, nil
	})
	if err != nil || reads != 3 {
		t.Fatalf("completed snapshot: reads=%d err=%v", reads, err)
	}
	if state.link != live || state.address || !state.up || !state.rule || !state.guard || !state.operatorRule {
		t.Fatalf("mixed partial and completed snapshots: %+v", state)
	}
}

func TestKernelSnapshotBoundsOngoingChanges(t *testing.T) {
	reads := 0
	state, err := readConsistentBase(func() (baseState, error) {
		reads++
		return baseState{address: true, guard: true}, netlink.ErrDumpInterrupted
	})
	if reads != kernelSnapshotAttempts || !errors.Is(err, netlink.ErrDumpInterrupted) {
		t.Fatalf("unbounded or lost interruption: reads=%d err=%v", reads, err)
	}
	if state != (baseState{}) {
		t.Fatalf("incomplete snapshot escaped: %+v", state)
	}
}

// The namespace is disposable: this never changes the lab host's networking.
func TestKernelSnapshotReconcileIsolatedNamespace(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("requires a disposable network namespace")
	}
	self, peer := testDevices(t)
	k, err := NewKernel(self)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := k.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := k.AddPeer(1, peer); err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if err := k.ReconcileBase(); err != nil {
			t.Fatal(err)
		}
		state, err := k.inspectBase()
		if err != nil {
			t.Fatal(err)
		}
		if state.link == nil || !state.up || !state.address || !state.rule || !state.guard {
			t.Fatalf("incomplete base after reconciliation: %+v", state)
		}
	}
}
