package meshcatalog

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func announcement(instance string, ttl uint32, port uint16, address string) *dns.Msg {
	typeName := "_http._tcp.local."
	full := instance + "." + typeName
	target := strings.ToLower(instance) + ".local."
	header := func(name string, typ uint16) dns.RR_Header {
		return dns.RR_Header{Name: name, Rrtype: typ, Class: dns.ClassINET, Ttl: ttl}
	}
	return &dns.Msg{MsgHdr: dns.MsgHdr{Response: true},
		Answer: []dns.RR{&dns.PTR{Hdr: header(typeName, dns.TypePTR), Ptr: full}},
		Extra: []dns.RR{
			&dns.SRV{Hdr: header(full, dns.TypeSRV), Port: port, Target: target},
			&dns.TXT{Hdr: header(full, dns.TypeTXT), Txt: []string{"path=/"}},
			&dns.A{Hdr: header(target, dns.TypeA), A: net.ParseIP(address)},
		}}
}

func TestCollectorBindsAppSourcePortAndGoodbye(t *testing.T) {
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, 535, "default", nil, nil)
	collector, err := NewCollector(AppScope{AppID: "com.wendy.test", AppIP: net.ParseIP("10.79.99.162"),
		BridgeIndex: 7, AllowedTypes: []string{"_http._tcp"}, Ports: map[uint16]uint16{8080: 18080}}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	msg := announcement("Camera", 60, 8080, "10.79.99.162")
	if changed, err := collector.Observe(msg, net.ParseIP("10.79.99.163"), 7, f.now); err != nil || len(changed) != 0 {
		t.Fatalf("other app IP admitted: %d %v", len(changed), err)
	}
	if changed, err := collector.Observe(msg, net.ParseIP("10.79.99.162"), 8, f.now); err != nil || len(changed) != 0 {
		t.Fatalf("other bridge admitted: %d %v", len(changed), err)
	}
	if changed, err := collector.Observe(announcement("WrongPort", 60, 8081, "10.79.99.162"), net.ParseIP("10.79.99.162"), 7, f.now); err != nil || len(changed) != 0 {
		t.Fatalf("undeclared port admitted: %d %v", len(changed), err)
	}
	if changed, err := collector.Observe(announcement("WrongIP", 60, 8080, "10.79.99.163"), net.ParseIP("10.79.99.162"), 7, f.now); err != nil || len(changed) != 0 {
		t.Fatalf("forged address admitted: %d %v", len(changed), err)
	}
	changed, err := collector.Observe(msg, net.ParseIP("10.79.99.162"), 7, f.now)
	if err != nil || len(changed) != 1 {
		t.Fatalf("valid app publication = %d %v", len(changed), err)
	}
	records := catalog.Snapshot(f.now)
	if len(records) != 1 || records[0].HostPort != 18080 || records[0].Instance != "Camera" {
		t.Fatalf("signed publication = %+v", records)
	}
	if changed, err := collector.Observe(msg, net.ParseIP("10.79.99.162"), 7, f.now.Add(time.Second)); err != nil || len(changed) != 0 {
		t.Fatalf("duplicate advertisement caused generation churn: %d %v", len(changed), err)
	}
	second := announcement("Other", 60, 8080, "10.79.99.162")
	if changed, err := collector.Observe(second, net.ParseIP("10.79.99.162"), 7, f.now.Add(time.Second)); err != nil || len(changed) != 1 {
		t.Fatalf("second same-type service = %d %v", len(changed), err)
	}
	if got := catalog.Snapshot(f.now.Add(time.Second)); len(got) != 2 {
		t.Fatalf("PTRs collapsed: %v", got)
	}
	goodbye := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: []dns.RR{
		&dns.PTR{Hdr: dns.RR_Header{Name: "_http._tcp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 0}, Ptr: "Camera._http._tcp.local."},
	}}
	if changed, err := collector.Observe(goodbye, net.ParseIP("10.79.99.162"), 7, f.now.Add(2*time.Second)); err != nil || len(changed) != 1 {
		t.Fatalf("goodbye = %d %v", len(changed), err)
	}
	if got := catalog.Snapshot(f.now.Add(2 * time.Second)); len(got) != 1 || got[0].Instance != "Other" {
		t.Fatalf("goodbye removed wrong service: %+v", got)
	}
	if changed, err := collector.Sweep(f.now.Add(61 * time.Second)); err != nil || len(changed) != 1 {
		t.Fatalf("TTL expiry should sign one removal: %d %v", len(changed), err)
	}
	if len(catalog.Snapshot(f.now.Add(61*time.Second))) != 0 {
		t.Fatal("expired service still visible")
	}
}

func TestCollectorAnyValidTCPTypeAndLifecycleWithdrawal(t *testing.T) {
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, 535, "default", nil, nil)
	collector, err := NewCollector(AppScope{AppID: "com.wendy.test", AppIP: net.ParseIP("10.79.99.162"),
		BridgeIndex: 7, Ports: map[uint16]uint16{8080: 18080}}, catalog)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := collector.Observe(announcement("Camera", 60, 8080, "10.79.99.162"),
		net.ParseIP("10.79.99.162"), 7, f.now)
	if err != nil || len(changed) != 1 {
		t.Fatalf("unlisted valid type: %d %v", len(changed), err)
	}
	changed, err = collector.WithdrawAll(f.now.Add(time.Second))
	if err != nil || len(changed) != 1 {
		t.Fatalf("lifecycle withdrawal: %d %v", len(changed), err)
	}
	if len(catalog.Snapshot(f.now.Add(time.Second))) != 0 {
		t.Fatal("stopped app remains advertised")
	}
	if len(collector.published) != 0 || len(collector.records) != 0 {
		t.Fatal("collector retains stopped app state")
	}
}
