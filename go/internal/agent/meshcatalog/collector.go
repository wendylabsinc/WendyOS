package meshcatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// AppScope is supplied by the container lifecycle, never learned from mDNS.
// One isolated mesh app owns the bridge interface and container IP. Ports map
// a declared container port to the live authorized host ingress port.
type AppScope struct {
	AppID        string
	AppIP        net.IP
	BridgeIndex  int
	AllowedTypes []string
	Ports        map[uint16]uint16
	UDPPorts     map[uint16]uint16
}

// Bound raw observations independently of admitted services: an app can
// advertise arbitrary incomplete RRsets. Bytes count DNS wire data; the entry
// bound also limits map keys and Go object overhead.
const maxCollectorRecords = 4096
const maxCollectorBytes = 1 << 20

type observedRR struct {
	rr      dns.RR
	expires time.Time
}

type publication struct{ digest [32]byte }

// Collector accepts only DNS-SD advertisements arriving from its app's IP on
// its app bridge. It validates PTR/SRV/TXT/A ownership and declared host-port
// mappings before signing. Call Observe and Sweep from one event loop.
type Collector struct {
	scope       AppScope
	catalog     *Catalog
	records     map[string]observedRR
	recordBytes int
	published   map[string]publication
}

func NewCollector(scope AppScope, catalog *Catalog) (*Collector, error) {
	if appconfig.ValidateAppID(scope.AppID) != nil || scope.AppIP.To4() == nil ||
		scope.BridgeIndex <= 0 || catalog == nil {
		return nil, errors.New("invalid isolated app mDNS scope")
	}
	for _, typ := range scope.AllowedTypes {
		if !typePattern.MatchString(typ) {
			return nil, fmt.Errorf("invalid permitted service type %q", typ)
		}
	}
	for containerPort, hostPort := range scope.Ports {
		if containerPort == 0 || hostPort == 0 {
			return nil, errors.New("invalid app service port mapping")
		}
	}
	for containerPort, hostPort := range scope.UDPPorts {
		if containerPort == 0 || hostPort == 0 {
			return nil, errors.New("invalid app UDP service port mapping")
		}
	}
	scope.AppIP = append(net.IP(nil), scope.AppIP.To4()...)
	scope.AllowedTypes = append([]string(nil), scope.AllowedTypes...)
	for i := range scope.AllowedTypes {
		scope.AllowedTypes[i] = strings.ToLower(scope.AllowedTypes[i])
	}
	ports := make(map[uint16]uint16, len(scope.Ports))
	for p, h := range scope.Ports {
		ports[p] = h
	}
	scope.Ports = ports
	udpPorts := make(map[uint16]uint16, len(scope.UDPPorts))
	for p, h := range scope.UDPPorts {
		udpPorts[p] = h
	}
	scope.UDPPorts = udpPorts
	return &Collector{scope: scope, catalog: catalog, records: map[string]observedRR{},
		published: map[string]publication{}}, nil
}

func rrKey(rr dns.RR) string {
	h := rr.Header()
	key := strings.ToLower(h.Name) + "|" + fmt.Sprint(h.Rrtype)
	if ptr, ok := rr.(*dns.PTR); ok {
		key += "|" + strings.ToLower(ptr.Ptr)
	}
	return key
}

// Observe ignores traffic from any other IP or bridge. The source tuple is
// supplied by the socket's control message, not a field inside the DNS packet.
// Outgoing projected records are sourced from the agent gateway and are thus
// never re-exported as this app's own advertisements.
func (c *Collector) Observe(msg *dns.Msg, source net.IP, bridgeIndex int, now time.Time) ([]Record, error) {
	if msg == nil || !msg.Response || bridgeIndex != c.scope.BridgeIndex || !source.Equal(c.scope.AppIP) {
		return nil, nil
	}
	c.pruneRecords(now)
	for _, rr := range slices.Concat(msg.Answer, msg.Ns, msg.Extra) {
		if rr == nil || rr.Header() == nil {
			continue
		}
		h := rr.Header()
		if h.Class&^uint16(0x8000) != dns.ClassINET {
			continue
		}
		if h.Rrtype != dns.TypePTR && h.Rrtype != dns.TypeSRV && h.Rrtype != dns.TypeTXT && h.Rrtype != dns.TypeA {
			continue
		}
		key := rrKey(rr)
		if h.Ttl == 0 {
			c.removeRecord(key)
			continue
		}
		old, exists := c.records[key]
		size, previous := dns.Len(rr), 0
		if exists {
			previous = dns.Len(old.rr)
		}
		if (!exists && len(c.records) >= maxCollectorRecords) || c.recordBytes-previous+size > maxCollectorBytes {
			continue
		}
		ttl := time.Duration(h.Ttl) * time.Second
		if ttl > MaxLease {
			ttl = MaxLease
		}
		c.records[key] = observedRR{rr: dns.Copy(rr), expires: now.Add(ttl)}
		c.recordBytes += size - previous
	}
	return c.Sweep(now)
}

func (c *Collector) removeRecord(key string) {
	if old, exists := c.records[key]; exists {
		c.recordBytes -= dns.Len(old.rr)
		delete(c.records, key)
	}
}

func (c *Collector) pruneRecords(now time.Time) {
	for key, rr := range c.records {
		if !rr.expires.After(now) {
			c.removeRecord(key)
		}
	}
}

// Sweep updates the complete active service set when DNS content or liveness
// changes. DNS renewals never cause a signed publication.
func (c *Collector) Sweep(now time.Time) ([]Record, error) {
	c.pruneRecords(now)
	active := map[string]bool{}
	var changed []Record
	for _, item := range c.records {
		ptr, ok := item.rr.(*dns.PTR)
		if !ok {
			continue
		}
		typeName := strings.TrimSuffix(strings.ToLower(ptr.Hdr.Name), ".local.")
		if !typePattern.MatchString(typeName) ||
			(len(c.scope.AllowedTypes) != 0 && !slices.Contains(c.scope.AllowedTypes, typeName)) {
			continue
		}
		instanceName := ptr.Ptr
		if !strings.HasSuffix(strings.ToLower(instanceName), "."+typeName+".local.") {
			continue
		}
		instance := instanceName[:len(instanceName)-len(typeName)-len(".local.")-1]
		if instance == "" || len(instance) > 63 {
			continue
		}
		srvItem, srvOK := c.records[strings.ToLower(instanceName)+"|"+fmt.Sprint(dns.TypeSRV)]
		txtItem, txtOK := c.records[strings.ToLower(instanceName)+"|"+fmt.Sprint(dns.TypeTXT)]
		if !srvOK || !txtOK {
			continue
		}
		srv, ok := srvItem.rr.(*dns.SRV)
		if !ok {
			continue
		}
		txt, ok := txtItem.rr.(*dns.TXT)
		if !ok {
			continue
		}
		aItem, aOK := c.records[strings.ToLower(srv.Target)+"|"+fmt.Sprint(dns.TypeA)]
		if !aOK {
			continue
		}
		a, ok := aItem.rr.(*dns.A)
		if !ok || !a.A.Equal(c.scope.AppIP) {
			continue
		}
		portMap := c.scope.Ports
		if strings.HasSuffix(typeName, "._udp") {
			portMap = c.scope.UDPPorts
		}
		hostPort := portMap[srv.Port]
		if hostPort == 0 {
			continue
		}
		deadline := item.expires
		for _, d := range []time.Time{srvItem.expires, txtItem.expires, aItem.expires} {
			if d.Before(deadline) {
				deadline = d
			}
		}
		lease := deadline.Sub(now)
		if lease < time.Second {
			continue
		}
		if lease > MaxLease {
			lease = MaxLease
		}
		serviceID := observedServiceID(typeName, instanceName)
		active[serviceID] = true
		spec := PublishSpec{AppID: c.scope.AppID, ServiceID: serviceID,
			Type: typeName, Instance: instance, HostPort: hostPort,
			TXT: append([]string(nil), txt.Txt...), Lease: lease}
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%q", typeName, instance, hostPort, spec.TXT)))
		if old, found := c.published[serviceID]; found && old.digest == digest {
			continue
		}
		w, err := c.catalog.Publish(spec, now)
		if err != nil {
			return changed, err
		}
		c.published[serviceID] = publication{digest: digest}
		changed = append(changed, w)
	}
	for serviceID := range c.published {
		if active[serviceID] {
			continue
		}
		w, err := c.catalog.Remove(c.scope.AppID, serviceID, now)
		if err != nil {
			return changed, err
		}
		delete(c.published, serviceID)
		changed = append(changed, w)
	}
	return changed, nil
}

// WithdrawAll is called before an app bridge or ingress mapping is removed.
// It clears observation state so a later bridge instance starts cleanly.
func (c *Collector) WithdrawAll(now time.Time) ([]Record, error) {
	var changed []Record
	for serviceID := range c.published {
		w, err := c.catalog.Remove(c.scope.AppID, serviceID, now)
		if err != nil {
			return changed, err
		}
		changed = append(changed, w)
	}
	c.records = map[string]observedRR{}
	c.recordBytes = 0
	c.published = map[string]publication{}
	return changed, nil
}

func observedServiceID(typ, instance string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(typ) + "\x00" + strings.ToLower(instance)))
	return hex.EncodeToString(sum[:12])
}
