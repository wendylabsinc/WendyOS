package meshcatalog

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestLANProjectionSurvivesBabelRouteLossAndWithdrawsOnGoodbye(t *testing.T) {
	f := newFixture(t)
	local, identities := f.newCatalog(t, 533, "default", nil, nil)
	remote, _ := f.newCatalog(t, 535, "default", nil, nil)
	if _, err := identities.Put(f.creds[535].Certificate.Certificate, f.now); err != nil {
		t.Fatal(err)
	}
	w, err := remote.Publish(testSpec(), f.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.Accept(w, f.now); err != nil {
		t.Fatal(err)
	}
	var view localmesh.NodeSnapshot
	bridge, err := NewMDNSBridge(AppScope{AppID: "com.wendy.browse", AppIP: net.ParseIP("10.42.2.2"), BridgeIndex: 8},
		local, func(string, Record) bool { return true }, func(SignedRecord) {}, func() localmesh.NodeSnapshot { return view })
	if err != nil {
		t.Fatal(err)
	}
	iface := lanTestInterface()
	cache := NewLANCache()
	source := net.ParseIP("192.168.41.22")
	msg := announcement("Camera", 60, 8080, source.String())
	cache.Observe(iface, source, msg, f.now)
	access := NewLANProjector("/proc/self/ns/net", "10.42.2.2", "10.42.2.1", "wendy0")
	var granted, revoked int
	access.ensure = func(a hostnetwork.LANServiceAccess) error {
		granted++
		if a.Destination != source.String() || a.Port != 8080 || a.Protocol != "tcp" {
			t.Errorf("wrong LAN grant: %+v", a)
		}
		return nil
	}
	access.withdraw = func(hostnetwork.LANServiceAccess) error { revoked++; return nil }
	access.withdrawRoute = func(hostnetwork.LANServiceAccess) error { return nil }
	access.withdrawDrop = func(hostnetwork.LANServiceAccess) error { return nil }
	bridge.SetLANProjection(func(now time.Time) []LANService {
		return cache.Snapshot([]localmesh.PhysicalLANInterface{iface}, now)
	}, access)
	project := func(now time.Time, want int) (added, gone []dns.RR) {
		t.Helper()
		rr, err := bridge.projection(now)
		if err != nil || len(rr) != want {
			t.Fatalf("projection=%d, err=%v, want=%d", len(rr), err, want)
		}
		return bridge.projectionDelta(rr)
	}
	added, gone := project(f.now.Add(time.Second), 4)
	if len(added) != 4 || len(gone) != 0 || granted != 1 {
		t.Fatalf("LAN service without Babel: added=%d gone=%d grants=%d", len(added), len(gone), granted)
	}
	view = bridgeRouteView(t, f.now, 535)
	added, gone = project(f.now.Add(2*time.Second), 8)
	if len(added) != 4 || len(gone) != 0 || granted != 1 {
		t.Fatalf("mesh route up changed LAN grant: added=%d gone=%d grants=%d", len(added), len(gone), granted)
	}
	view.Routes[0].Unreachable = true
	added, gone = project(f.now.Add(3*time.Second), 4)
	if len(added) != 0 || len(gone) != 4 || revoked != 0 {
		t.Fatalf("mesh route loss withdrew LAN: added=%d gone=%d LAN revokes=%d", len(added), len(gone), revoked)
	}
	goodbye := msg.Copy()
	for _, rr := range goodbye.Answer {
		rr.Header().Ttl = 0
	}
	cache.Observe(iface, source, goodbye, f.now.Add(4*time.Second))
	added, gone = project(f.now.Add(4*time.Second), 0)
	if len(added) != 0 || len(gone) != 4 || revoked != 1 {
		t.Fatalf("LAN goodbye did not revoke: added=%d gone=%d revokes=%d", len(added), len(gone), revoked)
	}
	for _, rr := range gone {
		if rr.Header().Ttl != 0 {
			t.Fatalf("LAN goodbye has positive TTL: %v", rr)
		}
	}
	if got := local.Snapshot(f.now.Add(4 * time.Second)); len(got) != 1 || got[0].Generation != 1 {
		t.Fatalf("LAN source modified signed catalog: %+v", got)
	}
}

func TestLANProjectionCoexistsWithNativePublisherOfSameServiceType(t *testing.T) {
	f := newFixture(t)
	catalog, _ := f.newCatalog(t, 533, "default", nil, nil)
	appIP := net.ParseIP("10.42.2.2")
	bridge, err := NewMDNSBridge(AppScope{AppID: "com.wendy.browse", AppIP: appIP, BridgeIndex: 8},
		catalog, func(string, Record) bool { return true }, func(SignedRecord) {}, func() localmesh.NodeSnapshot { return localmesh.NodeSnapshot{} })
	if err != nil {
		t.Fatal(err)
	}
	// A normal app responder enumerates its service type and claims its own
	// instance. The shared type must not hide another LAN instance.
	enumeration := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true}, Answer: []dns.RR{
		&dns.PTR{Hdr: dns.RR_Header{Name: "_services._dns-sd._udp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 60}, Ptr: "_http._tcp.local."},
	}}
	bridge.observeAppClaims(enumeration, appIP, 8, f.now)
	bridge.observeAppClaims(announcement("Native", 60, 8080, appIP.String()), appIP, 8, f.now)
	if bridge.appClaimsName("_http._tcp.local.") {
		t.Fatal("shared DNS-SD service type treated as app-owned")
	}
	if !bridge.appClaimsName("Native._http._tcp.local.") {
		t.Fatal("native instance claim was lost")
	}
	iface := lanTestInterface()
	source := net.ParseIP("192.168.41.22")
	cache := NewLANCache()
	cache.Observe(iface, source, announcement("Camera", 60, 8081, source.String()), f.now)
	cache.Observe(iface, source, announcement("Native", 60, 8082, source.String()), f.now)
	access := NewLANProjector("/proc/self/ns/net", appIP.String(), "10.42.2.1", "wendy0")
	var grants []uint16
	access.ensure = func(a hostnetwork.LANServiceAccess) error { grants = append(grants, a.Port); return nil }
	access.withdrawRoute = func(hostnetwork.LANServiceAccess) error { return nil }
	access.withdrawDrop = func(hostnetwork.LANServiceAccess) error { return nil }
	bridge.SetLANProjection(func(now time.Time) []LANService {
		return cache.Snapshot([]localmesh.PhysicalLANInterface{iface}, now)
	}, access)
	records, err := bridge.projection(f.now.Add(time.Second))
	if err != nil || len(records) != 4 || len(grants) != 1 || grants[0] != 8081 {
		t.Fatalf("same-type LAN instance suppressed or native collision granted: records=%v grants=%v err=%v", records, grants, err)
	}
}

func TestLANProjectorBlocksWholeDestinationAfterPartialGrantFailure(t *testing.T) {
	access := NewLANProjector("/proc/self/ns/net", "10.42.2.2", "10.42.2.1", "wendy0")
	failSecond := true
	var routesRemoved int
	access.ensure = func(a hostnetwork.LANServiceAccess) error {
		if a.Port == 8081 && failSecond {
			return fmt.Errorf("transient NAT failure")
		}
		return nil
	}
	access.withdraw = func(hostnetwork.LANServiceAccess) error { return nil }
	access.withdrawRoute = func(hostnetwork.LANServiceAccess) error { routesRemoved++; return nil }
	access.withdrawDrop = func(hostnetwork.LANServiceAccess) error { return nil }
	base := LANService{Interface: lanTestInterface(), Address: net.ParseIP("192.168.41.22"), Protocol: "tcp"}
	one, two := base, base
	one.Port = 8080
	one.Records = []dns.RR{&dns.PTR{Hdr: dns.RR_Header{Name: "_http._tcp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 60}, Ptr: "One._http._tcp.local."}}
	two.Port = 8081
	two.Records = []dns.RR{&dns.PTR{Hdr: dns.RR_Header{Name: "_http._tcp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 60}, Ptr: "Two._http._tcp.local."}}
	if projected := access.Sync([]LANService{one, two}); len(projected) != 0 || routesRemoved == 0 {
		t.Fatalf("partial grant exposed destination: records=%v route removals=%d", projected, routesRemoved)
	}
	failSecond = false
	if projected := access.Sync([]LANService{one, two}); len(projected) != 2 {
		t.Fatalf("retry did not restore both grants: %v", projected)
	}
}

func TestLANProjectorReportsDistinctGrantFailureAndRecovery(t *testing.T) {
	p := NewLANProjector("/proc/self/ns/net", "10.42.2.2", "10.42.2.1", "wendy0")
	service := LANService{Interface: lanTestInterface(), Address: net.ParseIP("192.168.41.22"),
		Port: 8080, Protocol: "tcp", Records: []dns.RR{&dns.PTR{Hdr: dns.RR_Header{Name: "_http._tcp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 60}, Ptr: "Camera._http._tcp.local."}}}
	var failures []error
	p.SetErrorHandler(func(err error) { failures = append(failures, err) })
	p.ensure = func(hostnetwork.LANServiceAccess) error { return fmt.Errorf("namespace path vanished") }
	p.withdrawRoute = func(hostnetwork.LANServiceAccess) error { return nil }
	p.withdrawDrop = func(hostnetwork.LANServiceAccess) error { return nil }
	for range 3 {
		if got := p.Sync([]LANService{service}); len(got) != 0 {
			t.Fatalf("failed grant projected records: %v", got)
		}
	}
	if len(failures) != 1 {
		t.Fatalf("repeated failure logged %d times, want once", len(failures))
	}
	p.ensure = func(hostnetwork.LANServiceAccess) error { return nil }
	if got := p.Sync([]LANService{service}); len(got) != 1 {
		t.Fatalf("grant recovery did not project: %v", got)
	}
	p.ensure = func(hostnetwork.LANServiceAccess) error { return fmt.Errorf("namespace path vanished") }
	p.visible[lanAccessKey(p.access(service))] = false
	_ = p.Sync([]LANService{service})
	if len(failures) != 2 {
		t.Fatalf("new failure after recovery not logged: %d", len(failures))
	}
}
