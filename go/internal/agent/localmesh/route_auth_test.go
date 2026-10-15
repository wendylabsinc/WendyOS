package localmesh

import (
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/babel"
)

func TestRouteAuthorizationRequiresLiveSignedOriginAndGatewayOffer(t *testing.T) {
	directory, chain, key, now := directoryFixture(t)
	var receipts []DirectoryReceipt
	if err := directory.SetPersistence(nil, func(next []DirectoryReceipt) error {
		receipts = append([]DirectoryReceipt(nil), next...)
		return nil
	}, now); err != nil {
		t.Fatal(err)
	}
	id, _ := RouterID(64, 445)
	host := netip.MustParsePrefix("10.88.1.189/32")
	defaultRoute := netip.MustParsePrefix("0.0.0.0/0")
	check := func(at time.Time, offers []int32, wantHost, wantDefault bool) {
		t.Helper()
		hosts, gateways := routeAuthorities(64, directory.Snapshot(at), offers, at)
		if got := routeAuthorized(64, hosts, gateways, id, host); got != wantHost {
			t.Fatalf("host authorization at %s = %t, want %t", at, got, wantHost)
		}
		if got := routeAuthorized(64, hosts, gateways, id, defaultRoute); got != wantDefault {
			t.Fatalf("default authorization at %s = %t, want %t", at, got, wantDefault)
		}
	}
	check(now, []int32{445}, false, false) // a neighbour's Babel claim is not proof
	m := Manifest{Version: 1, Org: 64, Asset: 445, Revision: 1, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 50052, Internet: true}
	w, err := SignManifest(m, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	check(now, nil, true, false) // signed Internet bit alone cannot authorize a default
	check(now, []int32{445}, true, true)
	check(now.Add(time.Minute), []int32{445}, false, false) // absolute lease expiry
	m.Revision = 2
	m.Withdraw = true
	m.Internet = false
	m.Expires = now.Add(time.Minute).UnixMilli()
	w, err = SignManifest(m, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := directory.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	check(now, []int32{445}, false, false) // signed withdrawal revokes both routes
	// Restart restores replay receipts but never live authorizations. A peer
	// must supply a fresh verified manifest before any route is admitted.
	restarted, err := NewDirectory(64, 10, directory.cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.SetPersistence(receipts, func([]DirectoryReceipt) error { return nil }, now); err != nil {
		t.Fatal(err)
	}
	directory = restarted
	check(now, []int32{445}, false, false)
}

func TestRouteAuthorizationRejectsWrongOriginAndPrefix(t *testing.T) {
	now := time.Now()
	hosts, gateways := routeAuthorities(64, []Manifest{{Org: 64, Asset: 445, Expires: now.Add(time.Minute).UnixMilli(), Internet: true}}, []int32{445}, now)
	id, _ := RouterID(64, 445)
	wrong, _ := RouterID(64, 460)
	for _, check := range []struct {
		id     uint64
		prefix string
	}{
		{uint64(wrong), "10.88.1.189/32"},
		{uint64(id), "10.88.1.204/32"},
		{uint64(id), "10.88.1.0/24"},
		{uint64(id), "::/0"},
	} {
		if routeAuthorized(64, hosts, gateways, babel.RouterID(check.id), netip.MustParsePrefix(check.prefix)) {
			t.Fatalf("admitted wrong origin/prefix %d %s", check.id, check.prefix)
		}
	}
}
