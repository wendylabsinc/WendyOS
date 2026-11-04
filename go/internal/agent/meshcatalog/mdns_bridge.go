package meshcatalog

import (
	"errors"
	"net"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// MDNSBridge serves one isolated app bridge. The container lifecycle supplies
// the app identity, bridge index, container IP and declared port mapping.
// emit observes local intent changes already submitted to the complete manifest.
type MDNSBridge struct {
	vips      *mesh.VIPTable
	scope     AppScope
	collector *Collector
	catalog   *Catalog
	policy    BrowsePolicy
	snapshot  func() localmesh.NodeSnapshot
	lanSource func(time.Time) []LANService
	lanAccess *LANProjector
	emit      func(Record)
	projected map[string]dns.RR
	seenTypes map[string]time.Time
	// App-owned mDNS names take precedence on this app's bridge. Claims are
	// learned only from its container IP/interface and expire with their TTL.
	appClaims          map[string]map[uint16]time.Time
	claimsOverflowTill time.Time
}

func NewMDNSBridge(scope AppScope, catalog *Catalog, vips *mesh.VIPTable, policy BrowsePolicy, emit func(Record), snapshot func() localmesh.NodeSnapshot) (*MDNSBridge, error) {
	if policy == nil || emit == nil || vips == nil || snapshot == nil {
		return nil, errors.New("missing scoped mDNS policy or relay")
	}
	collector, err := NewCollector(scope, catalog)
	if err != nil {
		return nil, err
	}
	return &MDNSBridge{scope: collector.scope, collector: collector, catalog: catalog, vips: vips, snapshot: snapshot,
		policy: policy, emit: emit, projected: map[string]dns.RR{}, seenTypes: map[string]time.Time{},
		appClaims: map[string]map[uint16]time.Time{}}, nil
}

// SetLANProjection adds unsigned, directly reachable physical-LAN services
// to this app's multicast bridge. They never enter the signed device manifest.
func (b *MDNSBridge) SetLANProjection(source func(time.Time) []LANService, access *LANProjector) {
	b.lanSource, b.lanAccess = source, access
}

const maxDiscoveredTypes = 32
const maxAppClaimNames = 256

// observeAppClaims also accepts probe authority records, so a local responder
// can claim its name before announcing it. Probe claims get a short lease;
// published claims use their advertised TTL and TTL-zero goodbyes release them.
func (b *MDNSBridge) observeAppClaims(msg *dns.Msg, source net.IP, ifindex int, now time.Time) {
	if msg == nil || ifindex != b.scope.BridgeIndex || !source.Equal(b.scope.AppIP) {
		return
	}
	b.pruneAppClaims(now)
	var records []dns.RR
	if msg.Response {
		records = append(records, msg.Answer...)
		records = append(records, msg.Ns...)
		records = append(records, msg.Extra...)
	} else {
		records = msg.Ns // proposed unique records in an mDNS probe
	}
	for _, rr := range records {
		if rr == nil || rr.Header() == nil || rr.Header().Class&0x7fff != dns.ClassINET {
			continue
		}
		var name string
		switch record := rr.(type) {
		case *dns.SRV, *dns.TXT, *dns.A:
			name = rr.Header().Name
		case *dns.PTR:
			// A PTR also claims its target instance before some publishers
			// transmit the corresponding SRV and TXT records. Service
			// enumeration targets are shared types, not owned instances.
			if strings.EqualFold(record.Hdr.Name, "_services._dns-sd._udp.local.") {
				continue
			}
			name = record.Ptr
		default:
			continue
		}
		if !strings.HasSuffix(strings.ToLower(name), ".local.") {
			continue
		}
		name = strings.ToLower(name)
		typeID := rr.Header().Rrtype
		if msg.Response && rr.Header().Ttl == 0 {
			if types := b.appClaims[name]; types != nil {
				delete(types, typeID)
				if len(types) == 0 {
					delete(b.appClaims, name)
				}
			}
			continue
		}
		lease := time.Duration(rr.Header().Ttl) * time.Second
		if !msg.Response {
			lease = 2 * time.Second
		}
		if lease <= 0 {
			continue
		}
		if b.appClaims[name] == nil {
			if len(b.appClaims) >= maxAppClaimNames {
				// An app can publish arbitrarily many names. If tracking fills,
				// hide all projections while the untracked claim is still live.
				if until := now.Add(lease); until.After(b.claimsOverflowTill) {
					b.claimsOverflowTill = until
				}
				continue
			}
			b.appClaims[name] = map[uint16]time.Time{}
		}
		b.appClaims[name][typeID] = now.Add(lease)
	}
}

func (b *MDNSBridge) pruneAppClaims(now time.Time) {
	for name, types := range b.appClaims {
		for typ, until := range types {
			if !until.After(now) {
				delete(types, typ)
			}
		}
		if len(types) == 0 {
			delete(b.appClaims, name)
		}
	}
}

func (b *MDNSBridge) appClaimsName(name string) bool {
	return len(b.appClaims[strings.ToLower(name)]) != 0
}

// A goodbye for an RRset now owned by the native app would evict that app's
// record too. Its own cache-flush announcement replaces the projected data;
// only unclaimed projected RRsets need an agent-generated goodbye.
func (b *MDNSBridge) unclaimedGoodbyes(gone []dns.RR, now time.Time) []dns.RR {
	b.pruneAppClaims(now)
	out := make([]dns.RR, 0, len(gone))
	for _, rr := range gone {
		if rr == nil {
			continue
		}
		name := rr.Header().Name
		if ptr, ok := rr.(*dns.PTR); ok {
			name = ptr.Ptr
		}
		if !b.appClaims[strings.ToLower(name)][rr.Header().Rrtype].After(now) {
			out = append(out, rr)
		}
	}
	return out
}

// learnTypes consumes only enumeration replies already checked against the
// app IP and bridge index by Run. The app cannot use a reflected remote
// projection to claim a type because agent-origin packets fail that check.
func (b *MDNSBridge) learnTypes(msg *dns.Msg, now time.Time) []string {
	var fresh []string
	for _, rr := range append(append(append([]dns.RR(nil), msg.Answer...), msg.Ns...), msg.Extra...) {
		ptr, ok := rr.(*dns.PTR)
		if !ok || !strings.EqualFold(ptr.Hdr.Name, "_services._dns-sd._udp.local.") {
			continue
		}
		typ := strings.TrimSuffix(strings.ToLower(ptr.Ptr), ".local.")
		if !typePattern.MatchString(typ) {
			continue
		}
		if ptr.Hdr.Ttl == 0 {
			delete(b.seenTypes, typ)
			continue
		}
		if _, exists := b.seenTypes[typ]; !exists {
			if len(b.seenTypes) >= maxDiscoveredTypes {
				continue
			}
			fresh = append(fresh, typ)
		}
		lease := time.Duration(ptr.Hdr.Ttl) * time.Second
		if lease > MaxLease {
			lease = MaxLease
		}
		b.seenTypes[typ] = now.Add(lease)
	}
	return fresh
}

func (b *MDNSBridge) knownTypes(now time.Time) []string {
	var names []string
	for typ, until := range b.seenTypes {
		if !until.After(now) {
			delete(b.seenTypes, typ)
			continue
		}
		names = append(names, typ)
	}
	slices.Sort(names)
	return names
}

func (b *MDNSBridge) observe(msg *dns.Msg, source net.IP, ifindex int, now time.Time) error {
	b.observeAppClaims(msg, source, ifindex, now)
	changed, err := b.collector.Observe(msg, source, ifindex, now)
	if err != nil {
		return err
	}
	for _, w := range changed {
		b.emit(w)
	}
	return nil
}

func (b *MDNSBridge) sweep(now time.Time) error {
	changed, err := b.collector.Sweep(now)
	if err != nil {
		return err
	}
	for _, w := range changed {
		b.emit(w)
	}
	return nil
}

func (b *MDNSBridge) withdraw(now time.Time) error {
	changed, err := b.collector.WithdrawAll(now)
	if err != nil {
		return err
	}
	for _, w := range changed {
		b.emit(w)
	}
	return nil
}

func (b *MDNSBridge) projection(now time.Time) ([]dns.RR, error) {
	b.pruneAppClaims(now)
	// A catalog record can outlive its origin's route. Keep its signed lease
	// and high-water receipt, but withdraw its DNS projection until the
	// origin's signed manifest and exact host route are both live again.
	view := b.snapshot()
	reachable := make(map[meshid.Device]bool)
	signed, err := ProjectDNS(b.catalog.Snapshot(now), b.scope.AppID, func(appID string, r Record) bool {
		device, err := meshid.DeviceFromPrincipal(r.Key.Device)
		if err != nil {
			return false
		}
		if device != b.catalog.device {
			ok, checked := reachable[device]
			if !checked {
				ok = eligibleSnapshot(view, device, now)
				reachable[device] = ok
			}
			if !ok {
				return false
			}
		}
		return b.policy(appID, r) && !b.claimsOverflowTill.After(now) &&
			!b.appClaimsName(projectedInstanceName(r)) && !b.appClaimsName(projectedHostName(r))
	}, b.vips, now)
	if err != nil || b.lanSource == nil || b.lanAccess == nil {
		return signed, err
	}
	// Native app publishers and signed mesh projections take precedence over
	// an unsigned LAN name. Suppressed names never receive LAN egress grants.
	owned := make(map[string]bool, len(signed))
	for _, rr := range signed {
		if rr.Header().Rrtype != dns.TypePTR {
			owned[strings.ToLower(rr.Header().Name)] = true
		}
	}
	services := b.lanSource(now)
	if b.claimsOverflowTill.After(now) {
		// Names omitted by the bounded native claim cache may collide too.
		// Withdraw unsigned grants until every untracked claim has expired.
		services = nil
	}
	allowed := make([]LANService, 0, len(services))
	for _, service := range services {
		conflict := false
		for _, rr := range service.Records {
			name := rr.Header().Name
			if ptr, ok := rr.(*dns.PTR); ok {
				// The PTR owner is a shared service type. Only its target
				// instance can conflict with a native publisher or a signed
				// mesh projection on this app bridge.
				name = ptr.Ptr
			}
			if b.appClaimsName(name) || owned[strings.ToLower(name)] {
				conflict = true
				break
			}
		}
		if !conflict {
			allowed = append(allowed, service)
		}
	}
	return append(signed, b.lanAccess.Sync(allowed)...), nil
}

func projectionKey(rr dns.RR) string {
	copy := dns.Copy(rr)
	copy.Header().Ttl = 0
	return copy.String()
}

// projectionDelta returns newly visible records and records which disappeared
// or changed since the last sweep. TTL changes alone do not count as changes;
// Run refreshes unchanged records separately before their cached TTL expires.
func (b *MDNSBridge) projectionDelta(current []dns.RR) (added, gone []dns.RR) {
	next := make(map[string]dns.RR, len(current))
	for _, rr := range current {
		key := projectionKey(rr)
		next[key] = dns.Copy(rr)
		if _, existed := b.projected[key]; !existed {
			added = append(added, dns.Copy(rr))
		}
	}
	for key, old := range b.projected {
		if _, ok := next[key]; !ok {
			copy := dns.Copy(old)
			copy.Header().Ttl = 0
			gone = append(gone, copy)
		}
	}
	b.projected = next
	return added, gone
}

// projectionGoodbyes is retained for callers that only need withdrawals.
func (b *MDNSBridge) projectionGoodbyes(current []dns.RR) []dns.RR {
	_, gone := b.projectionDelta(current)
	return gone
}

// announcementRefreshInterval sends a cache refresh well before the shortest
// projected TTL expires, while capping steady-state multicast traffic.
func announcementRefreshInterval(records []dns.RR) time.Duration {
	interval := 30 * time.Second
	for _, rr := range records {
		if half := time.Duration(rr.Header().Ttl) * time.Second / 2; half < interval {
			interval = half
		}
	}
	if interval < time.Second {
		return time.Second
	}
	return interval
}
