//go:build linux

package nanprovider

import (
	"testing"
	"time"
)

func TestBorrowedNANRecoveryCanReplacePrivateNDIOnly(t *testing.T) {
	r := nanRecovery{}
	if !r.canReset(true) || r.next(true) != resetNDI {
		t.Fatal("external management ownership prevents recovery of our private NDI")
	}
	if !r.attempted() || r.canReset(true) || r.next(true) != resetNone {
		t.Fatal("private NDI recovery must be bounded and never restart borrowed nan0")
	}
}

func TestOwnedNANRecoveryRequiresNoRXBeforeManagementReset(t *testing.T) {
	for _, noRX := range []bool{false, true} {
		r := nanRecovery{ownsSession: true}
		if !r.canReset(true) || r.next(false) != resetNDI || !r.canReset(true) {
			t.Fatal("owned management must try private NDI recovery first")
		}
		want := resetNone
		if noRX {
			want = resetOwnedSession
		}
		if got := r.next(noRX); got != want {
			t.Fatalf("next(noRX=%v) = %v, want %v", noRX, got, want)
		}
		if noRX && (r.canReset(true) || r.next(true) != resetNone) {
			t.Fatal("owned management recovery exceeded one reset")
		}
	}
}

func TestOwnedNANRecoveryPreservesWarmEpochAfterNDIRetry(t *testing.T) {
	r := nanRecovery{ownsSession: true}
	if r.next(false) != resetNDI {
		t.Fatal("first warm failure must recreate the private NDI")
	}
	if r.canReset(false) {
		t.Fatal("second warm failure would tear down the epoch without a permitted reset")
	}
	if !r.canReset(true) || r.next(true) != resetOwnedSession {
		t.Fatal("warm evidence must not consume the later no-RX management reset")
	}
	if r.canReset(false) || r.canReset(true) {
		t.Fatal("exhausted recovery must preserve the final live epoch")
	}
}

func TestNANRecoveryRearmsOnlyPrivateNDIAfterStableAuthentication(t *testing.T) {
	for _, owned := range []bool{false, true} {
		r := nanRecovery{ownsSession: owned}
		if r.next(false) != resetNDI {
			t.Fatal("first outage did not receive its private-interface attempt")
		}
		if owned && r.next(true) != resetOwnedSession {
			t.Fatal("owned management attempt was unavailable")
		}
		now := time.Unix(1000, 0)
		link := &radioLink{since: now}
		links := map[string]*radioLink{"neighbor": link}
		for _, delay := range []time.Duration{0, 10 * time.Minute} {
			if r.rearmAfterStableAuthentication(now.Add(delay), links) || r.canReset(false) {
				t.Fatal("an uninterrupted unauthenticated outage renewed recovery")
			}
		}
		now = now.Add(10 * time.Minute)
		link.health(true, now)
		if r.rearmAfterStableAuthentication(now, links) || r.rearmAfterStableAuthentication(now.Add(time.Minute-time.Second), links) {
			t.Fatal("a short authenticated connection renewed recovery")
		}
		// A failure breaks the same neighbor's stability window, even if its
		// next handshake follows immediately, between consecutive radio ticks.
		link.health(false, now.Add(time.Minute-time.Second))
		link.health(true, now.Add(time.Minute))
		if r.rearmAfterStableAuthentication(now.Add(time.Minute), links) || r.rearmAfterStableAuthentication(now.Add(2*time.Minute-time.Second), links) {
			t.Fatal("separate transient connections accumulated stability time")
		}
		if !r.rearmAfterStableAuthentication(now.Add(2*time.Minute), links) || !r.canReset(false) {
			t.Fatal("stable authenticated recovery left the next outage without a private-interface attempt")
		}
		if r.sessionReset != owned || r.ownsSession != owned {
			t.Fatal("rearming changed management ownership or renewed its lifetime budget")
		}
		if r.next(false) != resetNDI || r.canReset(true) || r.next(true) != resetNone {
			t.Fatal("the new outage exceeded its single private-interface attempt")
		}
	}
}
