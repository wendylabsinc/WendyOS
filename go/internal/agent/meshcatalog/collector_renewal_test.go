package meshcatalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func renewalCollector(t *testing.T, persist func([]Receipt) error) (fixture, *Collector) {
	t.Helper()
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, 535, "default", nil, persist)
	c, e := NewCollector(AppScope{AppID: "com.wendy.test", AppIP: net.ParseIP("10.79.99.162"), BridgeIndex: 7, Ports: map[uint16]uint16{8080: 18080}}, catalog)
	if e != nil {
		t.Fatal(e)
	}
	return f, c
}
func renewalRecord(t *testing.T, w SignedRecord) Record {
	t.Helper()
	var r Record
	if e := json.Unmarshal(w.Body, &r); e != nil {
		t.Fatal(e)
	}
	return r
}
func renewalObserve(t *testing.T, c *Collector, m *dns.Msg, now time.Time) []SignedRecord {
	t.Helper()
	w, e := c.Observe(m, c.scope.AppIP, 7, now)
	if e != nil {
		t.Fatal(e)
	}
	return w
}

func TestCollectorUnchangedDeadlineDoesNotResign(t *testing.T) {
	f, c := renewalCollector(t, nil)
	initial := renewalObserve(t, c, announcement("Camera", 120, 8080, c.scope.AppIP.String()), f.now)
	if len(initial) != 1 {
		t.Fatal("initial publish")
	}
	first := renewalRecord(t, initial[0])
	syncer := NewSynchronizer(c.catalog, c.catalog.cache)
	_ = syncer.Record(initial[0], f.now)
	var frames bytes.Buffer
	changes := 0
	for _, offset := range []time.Duration{60, 90, 105, 113, 117, 119} {
		now := f.now.Add(offset * time.Second)
		w, e := c.Sweep(now)
		if e != nil {
			t.Fatal(e)
		}
		for _, wire := range w {
			r := renewalRecord(t, wire)
			t.Logf("offset=%s generation=%d expires=%d initialExpires=%d", offset*time.Second, r.Generation, r.Expires, first.Expires)
			changes++
			for _, m := range syncer.Record(wire, now) {
				if e = WriteMessage(&frames, m); e != nil {
					t.Fatal(e)
				}
			}
		}
	}
	t.Logf("unchanged deadline redundant generations=%d framed record bytes=%d", changes, frames.Len())
	if changes != 0 || frames.Len() != 0 {
		t.Errorf("unchanged DNS deadline re-signed %d records / %d framed bytes", changes, frames.Len())
	}
	w, e := c.Sweep(f.now.Add(120 * time.Second))
	if e != nil || len(w) != 1 {
		t.Fatalf("expiry withdrawal: %d %v", len(w), e)
	}
	r := renewalRecord(t, w[0])
	if !r.Withdraw || r.Generation <= first.Generation {
		t.Fatal("missing expiry withdrawal")
	}
	if w, e = c.Sweep(f.now.Add(121 * time.Second)); e != nil || len(w) != 0 {
		t.Fatal("repeated withdrawal", e)
	}
}

func TestCollectorFreshDeadlineAndContentRenewal(t *testing.T) {
	f, c := renewalCollector(t, nil)
	m := announcement("Camera", 120, 8080, c.scope.AppIP.String())
	first := renewalObserve(t, c, m, f.now)
	r0 := renewalRecord(t, first[0])
	// A refresh before half lease is remembered but does not immediately churn.
	if w := renewalObserve(t, c, m, f.now.Add(30*time.Second)); len(w) != 0 {
		t.Fatal("early refresh churn")
	}
	w, e := c.Sweep(f.now.Add(60 * time.Second))
	if e != nil || len(w) != 1 {
		t.Fatalf("fresh source renewal: %d %v", len(w), e)
	}
	r1 := renewalRecord(t, w[0])
	if r1.Expires != f.now.Add(150*time.Second).UnixMilli() || r1.Generation != r0.Generation+1 {
		t.Fatal("wrong refreshed expiry/generation", r1)
	}
	// Content changes must bypass the renewal timer and sign immediately.
	changed := announcement("Camera", 120, 8080, c.scope.AppIP.String())
	changed.Extra[1].(*dns.TXT).Txt = []string{"path=/changed"}
	w = renewalObserve(t, c, changed, f.now.Add(61*time.Second))
	if len(w) != 1 {
		t.Fatal("content change delayed")
	}
	r2 := renewalRecord(t, w[0])
	if r2.Generation != r1.Generation+1 || r2.TXT[0] != "path=/changed" || r2.Expires != f.now.Add(181*time.Second).UnixMilli() {
		t.Fatal("wrong content update", r2)
	}
	goodbye := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: []dns.RR{&dns.PTR{Hdr: dns.RR_Header{Name: "_http._tcp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 0}, Ptr: "Camera._http._tcp.local."}}}
	w = renewalObserve(t, c, goodbye, f.now.Add(62*time.Second))
	if len(w) != 1 || !renewalRecord(t, w[0]).Withdraw {
		t.Fatal("goodbye delayed")
	}
}

func TestCollectorDNSClampAndCappedSyntheticRenewal(t *testing.T) {
	f, c := renewalCollector(t, nil)
	w := renewalObserve(t, c, announcement("Camera", 600, 8080, c.scope.AppIP.String()), f.now)
	if len(w) != 1 {
		t.Fatal("initial")
	}
	for _, rr := range c.records {
		if !rr.expires.Equal(f.now.Add(MaxLease)) {
			t.Fatal("source RR policy changed")
		}
	}
	// Exercise the general signed-lease cap with synthetic stored deadlines only.
	// Production Observe deliberately clamps RR authority to MaxLease.
	for key, rr := range c.records {
		rr.expires = f.now.Add(300 * time.Second)
		c.records[key] = rr
	}
	for _, seconds := range []int{60, 120, 180} {
		now := f.now.Add(time.Duration(seconds) * time.Second)
		w, e := c.Sweep(now)
		if e != nil || len(w) != 1 {
			t.Fatalf("capped renewal %d: %d %v", seconds, len(w), e)
		}
		r := renewalRecord(t, w[0])
		if r.Expires != now.Add(MaxLease).UnixMilli() {
			t.Fatal("signed cap not preserved", r)
		}
	}
	for _, seconds := range []int{240, 270, 299} {
		w, e := c.Sweep(f.now.Add(time.Duration(seconds) * time.Second))
		if e != nil || len(w) != 0 {
			t.Fatal("resigned unchanged synthetic terminal deadline", seconds, len(w), e)
		}
	}
	w, e := c.Sweep(f.now.Add(300 * time.Second))
	if e != nil || len(w) != 1 || !renewalRecord(t, w[0]).Withdraw {
		t.Fatal("synthetic authority expiry not removed")
	}
}

func TestCollectorRenewalPersistenceFailureRetries(t *testing.T) {
	fail := false
	f, c := renewalCollector(t, func([]Receipt) error {
		if fail {
			return errors.New("injected persist failure")
		}
		return nil
	})
	m := announcement("Camera", 120, 8080, c.scope.AppIP.String())
	_ = renewalObserve(t, c, m, f.now)
	_ = renewalObserve(t, c, m, f.now.Add(30*time.Second))
	fail = true
	if w, e := c.Sweep(f.now.Add(60 * time.Second)); e == nil || len(w) != 0 {
		t.Fatal("persistence failure hidden")
	}
	fail = false
	w, e := c.Sweep(f.now.Add(61 * time.Second))
	if e != nil || len(w) != 1 {
		t.Fatalf("failed renewal suppressed retry: %d %v", len(w), e)
	}
	r := renewalRecord(t, w[0])
	if r.Generation != 2 || r.Expires != f.now.Add(150*time.Second).UnixMilli() {
		t.Fatal("failed persistence advanced generation or expiry", r)
	}
	if w, e = c.Sweep(f.now.Add(106 * time.Second)); e != nil || len(w) != 0 {
		t.Fatal("retry caused duplicate unchanged-deadline generation")
	}
}
