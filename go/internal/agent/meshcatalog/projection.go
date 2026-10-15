package meshcatalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// BrowsePolicy is evaluated separately for each requesting app and record.
// Returning false hides the record from that app's mDNS projection. The
// publisher's service type authorization is a separate admission check.
type BrowsePolicy func(requesterAppID string, record Record) bool

// ProjectDNS renders a complete DNS-SD set for one isolated app bridge. It
// never copies a source container IP: SRV points to the origin's Wendy VIP,
// whose port must already be authorized by the destination agent. Every DNS
// TTL is bounded by the remaining signed lease. A projected record is never
// converted into a new local publication.
func ProjectDNS(records []Record, requesterAppID string, policy BrowsePolicy, now time.Time) ([]dns.RR, error) {
	if appconfig.ValidateAppID(requesterAppID) != nil || policy == nil {
		return nil, errors.New("invalid DNS-SD projection scope")
	}
	out := make([]dns.RR, 0, len(records)*4)
	hostAddresses := make(map[string]*dns.A)
	for _, r := range records {
		if r.Withdraw || !policy(requesterAppID, r) {
			continue
		}
		if err := r.Validate(now); err != nil {
			return nil, err
		}
		// Use the caller's clock, not wall clock, so a clock-snapshot test and
		// expiry check agree. A subsecond lease cannot support a positive TTL.
		remaining := time.UnixMilli(r.Expires).Sub(now)
		if remaining < time.Second {
			continue
		}
		ttl := uint32(remaining / time.Second)
		if ttl > 120 {
			ttl = 120
		}
		vip, err := mesh.VIPForDevice(r.Key.Asset)
		if err != nil {
			return nil, err
		}
		serviceName := r.Type + ".local."
		instanceName := projectedInstanceName(r)
		hostName := projectedHostName(r)
		header := func(name string, rrtype uint16) dns.RR_Header {
			return dns.RR_Header{Name: name, Rrtype: rrtype, Class: dns.ClassINET, Ttl: ttl}
		}
		out = append(out,
			&dns.PTR{Hdr: header(serviceName, dns.TypePTR), Ptr: instanceName},
			&dns.SRV{Hdr: header(instanceName, dns.TypeSRV), Priority: 0, Weight: 0,
				Port: r.HostPort, Target: hostName},
			&dns.TXT{Hdr: header(instanceName, dns.TypeTXT), Txt: append([]string(nil), r.TXT...)},
		)
		// Several services on one origin share one VIP. Emit a single address
		// record with the longest still-valid lease, so removing one service
		// cannot withdraw the address needed by its siblings.
		if address := hostAddresses[hostName]; address != nil {
			if ttl > address.Hdr.Ttl {
				address.Hdr.Ttl = ttl
			}
		} else {
			address := &dns.A{Hdr: header(hostName, dns.TypeA), A: net.IP(vip.AsSlice())}
			hostAddresses[hostName] = address
			out = append(out, address)
		}
	}
	return out, nil
}

func projectedInstanceName(r Record) string {
	return escapeDNSLabel(projectedInstance(r)) + "." + r.Type + ".local."
}

func projectedHostName(r Record) string {
	return fmt.Sprintf("device-%d.mesh.local.", r.Key.Asset)
}

func projectedInstance(r Record) string {
	h := sha256.Sum256([]byte(r.Key.AppID + "\x00" + r.Key.ServiceID))
	suffix := fmt.Sprintf("-%d-%s", r.Key.Asset, hex.EncodeToString(h[:3]))
	name := strings.TrimSpace(r.Instance)
	if len(name) > 63-len(suffix) {
		name = name[:63-len(suffix)]
	}
	return name + suffix
}

func escapeDNSLabel(label string) string {
	var b strings.Builder
	for _, c := range []byte(label) {
		switch c {
		case '.', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
