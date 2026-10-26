package localmesh

import (
	"crypto/ecdsa"
	"errors"
	"testing"
	"time"
)

func shortLivedWarmFixture(t *testing.T, sameKey bool, renewedLifetime time.Duration) warmRenewalFixture {
	t.Helper()
	now := time.Now().Truncate(time.Second)
	old := newManifestTestAuthority(t, now).issue(t, now, nil, now.Add(30*time.Second))
	var key *ecdsa.PrivateKey
	if sameKey {
		key = old.Signer.(*ecdsa.PrivateKey)
	}
	renewed := newManifestTestAuthority(t, now).issue(t, now, key, now.Add(renewedLifetime))
	verify := func(chain [][]byte, at time.Time) (Identity, error) {
		if id, err := renewed.Verify(chain, at); err == nil {
			return id, nil
		}
		return old.Verify(chain, at)
	}
	return warmRenewalFixture{now: now, old: old, renewed: renewed, verify: verify}
}

func TestWarmRenewalRecoversWhenManifestOutlivesPreviousLeaf(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "different-key", true: "same-key"}[sameKey], func(t *testing.T) {
			f := shortLivedWarmFixture(t, sameKey, time.Hour)
			receiver, directory, _ := f.receiver(t)
			sender := f.source(t, f.renewed)
			requests, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now)
			if err != nil {
				t.Fatal(err)
			}
			wire := f.wire(t, f.now)
			if _, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &wire}, f.now); err != nil || len(changed) != 0 {
				t.Fatalf("active renewal with a still-valid short-lived old leaf closed the link: %v", err)
			}
			if len(directory.Snapshot(f.now)) != 0 {
				t.Fatal("pending renewal gained authority")
			}
			assertWarmRouteAuthorities(t, directory, f.now)
			changed, err := driveWarmCertificateFetch(sender, receiver, requests, f.now)
			if err != nil || len(changed) != 1 || len(directory.Snapshot(f.now)) != 1 {
				t.Fatalf("new valid chain did not recover renewed presence: changed=%d err=%v", len(changed), err)
			}
			assertWarmRouteAuthorities(t, directory, f.now, wire)
		})
	}
}

func TestWarmRenewalRejectsManifestOutlivingVerifiedReplacement(t *testing.T) {
	for _, sameKey := range []bool{false, true} {
		t.Run(map[bool]string{false: "different-key", true: "same-key"}[sameKey], func(t *testing.T) {
			f := shortLivedWarmFixture(t, sameKey, time.Minute)
			receiver, directory, _ := f.receiver(t)
			sender := f.source(t, f.renewed)
			requests, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now)
			if err != nil {
				t.Fatal(err)
			}
			wire := f.wire(t, f.now)
			if _, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &wire}, f.now); err != nil || len(changed) != 0 {
				t.Fatal("active renewal was not isolated", err)
			}
			if _, err = driveWarmCertificateFetch(sender, receiver, requests, f.now); !errors.Is(err, ErrManifestOutlivesCertificate) {
				t.Fatal("verified replacement lifetime did not remain fatal", err)
			}
			if len(directory.Snapshot(f.now)) != 0 || len(receiver.pending) != 0 || len(receiver.pendingRenewals) != 0 {
				t.Fatal("overlong verified replacement gained authority or remained pending")
			}
			assertWarmRouteAuthorities(t, directory, f.now)
		})
	}
}

func TestWarmLifetimeMismatchRequiresExactActiveDifferentLeaf(t *testing.T) {
	for _, scenario := range []string{"absent-descriptor", "same-leaf", "other-principal"} {
		t.Run(scenario, func(t *testing.T) {
			f := shortLivedWarmFixture(t, true, time.Hour)
			receiver, directory, _ := f.receiver(t)
			descriptor := warmDescriptor(f.renewed)
			if scenario == "same-leaf" {
				descriptor = warmDescriptor(f.old)
			}
			if scenario == "other-principal" {
				descriptor.Certificate.Chain.Principal = testPrincipal(testTenantUUID, testDeviceBID)
			}
			if scenario != "absent-descriptor" {
				if _, _, err := receiver.Receive(descriptor, f.now); err != nil {
					t.Fatal(err)
				}
			}
			wire := f.wire(t, f.now)
			if _, changed, err := receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &wire}, f.now); !errors.Is(err, ErrManifestOutlivesCertificate) || len(changed) != 0 {
				t.Fatal("ordinary lifetime mismatch was deferred", err)
			}
			if len(directory.Snapshot(f.now)) != 0 || len(receiver.pendingRenewals) != 0 {
				t.Fatal("out-of-scope lifetime mismatch gained pending authority")
			}
			assertWarmRouteAuthorities(t, directory, f.now)
		})
	}
}

func TestWarmRenewalRetainsOriginalMonotonicFetchBoundWithEarlierSignedExpiry(t *testing.T) {
	f := newWarmRenewalFixture(t)
	// Keep the real monotonic clock component; Truncate/UnixMilli deliberately
	// do not carry it, as is true of the manifest's signed absolute expiry.
	f.now = time.Now()
	receiver, directory, _ := f.receiver(t)
	if _, _, err := receiver.Receive(warmDescriptor(f.renewed), f.now); err != nil {
		t.Fatal(err)
	}
	_, original, ok := receiver.certificates.PendingChain(f.old.Device.Principal(), f.now)
	if !ok || original == original.Round(0) {
		t.Fatal("fixture did not retain a monotonic fetch allowance")
	}
	m := routeManifest(f.renewed.Device, f.now)
	m.Revision = 2
	m.Expires = f.now.Add(30 * time.Second).UnixMilli()
	w, err := SignManifest(m, f.renewed.Certificate.Certificate, f.renewed.Signer, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, f.now); err != nil {
		t.Fatal(err)
	}
	first := receiver.pendingRenewals[w.Principal]
	if first.fetchUntil != original || !first.expires.Before(first.fetchUntil) {
		t.Fatal("earlier signed expiry discarded the original monotonic fetch bound")
	}
	at := f.now.Add(10 * time.Second)
	if _, _, err = receiver.Receive(warmDescriptor(f.renewed), at); err != nil {
		t.Fatal(err)
	}
	if _, _, err = receiver.Receive(ControlMessage{Kind: "manifest", Manifest: &w}, at); err != nil {
		t.Fatal(err)
	}
	_, refreshed, _ := receiver.certificates.PendingChain(w.Principal, at)
	if !refreshed.After(original) {
		t.Fatal("fixture did not refresh the exchange hint")
	}
	if pending := receiver.pendingRenewals[w.Principal]; pending.fetchUntil != original || !pending.expires.Equal(first.expires) {
		t.Fatal("repeated hints renewed a retained payload's original bounds")
	}
	assertWarmRouteAuthorities(t, directory, at)
}
