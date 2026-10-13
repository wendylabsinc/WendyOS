package meshcatalog

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func lanTestInterface() localmesh.PhysicalLANInterface {
	_, subnet, _ := net.ParseCIDR("192.168.41.0/24")
	return localmesh.PhysicalLANInterface{Name: "eth7", Index: 7, IP: net.ParseIP("192.168.41.1"), Net: subnet}
}

func TestLANCacheKeepsDirectEndpointWithoutMeshRoute(t *testing.T) {
	now := time.Now()
	iface := lanTestInterface()
	source := net.ParseIP("192.168.41.22")
	cache := NewLANCache()
	msg := announcement("Camera", 60, 8080, "192.168.41.22")
	cache.Observe(iface, source, msg, now)
	services := cache.Snapshot([]localmesh.PhysicalLANInterface{iface}, now.Add(time.Second))
	if len(services) != 1 || services[0].Port != 8080 || !services[0].Address.Equal(source) ||
		services[0].Records[1].(*dns.SRV).Target != "camera.local." {
		t.Fatalf("LAN endpoint changed: %+v", services)
	}
	// No Babel snapshot or signed catalog participates in this source.
	if got := cache.Snapshot(nil, now.Add(time.Second)); len(got) != 0 {
		t.Fatalf("disconnected interface projected %d LAN services", len(got))
	}
	goodbye := msg.Copy()
	for _, rr := range goodbye.Answer {
		rr.Header().Ttl = 0
	}
	cache.Observe(iface, source, goodbye, now.Add(2*time.Second))
	if got := cache.Snapshot([]localmesh.PhysicalLANInterface{iface}, now.Add(2*time.Second)); len(got) != 0 {
		t.Fatalf("LAN goodbye retained %d services", len(got))
	}
	cache.Observe(iface, source, msg, now.Add(3*time.Second))
	if got := cache.Snapshot([]localmesh.PhysicalLANInterface{iface}, now.Add(64*time.Second)); len(got) != 0 {
		t.Fatalf("LAN TTL expiry retained %d services", len(got))
	}
}

func TestLANCacheRejectsOffLinkAndForgedEndpoints(t *testing.T) {
	now := time.Now()
	iface := lanTestInterface()
	cache := NewLANCache()
	msg := announcement("Camera", 60, 8080, "192.168.41.22")
	cache.Observe(iface, net.ParseIP("192.168.42.22"), msg, now)
	cache.Observe(iface, net.ParseIP("192.168.41.23"), msg, now)
	if got := cache.Snapshot([]localmesh.PhysicalLANInterface{iface}, now); len(got) != 0 {
		t.Fatalf("off-link or forged endpoint admitted: %+v", got)
	}
	cache.Observe(iface, net.ParseIP("192.168.41.22"), msg, now)
	wrong := iface
	wrong.Index++
	if got := cache.Snapshot([]localmesh.PhysicalLANInterface{wrong}, now); len(got) != 0 {
		t.Fatalf("wrong interface admitted: %+v", got)
	}
	wendy := announcement("wendy", 60, 43023, "192.168.41.22")
	for _, rr := range append(wendy.Answer, wendy.Extra...) {
		rr.Header().Name = replaceServiceType(rr.Header().Name, "_http._tcp", "_wendy-mesh._tcp")
		if ptr, ok := rr.(*dns.PTR); ok {
			ptr.Ptr = replaceServiceType(ptr.Ptr, "_http._tcp", "_wendy-mesh._tcp")
		}
	}
	cache.Observe(iface, net.ParseIP("192.168.41.22"), wendy, now)
	if got := cache.Snapshot([]localmesh.PhysicalLANInterface{iface}, now); len(got) != 1 {
		t.Fatalf("carrier service leaked into app browse: %+v", got)
	}
}

func TestLANEndpointAllowsOrdinaryTenNetButRejectsMeshRanges(t *testing.T) {
	for _, check := range []struct {
		ip   string
		want bool
	}{
		{"10.42.1.7", true},
		{"10.88.1.7", false},
		{"10.99.1.7", false},
		{"127.0.0.1", false},
		{"224.0.0.251", false},
	} {
		if got := eligibleLANEndpoint(net.ParseIP(check.ip)); got != check.want {
			t.Errorf("eligibleLANEndpoint(%s)=%v, want %v", check.ip, got, check.want)
		}
	}
}

func TestLANBrowserRetainsServiceUntilLastPhysicalSourceEnds(t *testing.T) {
	now := time.Now()
	one := lanTestInterface()
	one.Cost = 64
	two := one
	two.Name, two.Index, two.IP, two.Cost = "wlan7", 8, net.ParseIP("192.168.41.2"), 128
	cache := NewLANCache()
	browser := &LANBrowser{cache: cache, current: []localmesh.PhysicalLANInterface{one, two}}
	source := net.ParseIP("192.168.41.22")
	msg := announcement("Camera", 60, 8080, "192.168.41.22")
	cache.Observe(one, source, msg, now)
	cache.Observe(two, source, msg, now)
	if got := browser.Snapshot(now); len(got) != 1 || got[0].Interface.Index != one.Index {
		t.Fatalf("duplicate interface projection = %+v", got)
	}
	goodbye := msg.Copy()
	for _, rr := range goodbye.Answer {
		rr.Header().Ttl = 0
	}
	cache.Observe(one, source, goodbye, now.Add(time.Second))
	if got := browser.Snapshot(now.Add(time.Second)); len(got) != 1 || got[0].Interface.Index != two.Index {
		t.Fatalf("last live LAN source lost: %+v", got)
	}
	cache.Observe(two, source, goodbye, now.Add(2*time.Second))
	if got := browser.Snapshot(now.Add(2 * time.Second)); len(got) != 0 {
		t.Fatalf("last source goodbye retained service: %+v", got)
	}
}

func replaceServiceType(value, old, new string) string {
	return strings.ReplaceAll(value, old, new)
}
