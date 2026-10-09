package meshcatalog

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestProjectionDeltaAnnouncesNewAndWithdrawsChangedRecords(t *testing.T) {
	b := &MDNSBridge{projected: map[string]dns.RR{}}
	ptr := &dns.PTR{Hdr: dns.RR_Header{Name: "_http._tcp.local.", Rrtype: dns.TypePTR,
		Class: dns.ClassINET, Ttl: 60}, Ptr: "Camera._http._tcp.local."}
	added, gone := b.projectionDelta([]dns.RR{ptr})
	if len(added) != 1 || len(gone) != 0 {
		t.Fatalf("new remote service: added=%d gone=%d", len(added), len(gone))
	}
	shorter := dns.Copy(ptr)
	shorter.Header().Ttl = 30
	added, gone = b.projectionDelta([]dns.RR{shorter})
	if len(added) != 0 || len(gone) != 0 {
		t.Fatalf("TTL countdown changed identity: added=%d gone=%d", len(added), len(gone))
	}
	replaced := dns.Copy(ptr).(*dns.PTR)
	replaced.Ptr = "Camera2._http._tcp.local."
	added, gone = b.projectionDelta([]dns.RR{replaced})
	if len(added) != 1 || len(gone) != 1 || gone[0].Header().Ttl != 0 {
		t.Fatalf("changed service: added=%d gone=%v", len(added), gone)
	}
	added, gone = b.projectionDelta(nil)
	if len(added) != 0 || len(gone) != 1 || gone[0].Header().Ttl != 0 {
		t.Fatalf("withdrawn service: added=%d gone=%v", len(added), gone)
	}
}

func TestAnnouncementPacketizationAndCacheFlush(t *testing.T) {
	var records []dns.RR
	for i := range 80 {
		records = append(records, &dns.PTR{Hdr: dns.RR_Header{Name: "_http._tcp.local.",
			Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 60},
			Ptr: fmt.Sprintf("Camera-%d._http._tcp.local.", i)})
	}
	records = append(records, &dns.SRV{Hdr: dns.RR_Header{Name: "Camera-0._http._tcp.local.",
		Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 60}, Port: 18080, Target: "device-42.mesh.local."})
	packets, err := packMDNSAnnouncements(records)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) < 2 {
		t.Fatalf("large announcement was not split: %d packets", len(packets))
	}
	count, flushed := 0, false
	for _, data := range packets {
		if len(data) > 1400 {
			t.Fatalf("oversized mDNS packet: %d", len(data))
		}
		var msg dns.Msg
		if err := msg.Unpack(data); err != nil || !msg.Response || !msg.Authoritative {
			t.Fatalf("invalid announcement: %v, %+v", err, msg.MsgHdr)
		}
		for _, rr := range msg.Answer {
			count++
			if rr.Header().Rrtype == dns.TypePTR && rr.Header().Class != dns.ClassINET {
				t.Fatal("shared PTR has cache-flush bit")
			}
			if rr.Header().Rrtype == dns.TypeSRV && rr.Header().Class&0x8000 != 0 {
				flushed = true
			}
		}
	}
	if count != len(records) || !flushed {
		t.Fatalf("announcement omitted records or cache flush: count=%d flush=%v", count, flushed)
	}
	if got := announcementRefreshInterval(records); got != 30*time.Second {
		t.Fatalf("refresh interval: %s", got)
	}
	records[0].Header().Ttl = 4
	if got := announcementRefreshInterval(records); got != 2*time.Second {
		t.Fatalf("short TTL refresh interval: %s", got)
	}
}

func TestProjectionGoodbyesAndScopedAnswer(t *testing.T) {
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, 533, "default", nil, nil)
	scope := AppScope{AppID: "com.wendy.test", AppIP: net.IPv4(10, 77, 0, 2), BridgeIndex: 7,
		AllowedTypes: []string{"_http._tcp"}, Ports: map[uint16]uint16{8080: 18080}}
	bridge, err := NewMDNSBridge(scope, catalog, func(_ string, _ Record) bool { return true }, func(SignedRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	w, err := catalog.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	_ = w
	projected, err := bridge.projection(f.now.Add(time.Second))
	if err != nil || len(projected) != 4 {
		t.Fatalf("projection: %d, %v", len(projected), err)
	}
	if gone := bridge.projectionGoodbyes(projected); len(gone) != 0 {
		t.Fatalf("initial projection produced %d goodbyes", len(gone))
	}
	query := new(dns.Msg)
	query.Question = []dns.Question{{Name: "_http._tcp.local.", Qtype: dns.TypePTR, Qclass: dns.ClassINET}}
	response := AnswerMDNS(query, projected)
	if response == nil || len(response.Answer) != 1 || len(response.Extra) != 3 {
		t.Fatalf("PTR browse response = %+v", response)
	}
	if response.Answer[0].Header().Ttl > 89 {
		t.Fatal("projection exceeded signed lease")
	}
	query.Answer = append(query.Answer, dns.Copy(response.Answer[0]))
	if answer := AnswerMDNS(query, projected); answer != nil {
		t.Fatal("known-answer suppression failed")
	}
	goodbyes := bridge.projectionGoodbyes(nil)
	if len(goodbyes) != 4 {
		t.Fatalf("missing projection goodbyes: %d", len(goodbyes))
	}
	for _, rr := range goodbyes {
		if rr.Header().Ttl != 0 {
			t.Fatal("goodbye has positive TTL")
		}
	}
}

func TestBridgeLearnsBoundedServiceTypes(t *testing.T) {
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, 533, "default", nil, nil)
	bridge, err := NewMDNSBridge(AppScope{AppID: "com.wendy.test", AppIP: net.IPv4(10, 77, 0, 2),
		BridgeIndex: 7, Ports: map[uint16]uint16{8080: 18080}}, catalog,
		func(string, Record) bool { return true }, func(SignedRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	response := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: []dns.RR{
		&dns.PTR{Hdr: dns.RR_Header{Name: "_services._dns-sd._udp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 300}, Ptr: "_http._tcp.local."},
		&dns.PTR{Hdr: dns.RR_Header{Name: "_services._dns-sd._udp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 300}, Ptr: "evil.local."},
	}}
	if fresh := bridge.learnTypes(response, f.now); len(fresh) != 1 || fresh[0] != "_http._tcp" {
		t.Fatalf("learned %v", fresh)
	}
	if names := bridge.knownTypes(f.now.Add(MaxLease - time.Second)); len(names) != 1 {
		t.Fatalf("bounded lease: %v", names)
	}
	if names := bridge.knownTypes(f.now.Add(MaxLease)); len(names) != 0 {
		t.Fatalf("expired type: %v", names)
	}
}

func TestAppResponderTakesPrecedenceOverProjectedName(t *testing.T) {
	f := newFixture(t)
	remote, _ := f.newCatalog(t, 535, "default", nil, nil)
	local, cache := f.newCatalog(t, 533, "default", nil, nil)
	if _, err := cache.Put(f.creds[535].Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	w, err := remote.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.Accept(w, f.now); err != nil {
		t.Fatal(err)
	}
	scope := AppScope{AppID: "com.wendy.test", AppIP: net.IPv4(10, 77, 0, 2), BridgeIndex: 7,
		Ports: map[uint16]uint16{8080: 18080}}
	bridge, err := NewMDNSBridge(scope, local, func(_ string, r Record) bool { return r.Key.Asset != 533 }, func(SignedRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	projected, err := bridge.projection(f.now)
	if err != nil || len(projected) != 4 {
		t.Fatalf("initial projection = %d, %v", len(projected), err)
	}
	bridge.projectionDelta(projected)
	instance := projectedInstanceName(local.Snapshot(f.now)[0])
	header := func(name string, typ uint16, ttl uint32) dns.RR_Header {
		return dns.RR_Header{Name: name, Rrtype: typ, Class: dns.ClassINET, Ttl: ttl}
	}
	appReply := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: []dns.RR{
		&dns.PTR{Hdr: header("_http._tcp.local.", dns.TypePTR, 60), Ptr: instance},
	}, Extra: []dns.RR{
		&dns.SRV{Hdr: header(instance, dns.TypeSRV, 60), Port: 8080, Target: "app.local."},
		&dns.TXT{Hdr: header(instance, dns.TypeTXT, 60), Txt: []string{"owner=app"}},
		&dns.A{Hdr: header("app.local.", dns.TypeA, 60), A: scope.AppIP},
	}}
	if err := bridge.observe(appReply, net.IPv4(10, 77, 0, 99), scope.BridgeIndex, f.now); err != nil {
		t.Fatal(err)
	}
	if p, err := bridge.projection(f.now); err != nil || len(p) != 4 {
		t.Fatalf("other source suppressed projection: %d, %v", len(p), err)
	}
	if err := bridge.observe(appReply, scope.AppIP, scope.BridgeIndex, f.now); err != nil {
		t.Fatal(err)
	}
	blocked, err := bridge.projection(f.now.Add(time.Second))
	if err != nil || len(blocked) != 0 {
		t.Fatalf("two responders kept colliding name visible: %d, %v", len(blocked), err)
	}
	added, gone := bridge.projectionDelta(blocked)
	if len(added) != 0 || len(gone) != 4 {
		t.Fatalf("conflict withdrawal = added %d, gone %d", len(added), len(gone))
	}
	if safe := bridge.unclaimedGoodbyes(gone, f.now.Add(time.Second)); len(safe) != 1 || safe[0].Header().Rrtype != dns.TypeA {
		t.Fatalf("conflict goodbye could evict app-owned RRsets: %v", safe)
	}
	// The native responder's goodbye releases its claim and the signed
	// remote service becomes visible again without changing its identity.
	goodbye := appReply.Copy()
	for _, section := range [][]dns.RR{goodbye.Answer, goodbye.Extra} {
		for _, rr := range section {
			rr.Header().Ttl = 0
		}
	}
	if err := bridge.observe(goodbye, scope.AppIP, scope.BridgeIndex, f.now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	restored, err := bridge.projection(f.now.Add(2 * time.Second))
	if err != nil || len(restored) != 4 {
		t.Fatalf("projection after native goodbye = %d, %v", len(restored), err)
	}
	added, gone = bridge.projectionDelta(restored)
	if len(added) != 4 || len(gone) != 0 {
		t.Fatalf("reannouncement after native goodbye = added %d, gone %d", len(added), len(gone))
	}
	// A probe's authority section reserves the same name before its first
	// announcement, but the reservation expires if it is abandoned.
	probe := &dns.Msg{Question: []dns.Question{{Name: instance, Qtype: dns.TypeANY, Qclass: dns.ClassINET}},
		Ns: []dns.RR{appReply.Extra[0]}}
	bridge.observeAppClaims(probe, scope.AppIP, scope.BridgeIndex, f.now.Add(3*time.Second))
	if p, _ := bridge.projection(f.now.Add(3 * time.Second)); len(p) != 0 {
		t.Fatalf("probe collision projected %d records", len(p))
	}
	if p, _ := bridge.projection(f.now.Add(6 * time.Second)); len(p) != 4 {
		t.Fatalf("abandoned probe left projection suppressed: %d", len(p))
	}
	hostReply := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: []dns.RR{
		&dns.A{Hdr: header(projectedHostName(remote.Snapshot(f.now)[0]), dns.TypeA, 4500), A: scope.AppIP},
	}}
	bridge.observeAppClaims(hostReply, scope.AppIP, scope.BridgeIndex, f.now.Add(7*time.Second))
	if p, _ := bridge.projection(f.now.Add(8 * time.Second)); len(p) != 0 {
		t.Fatalf("app host A collision projected %d records", len(p))
	}
	// A native hostname's TTL can outlive the signed service lease. Its
	// ownership claim must not be shortened to MaxLease.
	if !bridge.appClaimsName(projectedHostName(remote.Snapshot(f.now)[0])) {
		t.Fatal("app host claim was not retained")
	}
	hostReply.Answer[0].Header().Ttl = 0
	bridge.observeAppClaims(hostReply, scope.AppIP, scope.BridgeIndex, f.now.Add(9*time.Second))
	if p, _ := bridge.projection(f.now.Add(9 * time.Second)); len(p) != 4 {
		t.Fatalf("host goodbye did not restore projection: %d", len(p))
	}
	bridge.projectionDelta(projected)
	withdraw, err := remote.Remove("com.wendy.test", "http", f.now.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.Accept(withdraw, f.now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	removed, err := bridge.projection(f.now.Add(10 * time.Second))
	if err != nil || len(removed) != 0 {
		t.Fatalf("signed removal projection = %d, %v", len(removed), err)
	}
	_, gone = bridge.projectionDelta(removed)
	goodbyes, err := packMDNSAnnouncements(bridge.unclaimedGoodbyes(gone, f.now.Add(10*time.Second)))
	if err != nil || len(goodbyes) == 0 {
		t.Fatalf("pack signed-removal goodbyes = %d, %v", len(goodbyes), err)
	}
	seen := 0
	for _, packet := range goodbyes {
		var msg dns.Msg
		if err := msg.Unpack(packet); err != nil {
			t.Fatal(err)
		}
		for _, rr := range msg.Answer {
			seen++
			if rr.Header().Ttl != 0 || (rr.Header().Class&0x8000 != 0) != (rr.Header().Rrtype != dns.TypePTR) {
				t.Fatalf("bad signed-removal goodbye on wire: %s", rr)
			}
		}
	}
	if seen != 4 {
		t.Fatalf("signed-removal goodbye record count = %d", seen)
	}
}

func TestMDNSResponseWireCacheFlushAndLegacyUnicast(t *testing.T) {
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, 535, "default", nil, nil)
	if _, err := catalog.Publish(testSpec(), f.now); err != nil {
		t.Fatal(err)
	}
	projected, err := ProjectDNS(catalog.Snapshot(f.now), "com.wendy.client", func(string, Record) bool { return true }, f.now)
	if err != nil {
		t.Fatal(err)
	}
	query := &dns.Msg{MsgHdr: dns.MsgHdr{Id: 42}, Question: []dns.Question{{Name: "_http._tcp.local.", Qtype: dns.TypePTR, Qclass: dns.ClassINET}}}
	response := AnswerMDNS(query, projected)
	for _, legacy := range []bool{false, true} {
		wire, err := packMDNSResponse(response, legacy)
		if err != nil {
			t.Fatal(err)
		}
		var msg dns.Msg
		if err := msg.Unpack(wire); err != nil {
			t.Fatal(err)
		}
		if len(msg.Answer) != 1 || len(msg.Extra) != 3 {
			t.Fatalf("incomplete DNS-SD response: %+v", msg)
		}
		if legacy && (msg.Id != 42 || len(msg.Question) != 1) || !legacy && (msg.Id != 0 || len(msg.Question) != 0) {
			t.Fatalf("response ID/question: legacy=%v id=%d questions=%d", legacy, msg.Id, len(msg.Question))
		}
		for _, rr := range append(msg.Answer, msg.Extra...) {
			flush := rr.Header().Class&0x8000 != 0
			if flush != (!legacy && rr.Header().Rrtype != dns.TypePTR) {
				t.Fatalf("cache-flush: legacy=%v rr=%s", legacy, rr)
			}
			if legacy && rr.Header().Ttl > 10 {
				t.Fatalf("legacy TTL exceeds 10s: %s", rr)
			}
		}
	}
}

func TestAppClaimLimitSuppressesProjectionUntilClaimsExpire(t *testing.T) {
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, 535, "default", nil, nil)
	if _, err := catalog.Publish(testSpec(), f.now); err != nil {
		t.Fatal(err)
	}
	scope := AppScope{AppID: "com.wendy.test", AppIP: net.IPv4(10, 77, 0, 2), BridgeIndex: 7,
		Ports: map[uint16]uint16{8080: 18080}}
	bridge, err := NewMDNSBridge(scope, catalog, func(string, Record) bool { return true }, func(SignedRecord) {})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxAppClaimNames; i++ {
		msg := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: []dns.RR{
			&dns.A{Hdr: dns.RR_Header{Name: fmt.Sprintf("native-%d.local.", i), Rrtype: dns.TypeA,
				Class: dns.ClassINET, Ttl: 60}, A: scope.AppIP},
		}}
		bridge.observeAppClaims(msg, scope.AppIP, scope.BridgeIndex, f.now)
	}
	if len(bridge.appClaims) != maxAppClaimNames {
		t.Fatalf("unbounded claim table: %d", len(bridge.appClaims))
	}
	if p, _ := bridge.projection(f.now); len(p) != 0 {
		t.Fatalf("overflow exposed %d projected records", len(p))
	}
	if p, _ := bridge.projection(f.now.Add(61 * time.Second)); len(p) != 4 {
		t.Fatalf("expired claims still suppress projection: %d", len(p))
	}
}
