package vpn

import (
	"net"
	"net/url"
	"strings"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshname"
)

func AnswerDNS(wire []byte, routes []Route) ([]byte, error) {
	var query dns.Msg
	if err := query.Unpack(wire); err != nil {
		return nil, err
	}
	reply := new(dns.Msg)
	reply.SetReply(&query)
	reply.Authoritative = true
	if query.Response || query.Opcode != dns.OpcodeQuery || len(query.Question) != 1 {
		reply.Rcode = dns.RcodeFormatError
		return reply.Pack()
	}
	q := query.Question[0]
	name := strings.ToLower(dns.Fqdn(q.Name))
	if q.Qclass != dns.ClassINET {
		reply.Rcode = dns.RcodeRefused
		return reply.Pack()
	}
	if name == meshname.Suffix+"." {
		reply.Rcode = dns.RcodeNameError
		return reply.Pack()
	}
	label, suffix, ok := deviceDNSLabel(name)
	if !ok {
		reply.Rcode = dns.RcodeRefused
		return reply.Pack()
	}
	for _, c := range q.Name {
		if c > 127 {
			reply.Rcode = dns.RcodeNameError
			return reply.Pack()
		}
	}
	identity := strings.HasPrefix(label, "device-") && !strings.Contains(label, ".")
	_, uuidErr := meshid.ParseDeviceUUID(strings.TrimPrefix(label, "device-"))
	uuidIdentity := identity && uuidErr == nil
	if !identity && suffix != meshname.Suffix {
		reply.Rcode = dns.RcodeRefused
		reply.Authoritative = false
		return reply.Pack()
	}
	// .cloud compatibility names historically address asset records. An exact
	// legacy host wins there; its asset UUID must not become a private identity.
	if suffix == meshname.LegacySuffix {
		for i := range routes {
			if name == strings.ToLower(dns.Fqdn(routes[i].Host)) {
				return answerRoute(reply, q, &routes[i])
			}
		}
	}
	var found *Route
	foundIdentity := ""
	for i := range routes {
		route := &routes[i]
		peer, verified, err := routeDevice(*route)
		if err != nil {
			continue
		}
		matches := false
		if suffix != meshname.Suffix && name == strings.ToLower(dns.Fqdn(route.Host)) {
			matches = true
		} else if uuidIdentity {
			matches = verified && peer.UUID().String() == strings.TrimPrefix(label, "device-")
		} else if suffix == meshname.Suffix {
			host, valid := meshname.Friendly(route.Name)
			matches = verified && valid && name == dns.Fqdn(host) && singleTenant(routes)
		} else {
			matches = name == strings.ToLower(dns.Fqdn(route.Host))
		}
		if !matches {
			continue
		}
		key := route.Selector
		if verified {
			key = peer.Principal()
		}
		if found != nil && key != foundIdentity {
			reply.Rcode = dns.RcodeNameError
			return reply.Pack()
		}
		// One authenticated device may have both mesh and Cloud paths. Prefer the
		// mesh path without depending on discovery/list ordering.
		if found == nil || preferDNSRoute(*route, *found) {
			found = route
			foundIdentity = key
		}
	}
	if found == nil {
		reply.Rcode = dns.RcodeNameError
		return reply.Pack()
	}
	return answerRoute(reply, q, found)
}

func answerRoute(reply *dns.Msg, q dns.Question, route *Route) ([]byte, error) {
	if q.Qtype == dns.TypeA {
		reply.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP(route.Address)}}
	}
	return reply.Pack()
}
func preferDNSRoute(candidate, current Route) bool {
	a, b := strings.HasPrefix(candidate.Selector, "mesh:"), strings.HasPrefix(current.Selector, "mesh:")
	if a != b {
		return a
	}
	return candidate.Selector < current.Selector
}
func deviceDNSLabel(name string) (string, string, bool) {
	for _, suffix := range []string{meshname.PreviousSuffix, "mesh.wendy.dev", meshname.LegacySuffix, meshname.Suffix} {
		if tail := "." + suffix + "."; strings.HasSuffix(name, tail) {
			return strings.TrimSuffix(name, tail), suffix, true
		}
	}
	return "", "", false
}

// Friendly aliases have meaning only within one explicitly selected tenant.
func singleTenant(routes []Route) bool {
	tenant := ""
	for _, route := range routes {
		u, err := url.Parse(route.Selector)
		if err != nil {
			return false
		}
		current := ""
		if u.Scheme == "mesh" && u.Host != "" {
			current = u.Host
		}
		if u.Scheme == "cloud" {
			parts := strings.Split(u.Path, "/")
			if len(parts) == 5 && parts[1] == "tenant" {
				current = parts[2]
			}
		}
		parsed, err := meshid.ParseTenant(current)
		if err != nil {
			return false
		}
		if tenant != "" && tenant != parsed.String() {
			return false
		}
		tenant = parsed.String()
	}
	return tenant != ""
}
