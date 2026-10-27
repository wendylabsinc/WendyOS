package meshcatalog

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestCollectorBoundsUntrustedObservationState(t *testing.T) {
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, p535, "default", nil, nil)
	source := net.ParseIP("10.79.99.162")
	c, err := NewCollector(AppScope{AppID: "com.wendy.test", AppIP: source, BridgeIndex: 7}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	observe := func(records ...dns.RR) {
		t.Helper()
		if _, err := c.Observe(&dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: records}, source, 7, f.now); err != nil {
			t.Fatal(err)
		}
	}
	address := func(name string, ttl uint32) dns.RR {
		return &dns.A{Hdr: dns.RR_Header{Name: name + ".local.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}, A: source}
	}
	// None of these incomplete observations produces a signed service. Their
	// memory must still be bounded independently of the catalogue entry limit.
	const limit = 4096
	for i := 0; i < limit+100; i += 100 {
		var records []dns.RR
		for j := i; j < i+100; j++ {
			records = append(records, address(fmt.Sprintf("name-%d", j), 60))
		}
		observe(records...)
	}
	if len(c.records) > limit {
		t.Fatalf("untrusted observations retained %d records, limit %d", len(c.records), limit)
	}
	// At capacity, an existing record can refresh and a goodbye frees space.
	observe(address("name-0", 120), address("name-1", 0), address("replacement", 60))
	if _, ok := c.records["replacement.local.|1"]; !ok {
		t.Fatal("goodbye did not free observation capacity")
	}
	if _, err := c.Sweep(f.now.Add(61 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(c.records) != 1 {
		t.Fatalf("expiry retained %d observations, want only refreshed record", len(c.records))
	}
	if _, ok := c.records["name-0.local.|1"]; !ok {
		t.Fatal("full cache prevented an existing observation's refresh")
	}
}

func TestKnownAnswerDoesNotSuppressWithZeroTTL(t *testing.T) {
	proposed := &dns.A{Hdr: dns.RR_Header{Name: "short.local.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1}, A: net.IPv4(10, 0, 0, 1)}
	known := dns.Copy(proposed)
	known.Header().Ttl = 0
	query := &dns.Msg{Answer: []dns.RR{known}}
	if knownAnswer(query, proposed) {
		t.Fatal("expired known answer suppressed a one-second refresh")
	}
	known.Header().Ttl = 1
	if !knownAnswer(query, proposed) {
		t.Fatal("current known answer was not suppressed")
	}
	proposed.Hdr.Ttl = ^uint32(0)
	if knownAnswer(query, proposed) {
		t.Fatal("short known lease suppressed a long-lived record")
	}
	known.Header().Ttl = ^uint32(0)
	if !knownAnswer(query, proposed) {
		t.Fatal("large lease overflowed known-answer comparison")
	}
}

func TestCollectorBoundsObservationBytesAndReleasesState(t *testing.T) {
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, p535, "default", nil, nil)
	source := net.ParseIP("10.79.99.162")
	c, err := NewCollector(AppScope{AppID: "com.wendy.test", AppIP: source, BridgeIndex: 7}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 1000 {
		rr := &dns.TXT{Hdr: dns.RR_Header{Name: fmt.Sprintf("large-%d.local.", i), Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60}, Txt: []string{strings.Repeat("x", 255), strings.Repeat("y", 255), strings.Repeat("z", 255), strings.Repeat("w", 255), strings.Repeat("a", 255)}}
		if _, err := c.Observe(&dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: []dns.RR{rr}}, source, 7, f.now); err != nil {
			t.Fatal(err)
		}
	}
	bytes := 0
	for _, item := range c.records {
		bytes += dns.Len(item.rr)
	}
	if bytes > 1<<20 || c.recordBytes != bytes {
		t.Fatalf("wire bytes=%d accounted=%d", bytes, c.recordBytes)
	}
	if len(c.records) >= 1000 {
		t.Fatal("large incomplete RRsets were retained without a byte bound")
	}
	if _, err := c.WithdrawAll(f.now); err != nil {
		t.Fatal(err)
	}
	if len(c.records) != 0 || c.recordBytes != 0 {
		t.Fatal("withdrawal retained observation state")
	}
}
