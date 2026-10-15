package meshcatalog

import (
	"fmt"
	"strings"

	"github.com/miekg/dns"
)

// packMDNSResponse applies wire rules after AnswerMDNS has selected records.
// mDNS responses to port 5353 flush unique RRsets; legacy one-shot unicast
// responses keep the query ID/question, clear cache-flush and cap TTL at 10s.
func packMDNSResponse(response *dns.Msg, legacy bool) ([]byte, error) {
	if response == nil {
		return nil, nil
	}
	wire := response.Copy()
	if !legacy {
		wire.Id = 0
		wire.Question = nil
	}
	for _, section := range [][]dns.RR{wire.Answer, wire.Ns, wire.Extra} {
		for _, rr := range section {
			if rr == nil {
				continue
			}
			h := rr.Header()
			h.Class &= 0x7fff
			if legacy {
				if h.Ttl > 10 {
					h.Ttl = 10
				}
			} else if h.Rrtype != dns.TypePTR {
				h.Class |= 0x8000
			}
		}
	}
	wire.Compress = true
	wire.Truncate(1400)
	return wire.Pack()
}

// packMDNSAnnouncements emits complete answer records in bounded multicast
// datagrams. Unique records use the cache-flush bit so a changed SRV or TXT
// replaces its predecessor immediately; shared PTRs retain ordinary class.
func packMDNSAnnouncements(records []dns.RR) ([][]byte, error) {
	const maxPacket = 1400
	var packets [][]byte
	msg := &dns.Msg{MsgHdr: dns.MsgHdr{Response: true, Authoritative: true}}
	flush := func() error {
		if len(msg.Answer) == 0 {
			return nil
		}
		data, err := msg.Pack()
		if err != nil {
			return err
		}
		if len(data) > maxPacket {
			return fmt.Errorf("mDNS announcement record exceeds %d bytes", maxPacket)
		}
		packets = append(packets, data)
		msg.Answer = nil
		return nil
	}
	for _, rr := range records {
		if rr == nil {
			continue
		}
		copy := dns.Copy(rr)
		if copy.Header().Rrtype != dns.TypePTR {
			copy.Header().Class |= 0x8000
		}
		msg.Answer = append(msg.Answer, copy)
		data, err := msg.Pack()
		if err != nil {
			return nil, err
		}
		if len(data) <= maxPacket {
			continue
		}
		msg.Answer = msg.Answer[:len(msg.Answer)-1]
		if err := flush(); err != nil {
			return nil, err
		}
		msg.Answer = append(msg.Answer, copy)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return packets, nil
}

// AnswerMDNS builds ordinary DNS-SD answers from already scoped projection
// records. It includes SRV/TXT/address additionals for PTR browse responses,
// supports service-type enumeration, and suppresses known answers whose TTL
// still exceeds half the projected lease. The caller chooses multicast or
// unicast delivery based on the question's QU bit and source port.
func AnswerMDNS(query *dns.Msg, projected []dns.RR) *dns.Msg {
	if query == nil || query.Response || len(query.Question) == 0 {
		return nil
	}
	response := new(dns.Msg)
	response.MsgHdr.Response = true
	response.MsgHdr.Authoritative = true
	response.Id = query.Id
	response.Question = append([]dns.Question(nil), query.Question...)
	for _, q := range query.Question {
		if q.Qclass&0x7fff != dns.ClassINET {
			continue
		}
		if strings.EqualFold(q.Name, "_services._dns-sd._udp.local.") && (q.Qtype == dns.TypePTR || q.Qtype == dns.TypeANY) {
			types := map[string]uint32{}
			for _, rr := range projected {
				ptr, ok := rr.(*dns.PTR)
				if !ok {
					continue
				}
				key := strings.ToLower(ptr.Hdr.Name)
				if old, seen := types[key]; !seen || ptr.Hdr.Ttl < old {
					types[key] = ptr.Hdr.Ttl
				}
			}
			for typ, ttl := range types {
				rr := &dns.PTR{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttl}, Ptr: typ}
				if !knownAnswer(query, rr) {
					response.Answer = append(response.Answer, rr)
				}
			}
			continue
		}
		for _, rr := range projected {
			if !strings.EqualFold(rr.Header().Name, q.Name) ||
				(q.Qtype != dns.TypeANY && rr.Header().Rrtype != q.Qtype) || knownAnswer(query, rr) {
				continue
			}
			response.Answer = append(response.Answer, dns.Copy(rr))
			if ptr, ok := rr.(*dns.PTR); ok {
				for _, extra := range projected {
					if strings.EqualFold(extra.Header().Name, ptr.Ptr) &&
						(extra.Header().Rrtype == dns.TypeSRV || extra.Header().Rrtype == dns.TypeTXT) {
						response.Extra = appendUniqueRR(response.Extra, extra)
						if srv, ok := extra.(*dns.SRV); ok {
							for _, address := range projected {
								if address.Header().Rrtype == dns.TypeA && strings.EqualFold(address.Header().Name, srv.Target) {
									response.Extra = appendUniqueRR(response.Extra, address)
								}
							}
						}
					}
				}
			} else if srv, ok := rr.(*dns.SRV); ok {
				for _, address := range projected {
					if address.Header().Rrtype == dns.TypeA && strings.EqualFold(address.Header().Name, srv.Target) {
						response.Extra = appendUniqueRR(response.Extra, address)
					}
				}
			}
		}
	}
	if len(response.Answer) == 0 {
		return nil
	}
	return response
}

func appendUniqueRR(dst []dns.RR, rr dns.RR) []dns.RR {
	for _, old := range dst {
		if dns.IsDuplicate(old, rr) {
			return dst
		}
	}
	return append(dst, dns.Copy(rr))
}

func knownAnswer(query *dns.Msg, proposed dns.RR) bool {
	for _, known := range query.Answer {
		if dns.IsDuplicate(known, proposed) && known.Header().Ttl >= proposed.Header().Ttl/2 {
			return true
		}
	}
	return false
}
