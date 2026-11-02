package localmesh

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestFutureManifestReportsLocalClockSkewWithoutAdmitting(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	w, err := SignManifest(routeManifest(device, now), chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	behind := now.Add(-598 * time.Second)
	// This is a cold boot with a stale clock, not a rollback after the cache
	// was populated. Preserve the cache's independent rollback protection.
	resetClockFixtureCache(t, d, chain, behind)
	changed, err := d.Accept(w, behind)
	if changed || err == nil {
		t.Fatalf("future lease admitted before time recovery: changed=%v err=%v", changed, err)
	}
	if !strings.Contains(err.Error(), "clock") {
		t.Fatalf("clock skew was hidden as a malformed manifest: %v", err)
	}
	if len(d.Snapshot(behind)) != 0 || len(d.history) != 0 {
		t.Fatal("future lease installed presence or replay state")
	}
	if changed, err = d.Accept(w, now); err != nil || !changed {
		t.Fatalf("same signed lease failed after clock recovery: changed=%v err=%v", changed, err)
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

func TestManifestFutureClockToleranceUnchanged(t *testing.T) {
	_, _, _, device, now := directoryFixture(t)
	for _, tc := range []struct {
		name      string
		ahead     time.Duration
		wantError bool
	}{
		{"within tolerance", 5 * time.Second, false},
		{"past tolerance", 5*time.Second + time.Millisecond, true},
		{"reported outage", 598 * time.Second, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := routeManifest(device, now.Add(tc.ahead))
			if err := m.validate(now); (err != nil) != tc.wantError {
				t.Fatalf("ahead=%s err=%v wantError=%v", tc.ahead, err, tc.wantError)
			}
		})
	}
}
