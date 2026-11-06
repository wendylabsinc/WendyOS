package meshcatalog

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/miekg/dns"
)

func TestProjectionSeparatesSameServiceOnDevicesWithSharedUUIDPrefix(t *testing.T) {
	now := time.Unix(1700000000, 0)
	first := Record{Version: 1, Key: Key{Mesh: "default", Device: p533, AppID: "com.wendy.test", ServiceID: "http"},
		Generation: 1, Type: "_http._tcp", Instance: strings.Repeat("Camera.", 9), HostPort: 18080,
		Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli()}
	second := first
	// A shared UUID prefix is not a shared owner. Identically configured apps
	// on these devices must still have independent DNS-SD instance RRsets.
	second.Key.Device = strings.Replace(p533, "000000000533", "000000000534", 1)
	vips := testVIPs(t)
	rrs, err := ProjectDNS([]Record{first, second}, "com.wendy.browser", func(string, Record) bool { return true }, vips, now)
	if err != nil {
		t.Fatal(err)
	}
	instances := map[string]string{}
	for _, rr := range rrs {
		if srv, ok := rr.(*dns.SRV); ok {
			name := strings.ToLower(srv.Hdr.Name)
			if target, exists := instances[name]; exists && target != srv.Target {
				t.Fatalf("distinct service owners collided at %s: %s and %s", name, target, srv.Target)
			}
			instances[name] = srv.Target
		}
	}
	if len(instances) != 2 {
		t.Fatalf("projected %d independent instances, want 2", len(instances))
	}
	for _, r := range []Record{first, second} {
		if len(projectedInstance(r)) > 63 {
			t.Fatal("owner suffix exceeded DNS label limit")
		}
	}
	// Exercise escaping and the actual announcement encoder, not only the
	// helper name. Both distinct SRV names must survive a DNS wire roundtrip.
	packets, err := packMDNSAnnouncements(rrs)
	if err != nil {
		t.Fatal(err)
	}
	wireInstances := map[string]bool{}
	for _, packet := range packets {
		var msg dns.Msg
		if err := msg.Unpack(packet); err != nil {
			t.Fatal(err)
		}
		for _, rr := range msg.Answer {
			if srv, ok := rr.(*dns.SRV); ok {
				wireInstances[strings.ToLower(srv.Hdr.Name)] = true
			}
		}
	}
	if len(wireInstances) != 2 {
		t.Fatal("DNS wire collapsed independent owners")
	}
	bridge := &MDNSBridge{projected: map[string]dns.RR{}}
	added, _ := bridge.projectionDelta(rrs)
	if len(added) != 8 {
		t.Fatalf("initial projection collapsed records: %d", len(added))
	}
	remaining, err := ProjectDNS([]Record{second}, "com.wendy.browser", func(string, Record) bool { return true }, vips, now)
	if err != nil {
		t.Fatal(err)
	}
	_, gone := bridge.projectionDelta(remaining)
	if len(gone) != 4 {
		t.Fatalf("removed owner withdrew %d records, want exactly 4", len(gone))
	}
	for _, rr := range gone {
		name := rr.Header().Name
		if ptr, ok := rr.(*dns.PTR); ok {
			name = ptr.Ptr
		}
		if strings.EqualFold(name, projectedInstanceName(second)) || strings.EqualFold(name, projectedHostName(second)) {
			t.Fatalf("owner removal withdrew surviving service's RRset: %s", rr)
		}
	}
}

func TestProjectedInstanceKeepsUTF8AndDNSLabelBounds(t *testing.T) {
	now := time.Unix(1700000000, 0)
	for _, name := range []string{"Camera " + strings.Repeat("🌡", 14), "Camera." + strings.Repeat("é", 28)} {
		r := Record{Version: 1, Key: Key{"default", p533, "com.wendy.test", "http"}, Generation: 1,
			Type: "_http._tcp", Instance: name, HostPort: 18080, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli()}
		label := projectedInstance(r)
		if !utf8.ValidString(label) || len(label) > 63 || !strings.HasPrefix(label, "Camera") {
			t.Fatalf("invalid or unreadable bounded label: %q", label)
		}
		rrs, err := ProjectDNS([]Record{r}, "com.wendy.browser", func(string, Record) bool { return true }, testVIPs(t), now)
		if err != nil {
			t.Fatal(err)
		}
		packets, err := packMDNSAnnouncements(rrs)
		if err != nil {
			t.Fatal(err)
		}
		for _, packet := range packets {
			var msg dns.Msg
			if err := msg.Unpack(packet); err != nil {
				t.Fatal(err)
			}
		}
		r.Instance = string([]byte{0xff})
		if err := r.Validate(now); err == nil {
			t.Fatal("invalid UTF-8 service name accepted")
		}
	}
}
