package localmesh

import (
	"errors"
	"testing"
	"time"
)

func TestVerifiedSupersessionDropsCrossLinkPendingBody(t *testing.T) {
	for _, action := range []string{"receive", "record", "idle-sweep"} {
		for _, equal := range []bool{false, true} {
			t.Run(action+map[bool]string{true: "/same-revision", false: "/higher-revision"}[equal], func(t *testing.T) {
				d, chain, key, device, now := directoryFixture(t)
				s := NewSynchronizer(d, d.cache)
				defer s.Close()
				m := routeManifest(device, now)
				old, err := SignManifest(m, chain, key, now)
				if err != nil {
					t.Fatal(err)
				}
				d.cache.mu.Lock()
				delete(d.cache.entries, old.Principal)
				d.cache.mu.Unlock()
				d.cache.parts.mu.Lock()
				clear(d.cache.parts.objects)
				d.cache.parts.bytes = 0
				d.cache.parts.mu.Unlock()
				if _, _, err := s.Receive(ControlMessage{Kind: "manifest", Manifest: &old}, now); err != nil || len(s.pending) != 1 {
					t.Fatal("fixture did not retain identity repair", err)
				}
				if _, _, err := s.Receive(ControlMessage{Kind: "certificate", Certificate: DescribeCertificates(old.Principal, chain)}, now); err != nil || !s.certificates.Pending() {
					t.Fatal("fixture did not retain certificate continuation", err)
				}
				// Identity repair through another neighbour updates this shared
				// cache without consuming the old per-link pending payload.
				if _, err := d.cache.Put(chain, now); err != nil {
					t.Fatal(err)
				}
				if !equal {
					m.Revision++
					m.Withdraw = true
				}
				current, err := SignManifest(m, chain, key, now)
				if err != nil {
					t.Fatal(err)
				}
				if action == "receive" {
					if _, changed, err := s.Receive(ControlMessage{Kind: "manifest", Manifest: &current}, now); err != nil || len(changed) != 1 {
						t.Fatal("verified current record was not admitted", err)
					}
				} else {
					if _, err := d.Accept(current, now); err != nil {
						t.Fatal(err)
					}
					if action == "record" {
						s.Record(current, now)
					} else {
						s.Sweep(now)
					}
				}
				if len(s.pending) != 0 || len(s.pendingDeadlines) != 0 || len(s.pendingRenewals) != 0 || s.certificates.Pending() || s.certificates.waiting != nil {
					t.Fatal("verified supersession retained full body or repair continuation")
				}
			})
		}
	}
}

func TestVerifiedLowerRevisionPreservesHigherPendingClaim(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	s := NewSynchronizer(d, d.cache)
	defer s.Close()
	m := routeManifest(device, now)
	m.Revision = 2
	pending, _ := SignManifest(m, chain, key, now)
	d.cache.mu.Lock()
	delete(d.cache.entries, pending.Principal)
	d.cache.mu.Unlock()
	if _, _, err := s.Receive(ControlMessage{Kind: "manifest", Manifest: &pending}, now); err != nil {
		t.Fatal(err)
	}
	until := s.pendingDeadlines[pending.ownerKey()]
	if _, err := d.cache.Put(chain, now); err != nil {
		t.Fatal(err)
	}
	m.Revision = 1
	current, _ := SignManifest(m, chain, key, now)
	if _, err := d.Accept(current, now); err != nil {
		t.Fatal(err)
	}
	s.Record(current, now)
	s.Sweep(now)
	// Even an unverified envelope claiming an extremely high revision must
	// not erase the genuine pending candidate or current admitted state.
	m.Revision = 99999
	bad, _ := SignManifest(m, chain, key, now)
	bad.Signature[0] ^= 1
	if _, _, err := s.Receive(ControlMessage{Kind: "manifest", Manifest: &bad}, now); !errors.Is(err, ErrInvalidManifestSignature) {
		t.Fatal("invalid signature unexpectedly admitted", err)
	}
	if old, ok := s.pending[pending.ownerKey()]; !ok || !sameManifestWire(old, pending) || s.pendingDeadlines[pending.ownerKey()] != until || len(d.Records(now)) != 1 {
		t.Fatal("lower verified/unsigned high revision erased higher pending state")
	}
}

func TestCompactWatermarkDropsOlderPendingAfterCurrentPayloadExpires(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	s := NewSynchronizer(d, d.cache)
	defer s.Close()
	m := routeManifest(device, now)
	old, _ := SignManifest(m, chain, key, now)
	d.cache.mu.Lock()
	delete(d.cache.entries, old.Principal)
	d.cache.mu.Unlock()
	s.Receive(ControlMessage{Kind: "manifest", Manifest: &old}, now)
	d.cache.Put(chain, now)
	m.Revision = 2
	m.Expires = now.Add(time.Second).UnixMilli()
	current, _ := SignManifest(m, chain, key, now)
	if _, err := d.Accept(current, now); err != nil {
		t.Fatal(err)
	}
	s.Sweep(now.Add(2 * time.Second))
	if len(d.entries) != 0 || len(d.history) != 1 || len(s.pending) != 0 || len(s.pendingDeadlines) != 0 {
		t.Fatal("authenticated compact watermark left older payload repair buffered")
	}
}
