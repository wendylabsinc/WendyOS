package meshcatalog

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestManifestCollectorSteadyDNSAndReplacement(t *testing.T) {
	f := newFixture(t)
	device := f.creds[p535].Device
	now := f.now
	view := localmesh.NodeSnapshot{Self: device}
	publications := 0
	catalog, err := NewManifestCatalog("default", device, func(string, string, uint16) error { return nil },
		func(services []localmesh.ManifestService) error {
			if err := localmesh.CheckManifestServices(services); err != nil {
				return err
			}
			publications++
			view.Devices = []localmesh.Manifest{{Device: device.UUID(), Revision: uint64(publications),
				Issued: now.UnixMilli(), Expires: now.Add(15 * time.Minute).UnixMilli(), Services: services}}
			return nil
		}, func() localmesh.NodeSnapshot { return view })
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCollector(AppScope{AppID: "com.wendy.test", AppIP: net.ParseIP("10.79.99.162"), BridgeIndex: 7,
		Ports: map[uint16]uint16{8080: 18080}}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	msg := announcement("Camera", 120, 8080, c.scope.AppIP.String())
	for i := 0; i <= 20; i++ {
		now = f.now.Add(time.Duration(i) * 30 * time.Second)
		changed, err := c.Observe(msg, c.scope.AppIP, 7, now)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && len(changed) != 0 {
			t.Fatalf("unchanged DNS caused publication at %s", now.Sub(f.now))
		}
	}
	if publications != 1 {
		t.Fatalf("steady DNS publications = %d", publications)
	}
	now = now.Add(time.Second)
	msg.Extra[1].(*dns.TXT).Txt = []string{"path=/changed"}
	if _, err = c.Observe(msg, c.scope.AppIP, 7, now); err != nil {
		t.Fatal(err)
	}
	rows := catalog.Snapshot(now)
	if publications != 2 || len(rows) != 1 || rows[0].TXT[0] != "path=/changed" || rows[0].Expires != now.Add(15*time.Minute).UnixMilli() {
		t.Fatalf("updated complete manifest: publications=%d rows=%+v", publications, rows)
	}
	if _, err = catalog.Publish(PublishSpec{AppID: "com.wendy.test", ServiceID: "other", Type: "_http._tcp", Instance: "Other", HostPort: 18080}, now); err != nil {
		t.Fatal(err)
	}
	rows = catalog.Snapshot(now)
	if len(rows) != 2 || rows[0].Expires != rows[1].Expires || rows[0].Generation != rows[1].Generation {
		t.Fatalf("lease/revision not inherited: %+v", rows)
	}
	if _, err = catalog.Remove("com.wendy.test", "other", now); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Snapshot(now)) != 1 {
		t.Fatal("omitted service survived replacement")
	}
	// The refreshed DNS source still disappears on its own TTL. That event
	// changes the list, while the earlier TTL renewals did not renew authority.
	if _, err = c.Sweep(now.Add(120 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(view.Devices[0].Services) != 0 {
		t.Fatal("expired DNS service survived")
	}
}
