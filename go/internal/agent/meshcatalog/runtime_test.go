package meshcatalog

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestBundleHintsArePeerScopedAndBounded(t *testing.T) {
	f := newFixture(t)
	c, _ := f.newCatalog(t, 533, "default", nil, nil)
	r, err := NewRuntime(c, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	if err != nil {
		t.Fatal(err)
	}
	bundle := f.creds[533].Certificate.Certificate
	fp := localmesh.Fingerprint(bundle)
	r.bundleWritten(534, bundle, f.now)
	if got := r.knownBundles(534, f.now.Add(time.Second)); len(got) != 1 || got[0] != fp {
		t.Fatalf("same-peer warm hint = %v", got)
	}
	if got := r.knownBundles(535, f.now.Add(time.Second)); len(got) != 0 {
		t.Fatalf("different peer inherited bundle hint = %v", got)
	}
	if got := r.knownBundles(534, f.now.Add(bundleHintLifetime)); len(got) != 0 {
		t.Fatalf("expired bundle hint survived = %v", got)
	}
	for i := 0; i < maxBundleHintsPerPeer+2; i++ {
		r.bundleWritten(534, [][]byte{[]byte(fmt.Sprintf("identity-%d", i))}, f.now.Add(time.Duration(i)*time.Second))
	}
	if got := r.knownBundles(534, f.now.Add((maxBundleHintsPerPeer+2)*time.Second)); len(got) != maxBundleHintsPerPeer {
		t.Fatalf("per-peer bundle hints=%d", len(got))
	}
	for i := int32(1); i <= maxBundleHintPeers+2; i++ {
		r.bundleWritten(i, bundle, f.now)
	}
	if got := len(r.bundleHints); got > maxBundleHintPeers {
		t.Fatalf("hint peers=%d", got)
	}
}

func TestEligibleSnapshotRequiresSignedPresenceAndOriginRoute(t *testing.T) {
	now := time.Now()
	addr, _, err := localmesh.Addresses(64, 535)
	if err != nil {
		t.Fatal(err)
	}
	router, err := localmesh.RouterID(64, 535)
	if err != nil {
		t.Fatal(err)
	}
	manifest := localmesh.Manifest{Org: 64, Asset: 535, Expires: now.Add(time.Minute).UnixMilli()}
	route := babel.Route{Prefix: netip.PrefixFrom(addr, 32), RouterID: router, Link: 2}
	view := localmesh.NodeSnapshot{Devices: []localmesh.Manifest{manifest}, Routes: []babel.Route{route}}
	if !eligibleSnapshot(view, 64, 535, now) {
		t.Fatal("live multi-hop route rejected")
	}
	checks := []struct {
		name   string
		change func(*localmesh.NodeSnapshot)
	}{
		{"missing manifest", func(v *localmesh.NodeSnapshot) { v.Devices = nil }},
		{"other org", func(v *localmesh.NodeSnapshot) { v.Devices[0].Org = 65 }},
		{"expired", func(v *localmesh.NodeSnapshot) { v.Devices[0].Expires = now.Add(-time.Second).UnixMilli() }},
		{"withdrawn", func(v *localmesh.NodeSnapshot) { v.Devices[0].Withdraw = true }},
		{"no route", func(v *localmesh.NodeSnapshot) { v.Routes = nil }},
		{"covering route", func(v *localmesh.NodeSnapshot) { v.Routes[0].Prefix = netip.PrefixFrom(addr, 24) }},
		{"wrong origin", func(v *localmesh.NodeSnapshot) { v.Routes[0].RouterID++ }},
		{"unreachable", func(v *localmesh.NodeSnapshot) { v.Routes[0].Unreachable = true }},
		{"local route", func(v *localmesh.NodeSnapshot) { v.Routes[0].Local = true }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			copy := localmesh.NodeSnapshot{Devices: append([]localmesh.Manifest(nil), view.Devices...), Routes: append([]babel.Route(nil), view.Routes...)}
			check.change(&copy)
			if eligibleSnapshot(copy, 64, 535, now) {
				t.Fatal("untrusted or unreachable peer admitted")
			}
		})
	}
}
