package localmesh

import (
	"testing"
	"time"
)

func TestRouteHandoffReplacementObservedPathNeverExtendsAcrossRenewal(t *testing.T) {
	for _, projectBeforeNext := range []bool{false, true} {
		for _, duplicate := range []bool{false, true} {
			name := "next-revision-before-snapshot"
			if projectBeforeNext {
				name = "snapshot-before-next-revision"
			}
			if duplicate {
				name += "/duplicate-observation"
			} else {
				name += "/replacement-observation"
			}
			t.Run(name, func(t *testing.T) {
				d, chain, signer, device, now := directoryFixture(t)
				base, selectedEnd := now, now.Add(time.Hour)
				d.certificateNow = func() time.Time { return base }
				d.cache.verify = func(_ [][]byte, _ time.Time) (Identity, error) {
					return Identity{Device: device, NotAfter: selectedEnd}, nil
				}
				old := routeManifest(device, now)
				// Signed dates fit both paths, but this receiver's 60s lease would
				// outlive the shorter replacement path's 20s physical remainder.
				old.Issued, old.Expires = now.Add(-50*time.Second).UnixMilli(), now.Add(10*time.Second).UnixMilli()
				wire := signRouteTestManifest(t, old, chain, signer, now)
				if _, err := d.Accept(wire, now); err != nil {
					t.Fatal(err)
				}
				next := cloneManifest(old)
				next.Revision++
				next.Route.Seqno++
				base = now.Add(time.Second)
				if !duplicate {
					selectedEnd = now.Add(20 * time.Second)
				}
				nextWire := signRouteTestManifest(t, next, chain, signer, now.Add(time.Second))
				if _, err := d.Accept(nextWire, now.Add(time.Second)); err != nil {
					t.Fatal(err)
				}
				owner := wire.ownerKey()
				if duplicate {
					mark, active := d.history[owner], d.entries[owner].activeUntil
					selectedEnd = now.Add(20 * time.Second)
					if changed, err := d.Accept(nextWire, now.Add(time.Second)); err != nil || changed {
						t.Fatal("duplicate changed announcement", changed, err)
					}
					if d.history[owner] != mark || !d.entries[owner].activeUntil.Equal(active) {
						t.Fatal("duplicate renewed lease/watermark")
					}
				}
				shortCap, shortEnd := d.entries[owner].certificateDeadline, d.entries[owner].certificateNotAfter
				if !shortCap.Equal(now.Add(20*time.Second)) || !shortEnd.Equal(shortCap) {
					t.Fatal("replacement did not record short path")
				}
				base, selectedEnd = now.Add(2*time.Second), now.Add(time.Hour)
				if projectBeforeNext {
					_, proofs := d.SnapshotWithRoutes(now.Add(2 * time.Second))
					if !proofs[*old.Route].Retained || proofs[*old.Route].Deadline.After(shortCap) {
						t.Fatal("first snapshot forgot replacement cap")
					}
				}
				next.Revision++
				next.Route.Seqno++
				if _, err := d.Accept(signRouteTestManifest(t, next, chain, signer, now.Add(2*time.Second)), now.Add(2*time.Second)); err != nil {
					t.Fatal(err)
				}
				_, proofs := d.SnapshotWithRoutes(now.Add(2 * time.Second))
				if !proofs[*old.Route].Retained || proofs[*old.Route].Deadline.After(shortCap) || !d.handoffs[owner].certificateNotAfter.Equal(shortEnd) {
					t.Fatal("later revision renewed earlier observed path")
				}
				mark := d.history[owner]
				// Certificate UTC jumps forward while receive-local elapsed moves1s.
				base = now.Add(15 * time.Second)
				_, proofs = d.SnapshotWithRoutes(now.Add(3 * time.Second))
				shortened := now.Add(8 * time.Second)
				if !proofs[*old.Route].Retained || !proofs[*old.Route].Deadline.Equal(shortened) {
					t.Fatalf("original observed NA forgotten: %+v", proofs[*old.Route])
				}
				base = now.Add(-time.Minute)
				_, proofs = d.SnapshotWithRoutes(now.Add(4 * time.Second))
				if proofs[*old.Route].Deadline.After(shortened) {
					t.Fatal("backward correction extended cap")
				}
				if got := d.RouteAuthorizations(shortened); got[*old.Route] || !got[*next.Route] {
					t.Fatal("old expiry borrowed new lease or revoked current tuple")
				}
				if d.history[owner] != mark {
					t.Fatal("verification renewed replay watermark")
				}
				records := d.Records(shortened)
				if len(records) != 1 {
					t.Fatal("old payload exported or latest removed")
				}
				current, err := DecodeManifest(records[0].Body)
				if err != nil || current.Revision != next.Revision {
					t.Fatal("old payload exported or latest removed")
				}
			})
		}
	}
}
