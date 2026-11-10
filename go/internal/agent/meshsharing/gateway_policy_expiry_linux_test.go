//go:build linux

package meshsharing

import (
	"testing"
	"time"
)

func TestActiveGatewayLeaseRechecksPolicyEvenWithoutWorkerIO(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	end := now.Add(time.Hour)
	clock := &controlPolicyClock{floor: end.Add(-30 * time.Second), uncertain: true}
	self, peer := policyControlPair(t, clock, now.Add(-time.Hour), now.Add(2*time.Hour), now.Add(-time.Hour), end)
	bounded, _ := policyControlConn(t, self, peer, nil)
	policy, borrowed, _ := borrowedLeaseFixture(t)
	borrowed.peer = peer.Device
	wrapped := &borrowedControlConn{expiryConn: bounded}
	borrowed.control = wrapped
	if !policy.ActiveGatewayLease(peer.Device) {
		t.Fatal("valid policy control was not live")
	}
	clock.set(end.Add(time.Second), 0, true)
	// No borrower worker performs I/O here. The expiry policy must invalidate
	// liveness even though the outer tracking wrapper has observed no Close.
	if policy.ActiveGatewayLease(peer.Device) {
		t.Fatal("expired policy retained active gateway lease")
	}
	if !borrowed.controlLive.Load() || wrapped.closed.Load() {
		t.Fatal("fixture accidentally relied on worker/outer Close invalidation")
	}
}
