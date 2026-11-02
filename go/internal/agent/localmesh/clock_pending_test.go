package localmesh

import (
	"errors"
	"testing"
	"time"
)

func TestClockPendingReentersNormalAdmissionWithoutExtendingDeadline(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	behind := now.Add(-598 * time.Second)
	resetClockFixtureCache(t, d, chain, behind)
	w, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSynchronizer(d, d.cache)
	defer s.Close()
	for range 2 {
		if _, _, err = s.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, behind); !errors.Is(err, ErrManifestClockSkew) {
			t.Fatal(err)
		}
	}
	deadline := s.pendingClock[w.ownerKey()]
	if deadline.IsZero() || len(s.pending) != 1 || len(d.history) != 0 || len(d.Snapshot(behind)) != 0 {
		t.Fatal("pending clock claim gained authority or leaked bodies")
	}
	if _, changed, err := s.ContinueClockPending(behind, 1); err != nil || len(changed) != 0 || s.pendingClock[w.ownerKey()] != deadline {
		t.Fatal("uncorrected retry changed deadline or admission", err)
	}
	if _, changed, err := s.ContinueClockPending(now, 1); err != nil || len(changed) != 1 || len(s.pendingClock) != 0 || len(s.pending) != 0 {
		t.Fatal("corrected claim skipped normal admission", err)
	}
	if len(d.history) != 1 || len(d.Snapshot(now)) != 1 {
		t.Fatal("ordinary admission did not install its replay receipt")
	}
}

func TestClockPendingBoundsAndExpiredClaimsDoNotTrigger(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	behind := now.Add(-598 * time.Second)
	resetClockFixtureCache(t, d, chain, behind)
	s := NewSynchronizer(d, d.cache)
	defer s.Close()
	w, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	// Occupy the bounded retained-body slots, then try another valid owner.
	s.pendingClock = map[string]time.Time{"occupied-a": time.Now().Add(time.Minute), "occupied-b": time.Now().Add(time.Minute)}
	s.pending["occupied-a"], s.pending["occupied-b"] = cloneSignedManifest(w), cloneSignedManifest(w)
	s.rememberClockPending(w)
	if len(s.pendingClock) != maxClockPendingPerPeer || len(s.pending) != maxClockPendingPerPeer {
		t.Fatal("clock pending body cap exceeded")
	}
	for k := range s.pendingClock {
		s.pendingClock[k] = time.Now().Add(-time.Second)
	}
	if _, _, err = s.ContinueClockPending(behind, 2); err != nil || len(s.pendingClock) != 0 || len(s.pending) != 0 {
		t.Fatal("monotonic deadline did not release bodies", err)
	}
	m := routeManifest(device, now.Add(-2*MaxLease))
	w, err = SignManifest(m, chain, key, time.UnixMilli(m.Issued))
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := s.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, now); err != nil || len(changed) != 0 || len(s.pendingClock) != 0 {
		t.Fatal("expired claim triggered bootstrap", err)
	}
}

func TestFutureReplayDoesNotTriggerClockRecovery(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	w, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	behind := now.Add(-598 * time.Second)
	resetClockFixtureCache(t, d, chain, behind)
	if changed, err := d.Accept(w, behind); changed || !errors.Is(err, ErrStaleManifest) {
		t.Fatalf("known replay triggered bootstrap: changed=%v err=%v", changed, err)
	}
}
