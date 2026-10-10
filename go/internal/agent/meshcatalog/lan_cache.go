package meshcatalog

import (
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

const maxLANRecords = 1024

// LANService is an unsigned, link-local DNS-SD observation. It is never
// admitted to the signed mesh catalog or retransmitted to another node.
type LANService struct {
	Interface localmesh.PhysicalLANInterface
	Source    net.IP
	Address   net.IP
	Port      uint16
	Protocol  string
	Records   []dns.RR
	Expires   time.Time
}

type lanRecordKey struct {
	ifindex int
	source  string
	rr      string
}

type lanRecord struct {
	rr      dns.RR
	expires time.Time
}

// LANCache holds only complete services heard on enabled physical links.
// Its bounded leases and per-interface source keys prevent a stale LAN
// announcement from masquerading as a signed mesh publication.
type LANCache struct {
	mu      sync.Mutex
	records map[lanRecordKey]lanRecord
}

func NewLANCache() *LANCache { return &LANCache{records: make(map[lanRecordKey]lanRecord)} }

func eligibleLANEndpoint(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
		return false
	}
	// Static mesh underlay and app VIP ranges are never physical-LAN
	// endpoints. Dynamic CNI overlap is checked against the host's actual
	// egress route before the service is granted to an app.
	return !(v4[0] == 10 && (v4[1] == 88 || v4[1] == 99))
}

func (c *LANCache) Observe(iface localmesh.PhysicalLANInterface, source net.IP, msg *dns.Msg, now time.Time) {
	if c == nil || msg == nil || !msg.Response || iface.Index <= 0 || iface.Net == nil ||
		!iface.Net.Contains(source) || !eligibleLANEndpoint(source) || source.Equal(iface.IP) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	for _, rr := range slices.Concat(msg.Answer, msg.Ns, msg.Extra) {
		if rr == nil || rr.Header() == nil || rr.Header().Class&0x7fff != dns.ClassINET {
			continue
		}
		switch rr.(type) {
		case *dns.PTR, *dns.SRV, *dns.TXT, *dns.A:
		default:
			continue
		}
		if !strings.HasSuffix(strings.ToLower(rr.Header().Name), ".local.") {
			continue
		}
		key := lanRecordKey{ifindex: iface.Index, source: source.String(), rr: rrKey(rr)}
		if rr.Header().Ttl == 0 {
			delete(c.records, key)
			continue
		}
		if len(c.records) >= maxLANRecords {
			if _, exists := c.records[key]; !exists {
				continue
			}
		}
		lease := time.Duration(rr.Header().Ttl) * time.Second
		if lease > MaxLease {
			lease = MaxLease
		}
		c.records[key] = lanRecord{rr: dns.Copy(rr), expires: now.Add(lease)}
	}
}

func (c *LANCache) prune(now time.Time) {
	for key, item := range c.records {
		if !item.expires.After(now) {
			delete(c.records, key)
		}
	}
}

func (c *LANCache) Snapshot(interfaces []localmesh.PhysicalLANInterface, now time.Time) []LANService {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	available := make(map[int]localmesh.PhysicalLANInterface, len(interfaces))
	for _, iface := range interfaces {
		available[iface.Index] = iface
	}
	var result []LANService
	for key, item := range c.records {
		ptr, ok := item.rr.(*dns.PTR)
		if !ok || ptr.Hdr.Rrtype != dns.TypePTR {
			continue
		}
		iface, present := available[key.ifindex]
		if !present || iface.Net == nil || !iface.Net.Contains(net.ParseIP(key.source)) {
			continue
		}
		typeName := strings.TrimSuffix(strings.ToLower(ptr.Hdr.Name), ".local.")
		if !typePattern.MatchString(typeName) || typeName == "_wendy-mesh._tcp" ||
			!strings.HasSuffix(strings.ToLower(ptr.Ptr), "."+typeName+".local.") {
			continue
		}
		suffix := lanRecordKey{ifindex: key.ifindex, source: key.source}
		suffix.rr = strings.ToLower(ptr.Ptr) + "|" + fmt.Sprint(dns.TypeSRV)
		srvItem, ok := c.records[suffix]
		if !ok {
			continue
		}
		srv, ok := srvItem.rr.(*dns.SRV)
		if !ok || srv.Port == 0 || !strings.HasSuffix(strings.ToLower(srv.Target), ".local.") {
			continue
		}
		suffix.rr = strings.ToLower(ptr.Ptr) + "|" + fmt.Sprint(dns.TypeTXT)
		txtItem, ok := c.records[suffix]
		if !ok {
			continue
		}
		suffix.rr = strings.ToLower(srv.Target) + "|" + fmt.Sprint(dns.TypeA)
		aItem, ok := c.records[suffix]
		if !ok {
			continue
		}
		a, ok := aItem.rr.(*dns.A)
		if !ok || !eligibleLANEndpoint(a.A) || !a.A.Equal(net.ParseIP(key.source)) || !iface.Net.Contains(a.A) {
			continue
		}
		until := item.expires
		for _, entry := range []lanRecord{srvItem, txtItem, aItem} {
			if entry.expires.Before(until) {
				until = entry.expires
			}
		}
		if !until.After(now.Add(time.Second)) {
			continue
		}
		lease := uint32(until.Sub(now) / time.Second)
		records := []dns.RR{dns.Copy(item.rr), dns.Copy(srvItem.rr), dns.Copy(txtItem.rr), dns.Copy(aItem.rr)}
		for _, rr := range records {
			rr.Header().Ttl = lease
		}
		protocol := "tcp"
		if strings.HasSuffix(typeName, "._udp") {
			protocol = "udp"
		}
		result = append(result, LANService{Interface: iface, Source: net.ParseIP(key.source),
			Address: append(net.IP(nil), a.A...), Port: srv.Port, Protocol: protocol, Records: records, Expires: until})
	}
	return result
}

// Types bounds follow-up PTR queries to types actually seen on this link.
func (c *LANCache) Types(iface localmesh.PhysicalLANInterface, now time.Time) []string {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	types := make(map[string]struct{})
	for key, item := range c.records {
		if key.ifindex != iface.Index {
			continue
		}
		ptr, ok := item.rr.(*dns.PTR)
		if !ok {
			continue
		}
		name := strings.ToLower(ptr.Hdr.Name)
		if name == "_services._dns-sd._udp.local." {
			typ := strings.TrimSuffix(strings.ToLower(ptr.Ptr), ".local.")
			if typePattern.MatchString(typ) && typ != "_wendy-mesh._tcp" {
				types[typ] = struct{}{}
			}
		} else {
			typ := strings.TrimSuffix(name, ".local.")
			if typePattern.MatchString(typ) && typ != "_wendy-mesh._tcp" {
				types[typ] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(types))
	for typ := range types {
		result = append(result, typ)
	}
	slices.Sort(result)
	if len(result) > 32 {
		result = result[:32]
	}
	return result
}
