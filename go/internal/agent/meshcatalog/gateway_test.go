package meshcatalog

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGatewayOfferSignedLifecycleAndProjectionIsolation(t *testing.T) {
	f := newFixture(t)
	origin, _ := f.newCatalog(t, 533, "default", nil, nil)
	peer, _ := f.newCatalog(t, 534, "default", nil, nil)
	if _, err := peer.cache.Put(origin.creds.Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	first, err := origin.PublishGatewayOffer(f.now)
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := peer.Accept(first, f.now); err != nil || !changed {
		t.Fatalf("accept gateway offer: changed=%v err=%v", changed, err)
	}
	offers := peer.GatewayOffers(f.now)
	if len(offers) != 1 || !IsGatewayOffer(offers[0]) || offers[0].Key.Asset != 533 {
		t.Fatalf("verified gateway offers = %+v", offers)
	}
	projected, err := ProjectDNS(peer.Snapshot(f.now), "com.wendy.browser", func(string, Record) bool { return true }, f.now)
	if err != nil || len(projected) != 0 {
		t.Fatalf("gateway capability leaked into DNS-SD: records=%v err=%v", projected, err)
	}

	second, err := origin.PublishGatewayOffer(f.now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	var renewed Record
	if err := json.Unmarshal(second.Body, &renewed); err != nil {
		t.Fatal(err)
	}
	if renewed.Generation != offers[0].Generation+1 {
		t.Fatalf("renewal generation = %d, want %d", renewed.Generation, offers[0].Generation+1)
	}
	if _, err := peer.Accept(second, f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	withdraw, err := origin.WithdrawGatewayOffer(f.now.Add(time.Minute + time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if len(withdraw.Body) == 0 {
		t.Fatal("missing signed gateway withdrawal")
	}
	if _, err := peer.Accept(withdraw, f.now.Add(time.Minute+time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := peer.GatewayOffers(f.now.Add(time.Minute + time.Second)); len(got) != 0 {
		t.Fatalf("withdrawn gateway still active: %+v", got)
	}
	if _, err := peer.Accept(second, f.now.Add(time.Minute+2*time.Second)); !errors.Is(err, ErrStale) {
		t.Fatalf("stale offer replay accepted: %v", err)
	}
	if again, err := origin.WithdrawGatewayOffer(f.now.Add(time.Minute + 2*time.Second)); err != nil || len(again.Body) != 0 {
		t.Fatalf("duplicate withdrawal = %+v, %v", again, err)
	}
}

func TestGatewayOfferReservedShapeAndAppPublicationDenied(t *testing.T) {
	f := newFixture(t)
	c, _ := f.newCatalog(t, 533, "default", nil, nil)
	base := Record{Version: 1, Key: Key{"default", 64, 533, GatewayAppID, GatewayServiceID},
		Generation: 1, Issued: f.now.UnixMilli(), Expires: f.now.Add(GatewayOfferLease).UnixMilli()}
	if GatewayOfferLease != 600*time.Second {
		t.Fatalf("gateway lease = %s, want 600s", GatewayOfferLease)
	}
	if err := base.Validate(f.now); err != nil {
		t.Fatal(err)
	}
	tooLong := base
	tooLong.Expires = f.now.Add(GatewayOfferLease + time.Second).UnixMilli()
	if err := tooLong.Validate(f.now); err == nil {
		t.Fatal("accepted gateway lease longer than 600 seconds")
	}
	for name, mutate := range map[string]func(*Record){
		"endpoint": func(r *Record) { r.HostPort = 443 },
		"type":     func(r *Record) { r.Type = "_http._tcp" },
		"instance": func(r *Record) { r.Instance = "Gateway" },
		"txt":      func(r *Record) { r.TXT = []string{"capability=true"} },
		"app":      func(r *Record) { r.Key.AppID = "com.wendy.test" },
		"service":  func(r *Record) { r.Key.ServiceID = "http" },
	} {
		t.Run(name, func(t *testing.T) {
			r := base
			mutate(&r)
			if IsGatewayOffer(r) || r.Validate(f.now) == nil {
				t.Fatalf("invalid reserved gateway shape accepted: %+v", r)
			}
		})
	}
	for _, spec := range []PublishSpec{
		{AppID: GatewayAppID, ServiceID: GatewayServiceID},
		{AppID: "com.wendy.test", ServiceID: GatewayServiceID},
	} {
		if _, err := c.Publish(spec, f.now); err == nil || !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("app published gateway identity: %+v, %v", spec, err)
		}
	}
	if _, err := c.Remove(GatewayAppID, GatewayServiceID, f.now); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("app withdrew gateway identity: %v", err)
	}
	if _, err := c.PublishGatewayOffer(f.now); err != nil {
		t.Fatal(err)
	}
	if got := c.GatewayOffers(f.now.Add(GatewayOfferLease)); len(got) != 0 {
		t.Fatalf("expired gateway offered: %+v", got)
	}
}
