package mesh

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

// startTestDNS binds a listener on 127.0.0.1 with an ephemeral port and
// returns the bound address.
func startTestDNS(t *testing.T, upstream string) (*DNSServer, string) {
	t.Helper()
	s := NewDNSServer(zap.NewNop(), upstream)
	s.port = 0 // ephemeral for tests; production default is 53
	if err := s.EnsureListener("127.0.0.1"); err != nil {
		t.Fatalf("EnsureListener: %v", err)
	}
	t.Cleanup(func() { s.ReleaseListener("127.0.0.1") })
	return s, s.listenerAddr("127.0.0.1")
}

func query(t *testing.T, addr, name string, qtype uint16) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	c := &dns.Client{Timeout: 2 * time.Second}
	resp, _, err := c.Exchange(m, addr)
	if err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
	return resp
}

func TestDNSAnswersMeshName(t *testing.T) {
	_, addr := startTestDNS(t, "")
	resp := query(t, addr, "device-215.mesh.wendy.internal.", dns.TypeA)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("rcode=%d answers=%d, want NOERROR with 1 answer", resp.Rcode, len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok || a.A.String() != "10.99.0.215" {
		t.Fatalf("answer = %v, want A 10.99.0.215", resp.Answer[0])
	}
}

func TestDNSAnswersLegacyMeshAlias(t *testing.T) {
	_, addr := startTestDNS(t, "")
	resp := query(t, addr, "device-215.cloud.wendy.dev.", dns.TypeA)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("rcode=%d answers=%d, want legacy alias to remain resolvable", resp.Rcode, len(resp.Answer))
	}
}

func TestDNSMeshNameAAAAEmptyNoError(t *testing.T) {
	_, addr := startTestDNS(t, "")
	resp := query(t, addr, "device-215.mesh.wendy.internal.", dns.TypeAAAA)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 0 {
		t.Fatalf("rcode=%d answers=%d, want NOERROR with 0 answers", resp.Rcode, len(resp.Answer))
	}
}

func TestDNSOutOfRangeIsNXDOMAIN(t *testing.T) {
	_, addr := startTestDNS(t, "")
	for _, name := range []string{"device-0.mesh.wendy.internal.", "device-70000.mesh.wendy.internal."} {
		resp := query(t, addr, name, dns.TypeA)
		if resp.Rcode != dns.RcodeNameError {
			t.Fatalf("%s: rcode=%d, want NXDOMAIN", name, resp.Rcode)
		}
	}
}

func TestDNSForwardsNonMeshNames(t *testing.T) {
	// Fake upstream that answers everything with 192.0.2.1.
	up, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstreamSrv := &dns.Server{PacketConn: up, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Answer = append(m.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5},
			A:   net.ParseIP("192.0.2.1"),
		})
		_ = w.WriteMsg(m)
	})}
	go upstreamSrv.ActivateAndServe()
	t.Cleanup(func() { _ = upstreamSrv.Shutdown() })

	_, addr := startTestDNS(t, up.LocalAddr().String())
	resp := query(t, addr, "example.com.", dns.TypeA)
	if len(resp.Answer) != 1 {
		t.Fatalf("forwarded answers = %d, want 1", len(resp.Answer))
	}
}

func TestDNSNoUpstreamIsServfail(t *testing.T) {
	_, addr := startTestDNS(t, "")
	resp := query(t, addr, "example.com.", dns.TypeA)
	if resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode=%d, want SERVFAIL", resp.Rcode)
	}
}

func TestDNSListenerRefcount(t *testing.T) {
	s, _ := startTestDNS(t, "")
	if err := s.EnsureListener("127.0.0.1"); err != nil { // refs=2
		t.Fatal(err)
	}
	s.ReleaseListener("127.0.0.1") // refs=1, still listening
	if s.listenerAddr("127.0.0.1") == "" {
		t.Fatal("listener shut down while still referenced")
	}
	s.ReleaseListener("127.0.0.1") // refs=0, shut down
	if s.listenerAddr("127.0.0.1") != "" {
		t.Fatal("listener still up after last release")
	}
	// startTestDNS cleanup releases once more; make that a no-op by re-adding.
	if err := s.EnsureListener("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
}

// captureWriter is a minimal dns.ResponseWriter stub that captures the
// written message for direct s.handle(...) assertions in tests below.
type captureWriter struct {
	dns.ResponseWriter
	msg *dns.Msg
}

func (c *captureWriter) WriteMsg(m *dns.Msg) error { c.msg = m; return nil }
func (c *captureWriter) LocalAddr() net.Addr       { return &net.UDPAddr{} }
func (c *captureWriter) RemoteAddr() net.Addr      { return &net.UDPAddr{} }

// fakeResolver is a test double for the friendly-name resolver.
type fakeResolver struct {
	slug   string
	byName map[string]int32
}

func (f fakeResolver) OrgSlug() string { return f.slug }
func (f fakeResolver) Resolve(name string) (int32, bool) {
	id, ok := f.byName[name]
	return id, ok
}

func answerA(t *testing.T, s *DNSServer, qname string) (rcode int, ip string) {
	t.Helper()
	m := new(dns.Msg)
	m.SetQuestion(qname, dns.TypeA)
	rw := &captureWriter{}
	s.handle(rw, m)
	if rw.msg == nil {
		t.Fatal("no reply written")
	}
	if len(rw.msg.Answer) == 0 {
		return rw.msg.Rcode, ""
	}
	return rw.msg.Rcode, rw.msg.Answer[0].(*dns.A).A.String()
}

func TestFriendlyNameResolves(t *testing.T) {
	s := NewDNSServer(zaptest.NewLogger(t), "")
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]int32{"brave-dolphin": 215}})

	rcode, ip := answerA(t, s, "brave-dolphin.acme.mesh.wendy.internal.")
	if rcode != dns.RcodeSuccess || ip != "10.99.0.215" {
		t.Fatalf("got rcode=%d ip=%q, want SUCCESS 10.99.0.215", rcode, ip)
	}
}

func TestLegacyFriendlyNameResolves(t *testing.T) {
	s := NewDNSServer(zaptest.NewLogger(t), "")
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]int32{"brave-dolphin": 215}})

	rcode, ip := answerA(t, s, "brave-dolphin.acme.cloud.wendy.dev.")
	if rcode != dns.RcodeSuccess || ip != "10.99.0.215" {
		t.Fatalf("legacy friendly alias: got rcode=%d ip=%q, want SUCCESS 10.99.0.215", rcode, ip)
	}
}

func TestFriendlyNameWrongOrgIsNXDOMAIN(t *testing.T) {
	s := NewDNSServer(zaptest.NewLogger(t), "")
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]int32{"brave-dolphin": 215}})

	rcode, _ := answerA(t, s, "brave-dolphin.other-org.mesh.wendy.internal.")
	if rcode != dns.RcodeNameError {
		t.Fatalf("wrong-org: got rcode=%d, want NXDOMAIN", rcode)
	}
}

func TestFriendlyNameUnknownDeviceIsNXDOMAIN(t *testing.T) {
	s := NewDNSServer(zaptest.NewLogger(t), "")
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]int32{}})

	rcode, _ := answerA(t, s, "ghost.acme.mesh.wendy.internal.")
	if rcode != dns.RcodeNameError {
		t.Fatalf("unknown device: got rcode=%d, want NXDOMAIN", rcode)
	}
}

func TestFriendlyNameNoResolverIsNXDOMAIN(t *testing.T) {
	s := NewDNSServer(zaptest.NewLogger(t), "")
	rcode, _ := answerA(t, s, "brave-dolphin.acme.mesh.wendy.internal.")
	if rcode != dns.RcodeNameError {
		t.Fatalf("no resolver: got rcode=%d, want NXDOMAIN", rcode)
	}
}

func TestNumericNamePathUnchanged(t *testing.T) {
	s := NewDNSServer(zaptest.NewLogger(t), "")
	s.SetResolver(fakeResolver{slug: "acme"})
	rcode, ip := answerA(t, s, "device-215.mesh.wendy.internal.")
	if rcode != dns.RcodeSuccess || ip != "10.99.0.215" {
		t.Fatalf("numeric: got rcode=%d ip=%q, want SUCCESS 10.99.0.215", rcode, ip)
	}
}

// TestFriendlyNameDoubledHyphenNormalizes proves answerFriendly re-normalizes
// the captured labels before comparing/resolving: the regex admits a doubled
// hyphen ("a--b"), which must still resolve against the resolver's
// already-normalized name ("a-b") because both sides go through Normalize.
func TestFriendlyNameDoubledHyphenNormalizes(t *testing.T) {
	s := NewDNSServer(zaptest.NewLogger(t), "")
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]int32{"a-b": 215}})

	rcode, ip := answerA(t, s, "a--b.acme.mesh.wendy.internal.")
	if rcode != dns.RcodeSuccess || ip != "10.99.0.215" {
		t.Fatalf("doubled hyphen: got rcode=%d ip=%q, want SUCCESS 10.99.0.215", rcode, ip)
	}
}
