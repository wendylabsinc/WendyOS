package mesh

import (
	"fmt"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// deviceUUID is the canonical device UUID the device-<uuid> tests query.
const deviceUUID = "00000001-4000-8000-0000-000000000001"

// startTestDNS binds a listener on 127.0.0.1 with an ephemeral port, installs
// a fresh VIP table under tenant A, and returns the bound address plus the
// table so tests can assert what the DNS paths pinned.
func startTestDNS(t *testing.T, upstream string) (*DNSServer, string, *VIPTable) {
	t.Helper()
	logger, _ := observer.New(zap.InfoLevel)
	s := NewDNSServer(zap.New(logger), upstream)
	s.port = 0 // ephemeral for tests; production default is 53
	table, _ := newVIPTable(t)
	s.SetMeshIdentity(mustTenant(t, tenantA), table)
	if err := s.EnsureListener("127.0.0.1"); err != nil {
		t.Fatalf("EnsureListener: %v", err)
	}
	t.Cleanup(func() { s.ReleaseListener("127.0.0.1") })
	return s, s.listenerAddr("127.0.0.1"), table
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

// singleA asserts the response is a NOERROR with exactly one A record and
// returns it.
func singleA(t *testing.T, resp *dns.Msg, name string) *dns.A {
	t.Helper()
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("%s: rcode=%d answers=%d, want NOERROR with 1 answer", name, resp.Rcode, len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("%s: answer = %v, want an A record", name, resp.Answer[0])
	}
	return a
}

// wantSingleA asserts the response is a NOERROR with exactly one A record
// pointing at want, and returns it.
func wantSingleA(t *testing.T, resp *dns.Msg, name, want string) *dns.A {
	t.Helper()
	a := singleA(t, resp, name)
	if a.A.String() != want {
		t.Fatalf("%s: answer = %v, want A %s", name, a, want)
	}
	return a
}

func TestDNSAnswersMeshName(t *testing.T) {
	_, addr, table := startTestDNS(t, "")
	want := "10.99.0.1" // first usable host of the service CIDR
	wantSingleA(t, query(t, addr, "device-"+deviceUUID+".mesh.wendy.internal.", dns.TypeA),
		"device-"+deviceUUID+".mesh.wendy.internal.", want)

	// The answer came from the table, so the table must now pin it.
	device, err := meshid.ParseDevice(mustTenant(t, tenantA), deviceUUID)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := table.Lookup(device); !ok || got.String() != want {
		t.Fatalf("table Lookup = (%s, %v), want (%s, true)", got, ok, want)
	}
}

func TestDNSAnswersLegacyMeshAlias(t *testing.T) {
	_, addr, _ := startTestDNS(t, "")
	resp := query(t, addr, "device-"+deviceUUID+".cloud.wendy.dev.", dns.TypeA)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 1 {
		t.Fatalf("rcode=%d answers=%d, want legacy alias to remain resolvable", resp.Rcode, len(resp.Answer))
	}
}

func TestDNSMeshNameAAAAEmptyNoError(t *testing.T) {
	_, addr, _ := startTestDNS(t, "")
	resp := query(t, addr, "device-"+deviceUUID+".mesh.wendy.internal.", dns.TypeAAAA)
	if resp.Rcode != dns.RcodeSuccess || len(resp.Answer) != 0 {
		t.Fatalf("rcode=%d answers=%d, want NOERROR with 0 answers", resp.Rcode, len(resp.Answer))
	}
	for _, rr := range resp.Answer {
		if rr.Header().Rrtype == dns.TypeAAAA {
			t.Fatalf("mesh name answered with AAAA: %v", rr)
		}
	}
}

// TestDNSAnswersStayInsideServiceCIDR proves the answered address is a usable
// host of 10.99.0.0/16, never the network or broadcast address.
func TestDNSAnswersStayInsideServiceCIDR(t *testing.T) {
	_, addr, _ := startTestDNS(t, "")
	prefix := netip.MustParsePrefix(DefaultServiceCIDR)
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("device-%08x-4000-8000-0000-%012x.mesh.wendy.internal.", i, i)
		a := singleA(t, query(t, addr, name, dns.TypeA), name)
		ip := netip.MustParseAddr(a.A.String())
		if !prefix.Contains(ip) || !ip.Is4() {
			t.Fatalf("%s answered %s, outside %s", name, ip, prefix)
		}
		if ip.String() == "10.99.0.0" || ip.String() == "10.99.255.255" {
			t.Fatalf("%s answered the non-host address %s", name, ip)
		}
	}
}

// TestDNSNumericNameIsNotAMeshName covers the removal of the legacy
// device-<digits> form: it must never be answered from the VIP table.
func TestDNSNumericNameIsNotAMeshName(t *testing.T) {
	_, addr, table := startTestDNS(t, "")
	for _, name := range []string{
		"device-215.mesh.wendy.internal.",
		"device-0.mesh.wendy.internal.",
		"device-70000.mesh.wendy.internal.",
		"device-000000014000800080000000000000001.mesh.wendy.internal.", // unhyphenated UUID
	} {
		resp := query(t, addr, name, dns.TypeA)
		for _, rr := range resp.Answer {
			a, ok := rr.(*dns.A)
			if !ok {
				continue
			}
			if netip.MustParsePrefix(DefaultServiceCIDR).Contains(netip.MustParseAddr(a.A.String())) {
				t.Fatalf("%s was answered with a mesh VIP: %v", name, rr)
			}
		}
	}
	if stats := table.Stats(); stats.Allocated != 0 {
		t.Fatalf("legacy numeric names allocated %d VIPs, want 0", stats.Allocated)
	}
}

// TestDNSDevicePathNeedsNoResolver proves the device-<uuid> form answers from
// the name plus the host's own table alone.
func TestDNSDevicePathNeedsNoResolver(t *testing.T) {
	s := NewDNSServer(zap.NewNop(), "")
	table, _ := newVIPTable(t)
	s.SetMeshIdentity(mustTenant(t, tenantA), table) // no SetResolver call

	rcode, ip := answerA(t, s, "device-"+deviceUUID+".mesh.wendy.internal.")
	if rcode != dns.RcodeSuccess || ip != "10.99.0.1" {
		t.Fatalf("device path without a resolver: got rcode=%d ip=%q, want SUCCESS 10.99.0.1", rcode, ip)
	}
}

// TestDNSMeshNameWithoutIdentityIsNXDOMAIN proves the nil-safe identity seam:
// a server that has not been given its tenant + table answers NXDOMAIN rather
// than inventing addresses.
func TestDNSMeshNameWithoutIdentityIsNXDOMAIN(t *testing.T) {
	s := NewDNSServer(zap.NewNop(), "")
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]meshid.Device{
		"brave-dolphin": deviceForIndex(t, mustTenant(t, tenantA), 1),
	}})
	for _, name := range []string{
		"device-" + deviceUUID + ".mesh.wendy.internal.",
		"brave-dolphin.acme.mesh.wendy.internal.",
	} {
		rcode, _ := answerA(t, s, name)
		if rcode != dns.RcodeNameError {
			t.Fatalf("%s without identity: got rcode=%d, want NXDOMAIN", name, rcode)
		}
	}
	// SetMeshIdentity(nil table) clears the identity again.
	table, _ := newVIPTable(t)
	s.SetMeshIdentity(mustTenant(t, tenantA), table)
	if rcode, ip := answerA(t, s, "device-"+deviceUUID+".mesh.wendy.internal."); rcode != dns.RcodeSuccess || ip != "10.99.0.1" {
		t.Fatalf("after SetMeshIdentity: got rcode=%d ip=%q, want SUCCESS 10.99.0.1", rcode, ip)
	}
	s.SetMeshIdentity(mustTenant(t, tenantA), nil)
	if rcode, _ := answerA(t, s, "device-"+deviceUUID+".mesh.wendy.internal."); rcode != dns.RcodeNameError {
		t.Fatalf("after clearing identity: got rcode=%d, want NXDOMAIN", rcode)
	}
}

// TestDNSExhaustionIsNXDOMAIN fills a small table and checks both the answer
// and the warn log the dashboard needs to see the pool is full.
func TestDNSExhaustionIsNXDOMAIN(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	s := NewDNSServer(zap.New(core), "")
	tenant := mustTenant(t, tenantA)
	table, _ := newVIPTableCIDR(t, "10.99.0.0/24") // 254 usable VIPs
	for i := 1; i <= 254; i++ {
		if _, err := table.Allocate(deviceForIndex(t, tenant, i)); err != nil {
			t.Fatalf("filling the pool at %d: %v", i, err)
		}
	}
	s.SetMeshIdentity(tenant, table)

	rcode, ip := answerA(t, s, "device-000000ff-4000-8000-0000-0000000000ff.mesh.wendy.internal.")
	if rcode != dns.RcodeNameError || ip != "" {
		t.Fatalf("exhausted pool: got rcode=%d ip=%q, want NXDOMAIN with no answer", rcode, ip)
	}
	entries := logs.FilterMessage("wendy net DNS: VIP allocation failed").All()
	if len(entries) == 0 {
		t.Fatalf("exhaustion produced no warn log: %+v", logs.All())
	}
	if _, ok := entries[0].ContextMap()["error"]; !ok {
		t.Fatalf("exhaustion warn log has no error field: %+v", entries[0].ContextMap())
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

	_, addr, _ := startTestDNS(t, up.LocalAddr().String())
	resp := query(t, addr, "example.com.", dns.TypeA)
	if len(resp.Answer) != 1 {
		t.Fatalf("forwarded answers = %d, want 1", len(resp.Answer))
	}
}

func TestDNSNoUpstreamIsServfail(t *testing.T) {
	_, addr, _ := startTestDNS(t, "")
	resp := query(t, addr, "example.com.", dns.TypeA)
	if resp.Rcode != dns.RcodeServerFailure {
		t.Fatalf("rcode=%d, want SERVFAIL", resp.Rcode)
	}
}

func TestDNSListenerRefcount(t *testing.T) {
	s, _, _ := startTestDNS(t, "")
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

// fakeResolver is a test double for the friendly-name resolver: it maps a
// normalized device name to that device's principal.
type fakeResolver struct {
	slug   string
	byName map[string]meshid.Device
}

func (f fakeResolver) OrgSlug() string { return f.slug }
func (f fakeResolver) Resolve(name string) (meshid.Device, bool) {
	device, ok := f.byName[name]
	return device, ok
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
	s := NewDNSServer(zap.NewNop(), "")
	table, _ := newVIPTable(t)
	tenant := mustTenant(t, tenantA)
	s.SetMeshIdentity(tenant, table)
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]meshid.Device{"brave-dolphin": deviceForIndex(t, tenant, 1)}})

	rcode, ip := answerA(t, s, "brave-dolphin.acme.mesh.wendy.internal.")
	if rcode != dns.RcodeSuccess || ip != "10.99.0.1" {
		t.Fatalf("got rcode=%d ip=%q, want SUCCESS 10.99.0.1", rcode, ip)
	}
	if _, ok := table.Lookup(deviceForIndex(t, tenant, 1)); !ok {
		t.Fatal("friendly path did not pin the device's VIP in the table")
	}
}

// TestFriendlyAndDevicePathsAgreeOnVIP proves both paths resolve the same
// principal to the same pinned VIP, so a container can cache either name.
func TestFriendlyAndDevicePathsAgreeOnVIP(t *testing.T) {
	s := NewDNSServer(zap.NewNop(), "")
	table, _ := newVIPTable(t)
	tenant := mustTenant(t, tenantA)
	device := deviceForIndex(t, tenant, 1)
	s.SetMeshIdentity(tenant, table)
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]meshid.Device{"brave-dolphin": device}})

	rcode, fromDevice := answerA(t, s, "device-"+device.UUID().String()+".mesh.wendy.internal.")
	if rcode != dns.RcodeSuccess {
		t.Fatalf("device path: rcode=%d", rcode)
	}
	rcode, fromFriendly := answerA(t, s, "brave-dolphin.acme.mesh.wendy.internal.")
	if rcode != dns.RcodeSuccess {
		t.Fatalf("friendly path: rcode=%d", rcode)
	}
	if fromDevice != fromFriendly {
		t.Fatalf("device path answered %s, friendly path answered %s; want the same VIP", fromDevice, fromFriendly)
	}
	if stats := table.Stats(); stats.Allocated != 1 {
		t.Fatalf("Stats = %+v, want one VIP pinned for the shared principal", stats)
	}
}

func TestLegacyFriendlyNameResolves(t *testing.T) {
	s := NewDNSServer(zap.NewNop(), "")
	table, _ := newVIPTable(t)
	tenant := mustTenant(t, tenantA)
	s.SetMeshIdentity(tenant, table)
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]meshid.Device{"brave-dolphin": deviceForIndex(t, tenant, 1)}})

	rcode, ip := answerA(t, s, "brave-dolphin.acme.cloud.wendy.dev.")
	if rcode != dns.RcodeSuccess || ip != "10.99.0.1" {
		t.Fatalf("legacy friendly alias: got rcode=%d ip=%q, want SUCCESS 10.99.0.1", rcode, ip)
	}
}

// TestFriendlyNameAAAAEmptyNoError proves the mesh never emits AAAA, on either
// path: the VIP pool is IPv4-only.
func TestFriendlyNameAAAAEmptyNoError(t *testing.T) {
	s := NewDNSServer(zap.NewNop(), "")
	table, _ := newVIPTable(t)
	tenant := mustTenant(t, tenantA)
	s.SetMeshIdentity(tenant, table)
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]meshid.Device{"brave-dolphin": deviceForIndex(t, tenant, 1)}})

	for _, qname := range []string{"brave-dolphin.acme.mesh.wendy.internal.", "device-" + deviceUUID + ".mesh.wendy.internal."} {
		m := new(dns.Msg)
		m.SetQuestion(qname, dns.TypeAAAA)
		rw := &captureWriter{}
		s.handle(rw, m)
		if rw.msg == nil {
			t.Fatalf("%s: no reply written", qname)
		}
		if rw.msg.Rcode != dns.RcodeSuccess || len(rw.msg.Answer) != 0 {
			t.Fatalf("%s AAAA: got rcode=%d answers=%d, want NOERROR with 0 answers", qname, rw.msg.Rcode, len(rw.msg.Answer))
		}
	}
}

func TestFriendlyNameWrongOrgIsNXDOMAIN(t *testing.T) {
	s := NewDNSServer(zap.NewNop(), "")
	table, _ := newVIPTable(t)
	tenant := mustTenant(t, tenantA)
	s.SetMeshIdentity(tenant, table)
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]meshid.Device{"brave-dolphin": deviceForIndex(t, tenant, 1)}})

	rcode, _ := answerA(t, s, "brave-dolphin.other-org.mesh.wendy.internal.")
	if rcode != dns.RcodeNameError {
		t.Fatalf("wrong-org: got rcode=%d, want NXDOMAIN", rcode)
	}
	if stats := table.Stats(); stats.Allocated != 0 {
		t.Fatalf("wrong-org name allocated %d VIPs, want 0", stats.Allocated)
	}
}

func TestFriendlyNameUnknownDeviceIsNXDOMAIN(t *testing.T) {
	s := NewDNSServer(zap.NewNop(), "")
	table, _ := newVIPTable(t)
	s.SetMeshIdentity(mustTenant(t, tenantA), table)
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]meshid.Device{}})

	rcode, _ := answerA(t, s, "ghost.acme.mesh.wendy.internal.")
	if rcode != dns.RcodeNameError {
		t.Fatalf("unknown device: got rcode=%d, want NXDOMAIN", rcode)
	}
}

// TestFriendlyNameForeignTenantIsNXDOMAIN proves the tenant guard: a resolver
// that hands back a device from another tenant must not get it a local VIP.
func TestFriendlyNameForeignTenantIsNXDOMAIN(t *testing.T) {
	s := NewDNSServer(zap.NewNop(), "")
	table, _ := newVIPTable(t)
	s.SetMeshIdentity(mustTenant(t, tenantA), table)
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]meshid.Device{
		"brave-dolphin": deviceForIndex(t, mustTenant(t, tenantB), 1),
	}})

	rcode, _ := answerA(t, s, "brave-dolphin.acme.mesh.wendy.internal.")
	if rcode != dns.RcodeNameError {
		t.Fatalf("foreign-tenant device: got rcode=%d, want NXDOMAIN", rcode)
	}
	if stats := table.Stats(); stats.Allocated != 0 {
		t.Fatalf("foreign-tenant device allocated %d VIPs, want 0", stats.Allocated)
	}
}

func TestFriendlyNameNoResolverIsNXDOMAIN(t *testing.T) {
	s := NewDNSServer(zap.NewNop(), "")
	table, _ := newVIPTable(t)
	s.SetMeshIdentity(mustTenant(t, tenantA), table)
	rcode, _ := answerA(t, s, "brave-dolphin.acme.mesh.wendy.internal.")
	if rcode != dns.RcodeNameError {
		t.Fatalf("no resolver: got rcode=%d, want NXDOMAIN", rcode)
	}
}

// TestFriendlyNameDoubledHyphenNormalizes proves answerFriendly re-normalizes
// the captured labels before comparing/resolving: the regex admits a doubled
// hyphen ("a--b"), which must still resolve against the resolver's
// already-normalized name ("a-b") because both sides go through Normalize.
func TestFriendlyNameDoubledHyphenNormalizes(t *testing.T) {
	s := NewDNSServer(zap.NewNop(), "")
	table, _ := newVIPTable(t)
	tenant := mustTenant(t, tenantA)
	s.SetMeshIdentity(tenant, table)
	s.SetResolver(fakeResolver{slug: "acme", byName: map[string]meshid.Device{"a-b": deviceForIndex(t, tenant, 1)}})

	rcode, ip := answerA(t, s, "a--b.acme.mesh.wendy.internal.")
	if rcode != dns.RcodeSuccess || ip != "10.99.0.1" {
		t.Fatalf("doubled hyphen: got rcode=%d ip=%q, want SUCCESS 10.99.0.1", rcode, ip)
	}
}
