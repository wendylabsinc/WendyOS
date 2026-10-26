package mesh

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshname"
	"go.uber.org/zap"
)

var meshDNSSuffixRE = `(?:` + regexp.QuoteMeta(meshname.Suffix) + `|` + regexp.QuoteMeta(meshname.LegacySuffix) + `)`

// canonicalUUIDRE matches the canonical textual UUID form (lowercase,
// hyphenated) that meshid enforces at every identity boundary. Uppercase or
// unhyphenated spellings are deliberately not accepted: they would resolve to
// a device the peer's own identity never produces.
const canonicalUUIDRE = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

// meshNameRE accepts the canonical device-<uuid> form of the private mesh
// namespace and its legacy public alias.
var meshNameRE = regexp.MustCompile(`^device-(` + canonicalUUIDRE + `)\.` + meshDNSSuffixRE + `\.$`)

// friendlyNameRE matches <devicename>.<org-slug>.mesh.wendy.internal. and its
// legacy alias. It never overlaps meshNameRE: the device form has one label
// before the suffix, while this form has two.
var friendlyNameRE = regexp.MustCompile(`^([a-z0-9-]+)\.([a-z0-9-]+)\.` + meshDNSSuffixRE + `\.$`)

// Resolver maps a normalized device name to that device's principal within
// this device's own tenant, and reports this device's own org slug. It is
// implemented in the services package (which owns the mDNS + cloud-roster
// dependencies) and injected via SetResolver, keeping the mesh package free of
// those deps.
type Resolver interface {
	Resolve(name string) (meshid.Device, bool)
	OrgSlug() string
}

// meshIdentity is the provisioning identity the mesh name paths resolve
// through: the tenant device names are parsed under, and the VIP table that
// pins them. Absent until SetMeshIdentity supplies it.
type meshIdentity struct {
	tenant meshid.Tenant
	vips   *VIPTable
}

// DNSServer answers device-<uuid>.mesh.wendy.internal with the device's mesh
// VIP and forwards every other query to the host's upstream resolver. One UDP
// listener is bound per app bridge gateway address, refcounted by the number
// of running meshed containers behind that bridge.
type DNSServer struct {
	logger   *zap.Logger
	upstream string // host resolver, e.g. "127.0.0.53:53"; empty → SERVFAIL for non-mesh names
	port     int    // 53 in production; overridable for tests

	mu        sync.Mutex
	listeners map[string]*gatewayListener
	resolver  Resolver      // friendly-name resolver; nil until SetResolver
	identity  *meshIdentity // tenant + VIP table; nil until SetMeshIdentity
}

type gatewayListener struct {
	closing chan struct{}
	refs    int
	srv     *dns.Server
	addr    string
}

// The shutdown seam lets lifecycle tests hold a real in-flight DNS request.
var shutdownDNSGateway = (*dns.Server).Shutdown

func NewDNSServer(logger *zap.Logger, upstream string) *DNSServer {
	return &DNSServer{
		logger:    logger,
		upstream:  upstream,
		port:      53,
		listeners: make(map[string]*gatewayListener),
	}
}

// SetResolver installs the friendly-name resolver. Safe to call once during
// agent startup after provisioning identity is available; a nil resolver
// makes every friendly name NXDOMAIN while the device-<uuid> path keeps
// working.
func (s *DNSServer) SetResolver(r Resolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolver = r
}

// SetMeshIdentity installs the tenant and VIP table mesh names resolve
// through. Safe to call once during agent startup after provisioning identity
// is available; without it every mesh name is NXDOMAIN, since a name with no
// tenant to parse under and no table to pin a VIP in has no answer to give.
func (s *DNSServer) SetMeshIdentity(tenant meshid.Tenant, vips *VIPTable) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if vips == nil {
		s.identity = nil
		return
	}
	s.identity = &meshIdentity{tenant: tenant, vips: vips}
}

// EnsureListener binds a UDP DNS listener on gatewayIP (idempotent,
// refcounted). Callers pair every EnsureListener with a ReleaseListener.
func (s *DNSServer) EnsureListener(gatewayIP string) error {
	for {
		s.mu.Lock()
		if l, ok := s.listeners[gatewayIP]; ok {
			if l.closing != nil {
				done := l.closing
				s.mu.Unlock()
				<-done
				continue // The old socket is closed; this acquisition can bind again.
			}
			l.refs++
			s.mu.Unlock()
			return nil
		}
		pc, err := net.ListenPacket("udp", net.JoinHostPort(gatewayIP, strconv.Itoa(s.port)))
		if err != nil {
			s.mu.Unlock()
			return fmt.Errorf("wendy net DNS: listen %s: %w", gatewayIP, err)
		}
		started, ended := make(chan struct{}), make(chan error, 1)
		srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(s.handle),
			NotifyStartedFunc: func() { close(started) }}
		go func() {
			err := srv.ActivateAndServe()
			ended <- err
			if err != nil {
				s.logger.Warn("wendy net DNS listener exited", zap.String("gateway", gatewayIP), zap.Error(err))
			}
		}()
		// Shutdown before ActivateAndServe starts returns "server not started"
		// without closing its socket. Do not publish a reference until ready.
		select {
		case <-started:
		case err := <-ended:
			_ = pc.Close()
			s.mu.Unlock()
			if err == nil {
				err = errors.New("DNS listener exited before startup")
			}
			return fmt.Errorf("wendy net DNS: start %s: %w", gatewayIP, err)
		}
		s.listeners[gatewayIP] = &gatewayListener{refs: 1, srv: srv, addr: pc.LocalAddr().String()}
		s.mu.Unlock()
		s.logger.Info("wendy net DNS listening", zap.String("addr", pc.LocalAddr().String()))
		return nil
	}
}

// ReleaseListener drops one reference; the listener shuts down when the last
// meshed container behind that gateway stops. Releasing an unknown gateway is
// a no-op.
func (s *DNSServer) ReleaseListener(gatewayIP string) {
	s.mu.Lock()
	l, ok := s.listeners[gatewayIP]
	if !ok || l.closing != nil {
		s.mu.Unlock()
		return
	}
	l.refs--
	if l.refs > 0 {
		s.mu.Unlock()
		return
	}
	l.closing = make(chan struct{})
	s.mu.Unlock()
	// Shutdown drains handlers, which also take s.mu. Keep a per-gateway
	// closing entry so new acquisitions wait for the socket without blocking
	// those handlers or other gateways behind the global state mutex.
	if err := shutdownDNSGateway(l.srv); err != nil {
		s.logger.Warn("wendy net DNS shutdown", zap.String("gateway", gatewayIP), zap.Error(err))
	}
	_ = l.srv.PacketConn.Close()
	s.mu.Lock()
	delete(s.listeners, gatewayIP)
	close(l.closing)
	s.mu.Unlock()
}

// listenerAddr returns the bound address for a gateway, or "" if not
// listening. Used by tests.
func (s *DNSServer) listenerAddr(gatewayIP string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.listeners[gatewayIP]; ok {
		return l.addr
	}
	return ""
}

func (s *DNSServer) handle(w dns.ResponseWriter, r *dns.Msg) {
	if len(r.Question) != 1 {
		s.reply(w, r, dns.RcodeRefused)
		return
	}
	q := r.Question[0]
	lower := strings.ToLower(q.Name)
	if m := meshNameRE.FindStringSubmatch(lower); m != nil {
		s.answerDevice(w, r, q, m[1])
		return
	}
	if m := friendlyNameRE.FindStringSubmatch(lower); m != nil {
		s.answerFriendly(w, r, q, m[1], m[2])
		return
	}
	s.forward(w, r)
}

// answerDevice resolves the canonical device-<uuid> form. It needs no roster
// data at all: the name carries the device UUID, and the VIP comes from the
// host's own table.
func (s *DNSServer) answerDevice(w dns.ResponseWriter, r *dns.Msg, q dns.Question, uuidText string) {
	s.mu.Lock()
	id := s.identity
	s.mu.Unlock()
	if id == nil {
		s.reply(w, r, dns.RcodeNameError)
		return
	}
	device, err := meshid.ParseDevice(id.tenant, uuidText)
	if err != nil {
		s.reply(w, r, dns.RcodeNameError)
		return
	}
	s.answerAllocated(w, r, q, id.vips, device)
}

func (s *DNSServer) answerFriendly(w dns.ResponseWriter, r *dns.Msg, q dns.Question, name, orgSlug string) {
	s.mu.Lock()
	resolver, id := s.resolver, s.identity
	s.mu.Unlock()
	// Re-normalize both labels so slightly-off-but-regex-valid forms (e.g. a
	// doubled hyphen) compare symmetrically against the resolver's
	// normalized names; Normalize is idempotent so already-normal names are
	// unaffected.
	name = Normalize(name)
	orgSlug = Normalize(orgSlug)
	if resolver == nil || id == nil || resolver.OrgSlug() == "" || orgSlug != resolver.OrgSlug() {
		s.reply(w, r, dns.RcodeNameError)
		return
	}
	device, ok := resolver.Resolve(name)
	if !ok {
		s.reply(w, r, dns.RcodeNameError)
		return
	}
	// The roster is scoped to our org, but the resolver is an injected
	// dependency: a device from another tenant is refused rather than
	// silently pinned to a local VIP.
	if device.Tenant().UUID() != id.tenant.UUID() {
		s.reply(w, r, dns.RcodeNameError)
		return
	}
	s.answerAllocated(w, r, q, id.vips, device)
}

// answerAllocated pins the device's VIP in the host table and writes its A
// record, or NXDOMAIN when the pool is exhausted. Shared by the device and
// friendly paths so both emit identical answers.
func (s *DNSServer) answerAllocated(w dns.ResponseWriter, r *dns.Msg, q dns.Question, vips *VIPTable, device meshid.Device) {
	vip, err := vips.Allocate(device)
	if err != nil {
		s.logger.Warn("wendy net DNS: VIP allocation failed",
			zap.String("mesh.device", device.UUID().String()),
			zap.String("mesh.name", q.Name),
			zap.Error(err))
		s.reply(w, r, dns.RcodeNameError)
		return
	}
	s.answerVIP(w, r, q, vip)
}

// answerVIP writes the A record for a resolved VIP. Shared by the device and
// friendly paths so both emit identical answers.
func (s *DNSServer) answerVIP(w dns.ResponseWriter, r *dns.Msg, q dns.Question, vip netip.Addr) {
	resp := new(dns.Msg)
	resp.SetReply(r)
	resp.Authoritative = true
	if q.Qtype == dns.TypeA {
		resp.Answer = append(resp.Answer, &dns.A{
			Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
			A:   net.IP(vip.AsSlice()),
		})
	}
	// AAAA and other types for a valid mesh name: NOERROR with no answers.
	// The mesh VIP pool is IPv4-only; a v6 answer would advertise an address
	// nothing routes.
	_ = w.WriteMsg(resp)
}

func (s *DNSServer) forward(w dns.ResponseWriter, r *dns.Msg) {
	if s.upstream == "" {
		s.reply(w, r, dns.RcodeServerFailure)
		return
	}
	c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
	resp, _, err := c.Exchange(r, s.upstream)
	if err != nil {
		s.reply(w, r, dns.RcodeServerFailure)
		return
	}
	_ = w.WriteMsg(resp)
}

func (s *DNSServer) reply(w dns.ResponseWriter, r *dns.Msg, rcode int) {
	m := new(dns.Msg)
	m.SetRcode(r, rcode)
	_ = w.WriteMsg(m)
}
