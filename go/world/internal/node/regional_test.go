package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"testing"
	"time"
)

func regionalNodePolicy() *subscription.Policy {
	p := nodePolicy("wendy.hazard.v1")
	p.UnknownLocation = "retain"
	p.Selection.Region = &selection.Region{Frame: world.FrameRef{ID: "floor", Revision: "1"}, MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{5, 5}}
	return p
}
func regionalNodeValue(a *fixture, seq uint64, x int) world.Component {
	c := component(a, 1, seq)
	c.Key.Type = "wendy.hazard.v1"
	c.Payload, _ = json.Marshal(map[string]any{"position_m": []int{x, 0, 1}, "uncertainty_m": 0, "outside_secret": x == 20})
	return c
}
func TestRegionalThreeNodesRepairMovementPartitionRestartAndAuthority(t *testing.T) {
	a := subscribedFixture(t, nil, "full", 32, nil)
	b := subscribedFixture(t, a.root, "west", 32, regionalNodePolicy())
	c := subscribedFixture(t, a.root, "other-west", 32, regionalNodePolicy())
	syncTo := func(f *fixture) {
		t.Helper()
		if e := f.n.Sync(context.Background(), Peer{a.server.URL, a.audience}, f.credentials); e != nil {
			t.Fatal(e)
		}
	}
	inside := regionalNodeValue(a, 1, 1)
	original := signed(t, a, inside)
	if _, e := a.n.apply(original, false); e != nil {
		t.Fatal(e)
	}
	syncTo(b)
	syncTo(c)
	if len(query(t, b).Components) != 1 || len(query(t, c).Components) != 1 {
		t.Fatal("initial inside missing")
	}
	outside := regionalNodeValue(a, 2, 20)
	outsideProof := signed(t, a, outside)
	expiredGrant := a.credentials.Grant.Grant
	expiredGrant.ID = "expired-origin"
	expiredGrant.NotBeforeUnixMS = time.Now().Add(-time.Hour).UnixMilli()
	expiredGrant.NotAfterUnixMS = time.Now().Add(-time.Minute).UnixMilli()
	expiredSignedGrant, e := authority.SignGrant(expiredGrant, a.root)
	if e != nil {
		t.Fatal(e)
	}
	expiredRecord, e := authority.SignRecord(expiredSignedGrant, a.credentials.PrivateKey, outside, time.Now().Add(-2*time.Minute))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.n.apply(expiredRecord, false); e == nil {
		t.Fatal("expired grant advanced fence")
	}
	if len(query(t, b).Components) != 1 {
		t.Fatal("expired grant removed inside")
	}
	bad := outsideProof
	bad.Signature = append([]byte(nil), bad.Signature...)
	bad.Signature[0] ^= 1
	if _, e := b.n.apply(bad, false); e == nil {
		t.Fatal("forged fence accepted")
	}
	if len(query(t, b).Components) != 1 {
		t.Fatal("forgery removed inside")
	}
	a.unavailable.Store(true)
	if e := b.n.Sync(context.Background(), Peer{a.server.URL, a.audience}, b.credentials); e == nil {
		t.Fatal("partition hidden")
	}
	if _, e := a.n.apply(outsideProof, false); e != nil {
		t.Fatal(e)
	}
	if len(query(t, b).Components) != 1 {
		t.Fatal("partition knowledge invented")
	}
	a.unavailable.Store(false)
	syncTo(b)
	syncTo(c)
	for _, f := range []*fixture{b, c} {
		if len(query(t, f).Components) != 0 {
			t.Fatal("outside still visible")
		}
		raw, _ := os.ReadFile(f.cfg.ProofPath)
		if bytes.Contains(raw, []byte("outside_secret")) {
			t.Fatal("outside or old payload retained in proof file")
		}
		subscribedRestart(t, f, regionalNodePolicy())
		if result, e := f.n.apply(original, false); e != nil || result != world.Obsolete {
			t.Fatal("restart resurrected", result, e)
		}
	}
	conflict := outside
	conflict.Payload = inside.Payload
	if _, e := b.n.apply(signed(t, a, conflict), false); !errors.Is(e, world.ErrConflict) {
		t.Fatal("same-version conflict hidden", e)
	}
	if e := b.n.Sync(context.Background(), Peer{c.server.URL, c.audience}, b.credentials); e == nil {
		t.Fatal("partial source accepted as regional full source")
	}
	reenter := regionalNodeValue(a, 3, 1)
	if _, e := a.n.apply(signed(t, a, reenter), false); e != nil {
		t.Fatal(e)
	}
	syncTo(b)
	if len(query(t, b).Components) != 1 {
		t.Fatal("reentry missing")
	}
	// A current denied source cannot move the inside value out of the region.
	r, e := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: time.Now().Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: time.Now().Add(30 * time.Minute).UnixMilli(), GrantIDs: []string{a.credentials.Grant.Grant.ID}}, a.root)
	if e != nil {
		t.Fatal(e)
	}
	if e = b.v.UpdateRevocations(r); e != nil {
		t.Fatal(e)
	}
	newOutside := regionalNodeValue(a, 4, 20)
	if _, e = b.n.apply(signed(t, a, newOutside), false); e == nil {
		t.Fatal("revoked source fenced")
	}
	cp, _ := b.s.RepairSnapshot()
	if len(cp.Entries) != 1 || len(cp.Fences) != 0 {
		t.Fatal("revoked fence changed durable state")
	}
}

// A crash may leave a harmless old-proof superset. Reopening a fenced world
// must discard it, while reopening the precommit world must retain coverage.
func TestRegionalRecoveryPrunesOnlyAfterWorldCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			a := subscribedFixture(t, nil, "origin", 8, nil)
			b := subscribedFixture(t, a.root, "reader", 8, regionalNodePolicy())
			inside := regionalNodeValue(a, 1, 1)
			if _, e := b.n.apply(signed(t, a, inside), false); e != nil {
				t.Fatal(e)
			}
			old, e := os.ReadFile(b.cfg.ProofPath)
			if e != nil {
				t.Fatal(e)
			}
			if committed {
				if _, e = b.n.apply(signed(t, a, regionalNodeValue(a, 2, 20)), false); e != nil {
					t.Fatal(e)
				}
			}
			// Restore exactly the proof bytes present before world commit, modelling
			// process loss before the postcommit prune can run.
			if e = os.WriteFile(b.cfg.ProofPath, old, 0600); e != nil {
				t.Fatal(e)
			}
			subscribedRestart(t, b, regionalNodePolicy())
			raw, _ := os.ReadFile(b.cfg.ProofPath)
			rows := query(t, b).Components
			if committed {
				if len(rows) != 0 || bytes.Contains(raw, []byte("outside_secret")) {
					t.Fatal("postcommit recovery retained payload")
				}
			} else {
				if len(rows) != 1 || !bytes.Contains(raw, []byte("outside_secret")) {
					t.Fatal("precommit recovery lost required proof")
				}
			}
		})
	}
}

func TestRegionExpiredLocalProofFallback(t *testing.T) {
	a := subscribedFixture(t, nil, "origin", 8, nil)
	b := subscribedFixture(t, a.root, "reader", 8, regionalNodePolicy())
	c := regionalNodeValue(a, 1, 1)
	g := a.credentials.Grant.Grant
	g.ID = "short-lived-origin"
	g.NotAfterUnixMS = time.Now().Add(150 * time.Millisecond).UnixMilli()
	short, e := authority.SignGrant(g, a.root)
	if e != nil {
		t.Fatal(e)
	}
	prior, e := authority.SignRecord(short, a.credentials.PrivateKey, c, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	incoming, e := authority.SignRecord(a.credentials.Grant, a.credentials.PrivateKey, c, time.Now().Add(-time.Second))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.n.apply(prior, false); e != nil {
		t.Fatal(e)
	}
	time.Sleep(time.Until(time.UnixMilli(g.NotAfterUnixMS)) + 10*time.Millisecond)
	if _, e = b.n.applyRepairPage([]RepairRecord{reviewRepairRecord(t, incoming, c.Timing.SampleTime.Add(c.TTL))}); e != nil {
		t.Fatal("valid incoming proof rejected because local grant expired", e)
	}
	d, _ := recordDigest(c)
	if b.n.proofs[d].Grant.Grant.ID != incoming.Grant.Grant.ID {
		t.Fatal("expired local proof retained")
	}
}
