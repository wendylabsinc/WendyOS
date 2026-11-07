package meshcatalog

import (
	"bytes"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestAdmittedCatalogLeaseIgnoresOriginWallSkewAndExpiresProjection(t *testing.T) {
	now := time.Unix(1800000000, 0)
	owner, err := meshid.DeviceFromPrincipal(p533)
	if err != nil {
		t.Fatal(err)
	}
	for _, skew := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			issued := now.Add(skew)
			m := localmesh.Manifest{Device: owner.UUID(), Revision: 7, Issued: issued.UnixMilli(), Expires: issued.Add(time.Minute).UnixMilli(), AdmittedUntil: now.Add(20 * time.Second), Services: []localmesh.ManifestService{{Mesh: "default", AppID: "com.wendy.test", ServiceID: "http", Type: "_http._tcp", Instance: "Camera", Port: 18080}}}
			view := localmesh.NodeSnapshot{Self: owner, Devices: []localmesh.Manifest{m}, Routes: []babel.Route{{Prefix: netip.PrefixFrom(owner.MeshAddress(), 128), Link: 1}}}
			catalog, err := NewManifestCatalog("default", owner, func(string, string, uint16) error { return nil }, func([]localmesh.ManifestService) error { return nil }, func() localmesh.NodeSnapshot { return view })
			if err != nil {
				t.Fatal(err)
			}
			records := catalog.Snapshot(now)
			if len(records) != 1 || records[0].Issued != m.Issued || records[0].Expires != m.Expires || records[0].LeaseRemaining(now) != 20*time.Second {
				t.Fatalf("projection lost immutable dates or admitted lease: %+v", records)
			}
			if !eligibleSnapshot(view, owner, now) {
				t.Fatal("live admitted manifest rejected")
			}
			projected, err := ProjectDNS(records, "com.wendy.browser", func(string, Record) bool { return true }, testVIPs(t), now)
			if err != nil || len(projected) != 4 {
				t.Fatal(projected, err)
			}
			for _, rr := range projected {
				if rr.Header().Ttl != 20 {
					t.Fatalf("DNS escaped admitted deadline: %v", rr)
				}
			}
			bridge := &MDNSBridge{projected: map[string]dns.RR{}}
			bridge.projectionDelta(projected)
			// Re-reading the same admitted revision does not mint a new lease.
			late := catalog.Snapshot(now.Add(19 * time.Second))
			if len(late) != 1 || late[0].LeaseRemaining(now.Add(19*time.Second)) != time.Second {
				t.Fatalf("catalog refreshed repeated snapshot: %+v", late)
			}
			expired := now.Add(20 * time.Second)
			if got := catalog.Snapshot(expired); len(got) != 0 {
				t.Fatalf("expired service still present: %+v", got)
			}
			if eligibleSnapshot(view, owner, expired) {
				t.Fatal("expired admission remained route eligible")
			}
			// A previously captured projection must also expire, so one stale record
			// cannot prevent withdrawal of the bridge's complete previous DNS set.
			projected, err = ProjectDNS(records, "com.wendy.browser", func(string, Record) bool { return true }, testVIPs(t), expired)
			if err != nil || len(projected) != 0 {
				t.Fatal(projected, err)
			}
			_, gone := bridge.projectionDelta(projected)
			if len(gone) != 4 {
				t.Fatalf("expired service retained %d DNS records", 4-len(gone))
			}
			view.Routes = nil
			if eligibleSnapshot(view, owner, now) {
				t.Fatal("admission bypassed live route requirement")
			}
		})
	}
}

func TestCatalogAdmissionMetadataIsNotWireData(t *testing.T) {
	now := time.Unix(1800000000, 0)
	r := Record{Version: 1, Key: Key{"default", p533, "com.wendy.test", "http"}, Generation: 1, Type: "_http._tcp", Instance: "Camera", HostPort: 18080, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli()}
	beforeJSON, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	beforeCBOR, err := netwire.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	r.AdmittedUntil = now.Add(30 * time.Second)
	afterJSON, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	afterCBOR, err := netwire.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeJSON, afterJSON) || !bytes.Equal(beforeCBOR, afterCBOR) {
		t.Fatal("local lease metadata changed wire bytes")
	}
	r.Expires = r.Issued + (MaxLease + time.Second).Milliseconds()
	if err := r.Validate(now); err == nil {
		t.Fatal("admission metadata bypassed signed duration bound")
	}
}
