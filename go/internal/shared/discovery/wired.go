package discovery

import (
	"context"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// maxWiredAddresses bounds what one TXT record can make the CLI dial.
const maxWiredAddresses = 8

// cableAnswerTimeout bounds the wait for a cable's reply; a device on the link
// answers within milliseconds.
const cableAnswerTimeout = 300 * time.Millisecond

// mdnsPort is where CableAnswers asks; a var so tests can run a responder.
var mdnsPort uint16 = 5353

// ParseWiredTXT reads the comma-separated `wired` TXT record, keeping only
// well-formed addresses.
func ParseWiredTXT(value string) []string {
	var out []string
	for _, field := range strings.Split(value, ",") {
		a, err := netip.ParseAddr(strings.TrimSpace(field))
		if err != nil || slices.Contains(out, a.String()) {
			continue
		}
		out = append(out, a.String())
		if len(out) == maxWiredAddresses {
			break
		}
	}
	return out
}

// OnLink reports whether addr lies in a subnet of one of this host's
// interfaces, i.e. is reachable without a router. Link-local addresses never
// qualify: every link shares that range.
func OnLink(addr string) bool {
	ip := net.ParseIP(strings.TrimSpace(addr))
	if ip == nil || ip.IsLinkLocalUnicast() {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && !ipn.IP.Equal(ip) && ipn.Contains(ip) {
			return true
		}
	}
	return false
}

// CableAnswers asks the mDNS responder at addr itself for host's address and
// reports whether it answers addr, which only host does. A host's mDNS cache
// cannot tell: a device answering on two ports of one link keeps replacing one
// cached address with the other.
func CableAnswers(ctx context.Context, addr, host string) bool {
	ip, err := netip.ParseAddr(addr)
	if err != nil || !ip.Is4() {
		return false
	}
	host = strings.TrimSuffix(host, ".")
	name, err := dnsmessage.NewName(host + ".")
	if err != nil || host == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, cableAnswerTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp4", netip.AddrPortFrom(ip, mdnsPort).String())
	if err != nil {
		return false
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	id := uint16(rand.Uint32())
	query, err := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}).Pack()
	if err != nil {
		return false
	}
	if _, err := conn.Write(query); err != nil {
		return false
	}
	resend := time.AfterFunc(cableAnswerTimeout/3, func() { _, _ = conn.Write(query) }) // a lost packet is not silence
	defer resend.Stop()
	buf := make([]byte, 9000) // the largest mDNS message
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return false
		}
		if answersWith(buf[:n], id, name, ip) {
			return true
		}
	}
}

// answersWith reports whether reply answers query id with name's address ip.
func answersWith(reply []byte, id uint16, name dnsmessage.Name, ip netip.Addr) bool {
	var p dnsmessage.Parser
	h, err := p.Start(reply)
	if err != nil || h.ID != id || !h.Response || p.SkipAllQuestions() != nil {
		return false
	}
	for {
		rh, err := p.AnswerHeader()
		if err != nil {
			return false
		}
		if rh.Type != dnsmessage.TypeA || !strings.EqualFold(rh.Name.String(), name.String()) {
			if p.SkipAnswer() != nil {
				return false
			}
			continue
		}
		a, err := p.AResource()
		if err != nil {
			return false
		}
		if netip.AddrFrom4(a.A) == ip {
			return true
		}
	}
}
