package localmesh

import (
	"errors"
	"testing"
	"time"
)

func TestFutureManifestUsesReceiverRelativeLease(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	w, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	behind := now.Add(-598 * time.Second)
	resetClockFixtureCache(t, d, chain, behind)
	if changed, err := d.Accept(w, behind); err != nil || !changed {
		t.Fatal(changed, err)
	}
	got := d.Snapshot(behind)
	if len(got) != 1 || got[0].LeaseRemaining(behind) != time.Minute {
		t.Fatal("future wall date changed signed duration", got)
	}
}

func TestFutureManifestMustAuthenticateBeforeClockRecovery(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	w, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	w.Signature[0] ^= 1
	resetClockFixtureCache(t, d, chain, now.Add(-598*time.Second))
	if changed, err := d.Accept(w, now.Add(-598*time.Second)); changed || !errors.Is(err, ErrInvalidManifestSignature) {
		t.Fatalf("invalid signature was mistaken for recoverable clock skew: changed=%v err=%v", changed, err)
	}
	if len(d.entries) != 0 || len(d.history) != 0 {
		t.Fatal("invalid future lease installed presence or replay state")
	}
}

func resetClockFixtureCache(t *testing.T, d *Directory, chain [][]byte, now time.Time) {
	t.Helper()
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), d.cache.verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(chain, now); err != nil {
		t.Fatal(err)
	}
	d.cache = cache
}

func TestManifestStructuralValidationDoesNotUseReceiverWallTime(t *testing.T) {
	_, _, _, device, now := directoryFixture(t)
	for _, tc := range []struct {
		name      string
		ahead     time.Duration
		wantError bool
	}{
		{"within tolerance", 5 * time.Second, false},
		{"past tolerance", 5*time.Second + time.Millisecond, false},
		{"reported outage", 598 * time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := routeManifest(device, now.Add(tc.ahead))
			if err := m.validate(now); (err != nil) != tc.wantError {
				t.Fatalf("ahead=%s err=%v wantError=%v", tc.ahead, err, tc.wantError)
			}
		})
	}
}
