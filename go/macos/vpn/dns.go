package vpn

import (
	"github.com/miekg/dns"
	"net"
	"strings"
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
	if !strings.HasSuffix(name, ".mesh.wendy.dev.") && !strings.HasSuffix(name, ".cloud.wendy.dev.") {
		reply.Rcode = dns.RcodeRefused
		return reply.Pack()
	}
	head := strings.TrimSuffix(strings.TrimSuffix(name, ".mesh.wendy.dev."), ".cloud.wendy.dev.")
	if !strings.HasPrefix(head, "device-") || strings.Contains(head, ".") {
		reply.Rcode = dns.RcodeRefused
		reply.Authoritative = false
		return reply.Pack()
	}
	for _, r := range routes {
		if name == dns.Fqdn(r.Host) {
			if q.Qtype == dns.TypeA {
				reply.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.ParseIP(r.Address)}}
			}
			return reply.Pack()
		}
	}
	reply.Rcode = dns.RcodeNameError
	return reply.Pack()
}
