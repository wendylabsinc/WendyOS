//go:build linux

package meshsharing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"sync"
	"time"

	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type grantedTunnel struct {
	peer         meshid.Device
	lease        tunnelLease
	tunnel       *internetTunnel
	rules        []internetRule
	routingRules []netlink.Rule
	revoked      chan struct{}
	revokeOnce   sync.Once
}

type borrowedTunnel struct {
	grantedTunnel
	control  net.Conn
	done     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
}

// linuxTunnelPolicy owns the separate Internet overlay. Babel sees only the
// outer device ULA endpoints. All leases and Internet routes terminate here.
type linuxTunnelPolicy struct {
	mu                sync.Mutex
	credentials       *localmesh.Credentials
	server            io.Closer
	resolver          *localmesh.HostPolicy
	base              []internetRule
	shareRules        []internetRule
	forwarding6       bool
	pool              leasePool
	grants            map[uint16]*grantedTunnel
	borrowed          *borrowedTunnel
	dialer            leaseDialer
	sharing, upstream string
	sharing6          bool
	donorRouting      donorRouting
	dns               *exec.Cmd
	dnsDone           chan error
	closed            bool
	probeIPv6         func(context.Context, string, string) bool
	borrowers         sync.WaitGroup
}

func newLinuxTunnelPolicy(ctx context.Context, credentials *localmesh.Credentials) (p *linuxTunnelPolicy, err error) {
	if credentials == nil || !credentials.Device.IsValid() {
		return nil, errors.New("Wendy Net Internet sharing requires credentials")
	}
	p = &linuxTunnelPolicy{credentials: credentials, grants: map[uint16]*grantedTunnel{}, resolver: &localmesh.HostPolicy{}, probeIPv6: probeUplinkIPv6}
	p.dialer.dial = func(ctx context.Context, gateway meshid.Device) (net.Conn, tunnelLease, error) {
		return dialGatewayLease(ctx, credentials, gateway)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, p.Close())
		}
	}()
	// Delete stale links first, removing old default and return routes before
	// reclaiming firewall entries or making any address available again.
	if err = reclaimInternetTunnels(); err != nil {
		return p, err
	}
	if err = reclaimDonorRouting(); err != nil {
		return p, err
	}
	// A crashed old-kernel gateway may still have global IPv6 forwarding
	// enabled. Restore that baseline before removing its firewall fallback.
	if err = hostnetwork.ReconcileIPv6Forwarding(); err != nil {
		return p, err
	}
	if err = checkLeasePoolsAvailable(); err != nil {
		return p, err
	}
	if err = reclaimInternetRules(ctx); err != nil {
		return p, err
	}
	for _, pool := range []string{leaseIPv4Pool, leaseIPv6Pool} {
		if err = flushTunnelConntrack(netip.MustParsePrefix(pool)); err != nil {
			return p, err
		}
	}
	if err = hostnetwork.AcquireForwarding("mesh-internet"); err != nil {
		return p, err
	}
	if err = hostnetwork.InitBridgeInternetChain(); err != nil {
		return p, err
	}
	for _, family := range []string{"iptables", "ip6tables"} {
		for _, direction := range []string{"-i", "-o"} {
			if err = addInternetRule(ctx, &p.base, family, "filter", "FORWARD", direction, tunnelPrefix+"+", "-j", "DROP"); err != nil {
				return p, err
			}
		}
		// Inner packets cannot reach this host's local services. DNS and lease
		// control use the authenticated outer mesh ULA path instead.
		if err = addInternetRule(ctx, &p.base, family, "filter", "INPUT", "-i", tunnelPrefix+"+", "-j", "DROP"); err != nil {
			return p, err
		}
	}
	p.server, err = startLeaseServer(ctx, credentials, p)
	return p, err
}

func (p *linuxTunnelPolicy) SetDNS(ctx context.Context, iface, address string) error {
	return p.resolver.SetDNS(ctx, iface, address)
}

func physicalIPv6Uplink(iface string) bool {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return false
	}
	addresses, err := netlink.AddrList(link, netlink.FAMILY_V6)
	if err != nil {
		return false
	}
	global := false
	for _, address := range addresses {
		ip, ok := netip.AddrFromSlice(address.IP)
		if ok && ip.IsGlobalUnicast() && !ip.IsPrivate() {
			global = true
		}
	}
	if !global {
		return false
	}
	routes, err := netlink.RouteList(link, netlink.FAMILY_V6)
	if err != nil {
		return false
	}
	for _, route := range routes {
		if route.Dst == nil || route.Dst.String() == "::/0" {
			return true
		}
	}
	return false
}

func (p *linuxTunnelPolicy) SetSharing(ctx context.Context, iface, upstream string) (err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("Wendy Net Internet policy closed")
	}
	if iface != "" && (!validPhysicalName(iface) || net.ParseIP(upstream) == nil) {
		return errors.New("invalid sharing uplink or DNS")
	}
	for _, grant := range p.grants {
		select {
		case <-grant.revoked:
			// A prior cleanup may have hit an xtables lock or transient
			// netlink error. Retry while retaining the address reservation.
			if err := p.revokeLocked(ctx, grant); err != nil {
				return err
			}
		default:
		}
	}
	share6 := iface != "" && physicalIPv6Uplink(iface) && p.probeIPv6(ctx, iface, upstream)
	var routes []netlink.Route
	if iface != "" {
		routes, err = selectedDonorRoutes(iface, share6)
		if err != nil {
			return errors.Join(err, p.stopSharingLocked(ctx))
		}
	}
	if p.dnsDone != nil {
		select {
		case exit := <-p.dnsDone:
			p.dnsDone <- exit
			return errors.Join(fmt.Errorf("Wendy Net DNS proxy stopped: %v", exit), p.stopSharingLocked(ctx))
		default:
		}
	}
	if iface == p.sharing && upstream == p.upstream && share6 == p.sharing6 && sameDonorRoutes(routes, p.donorRouting.routes) && (iface != "" || (len(p.grants) == 0 && len(p.shareRules) == 0 && !p.forwarding6)) {
		return nil
	}
	if err = p.stopSharingLocked(ctx); err != nil {
		return err
	}
	if iface == "" {
		return nil
	}
	if p.borrowed != nil {
		return errors.New("cannot share a borrowed Internet route")
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			err = errors.Join(err, p.stopSharingLocked(cleanup))
		}
	}()
	if err = p.donorRouting.install(routes); err != nil {
		return err
	}
	if share6 {
		if err = hostnetwork.AcquireIPv6Forwarding("mesh-internet"); err != nil {
			return errors.Join(err, hostnetwork.ReconcileIPv6Forwarding())
		}
		p.forwarding6 = true
	}
	listen := p.credentials.Device.MeshAddress().String()
	for _, proto := range []string{"udp", "tcp"} {
		if err = addInternetRule(ctx, &p.shareRules, "ip6tables", "filter", "INPUT", "!", "-i", "wlmp+", "-d", listen, "-p", proto, "--dport", "53", "-j", "DROP"); err != nil {
			return err
		}
	}
	args := []string{"--conf-file=/dev/null", "--keep-in-foreground", "--no-resolv", "--no-hosts", "--bind-interfaces", "--listen-address=" + listen, "--server=" + upstream + "@" + iface, "--cache-size=150", "--pid-file="}
	if _, err = command(ctx, "dnsmasq", append(args, "--test")...); err != nil {
		return err
	}
	p.dns = exec.Command("dnsmasq", args...)
	if err = p.dns.Start(); err != nil {
		p.dns = nil
		return err
	}
	p.dnsDone = make(chan error, 1)
	cmd, done := p.dns, p.dnsDone
	go func() { done <- cmd.Wait() }()
	select {
	case exit := <-done:
		done <- exit
		return fmt.Errorf("Wendy Net DNS proxy failed to start: %v", exit)
	case <-time.After(150 * time.Millisecond):
	}
	p.sharing, p.upstream, p.sharing6 = iface, upstream, share6
	return nil
}

func (p *linuxTunnelPolicy) stopSharingLocked(ctx context.Context) error {
	// Refuse new grants before tearing down any old grants.
	p.sharing, p.upstream, p.sharing6 = "", "", false
	var errs []error
	for _, grant := range p.grants {
		errs = append(errs, p.revokeLocked(ctx, grant))
	}
	if len(p.grants) == 0 {
		errs = append(errs, p.donorRouting.close())
	}
	if p.dns != nil {
		_ = p.dns.Process.Kill()
		<-p.dnsDone
		p.dns, p.dnsDone = nil, nil
	}
	if p.forwarding6 && len(p.grants) == 0 {
		if err := hostnetwork.ReleaseIPv6Forwarding("mesh-internet"); err != nil {
			return errors.Join(errors.Join(errs...), err)
		}
		p.forwarding6 = false
	}
	errs = append(errs, removeInternetRules(ctx, &p.shareRules))
	return errors.Join(errs...)
}

func (p *linuxTunnelPolicy) Grant(ctx context.Context, peer meshid.Device) (lease tunnelLease, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return lease, err
	}
	if p.closed || p.sharing == "" {
		return lease, errors.New("gateway is not sharing an uplink")
	}
	if len(p.grants) >= maxLeaseConnections {
		return lease, errors.New("gateway lease capacity reached")
	}
	if !peer.IsValid() || peer == p.credentials.Device || peer.Tenant() != p.credentials.Device.Tenant() {
		return lease, errors.New("invalid gateway borrower")
	}
	lease, err = p.pool.allocate(peer)
	if err != nil {
		return lease, err
	}
	lease.IPv6 = p.sharing6
	grant := &grantedTunnel{peer: peer, lease: lease, revoked: make(chan struct{})}
	p.grants[lease.Slot] = grant
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = errors.Join(err, p.revokeLocked(cleanup, grant))
		}
	}()
	grant.tunnel, err = newInternetTunnel(fmt.Sprintf("%s%x", tunnelPrefix, lease.Slot), p.credentials.Device, peer)
	if err != nil {
		return lease, err
	}
	if err = grant.tunnel.route(lease.address4(), false); err != nil {
		return lease, err
	}
	if lease.IPv6 {
		if err = grant.tunnel.route(lease.address6(), false); err != nil {
			return lease, err
		}
	}
	if err = addDonorLeaseRules(grant.tunnel.link.Name, lease, &grant.routingRules); err != nil {
		return lease, err
	}
	err = grantTunnelEgress(ctx, &grant.rules, grant.tunnel.link.Name, p.sharing, lease)
	return lease, err
}

func (p *linuxTunnelPolicy) revokeLocked(ctx context.Context, grant *grantedTunnel) error {
	grant.revokeOnce.Do(func() { close(grant.revoked) })
	// Delete the tunnel first, so new packets cannot race conntrack flushing.
	if grant.tunnel != nil {
		if err := grant.tunnel.close(); err != nil {
			// Even if a link was changed externally, withdraw our forwarding
			// grants. Keep the slot and ownership record for a later retry.
			return errors.Join(err, removeInternetRules(ctx, &grant.rules))
		}
		grant.tunnel = nil
	}
	if err := removeDonorLeaseRules(&grant.routingRules); err != nil {
		return errors.Join(err, removeInternetRules(ctx, &grant.rules))
	}
	if err := removeInternetRules(ctx, &grant.rules); err != nil {
		return err
	}
	if err := flushLease(grant.lease); err != nil {
		return err
	}
	delete(p.grants, grant.lease.Slot)
	p.pool.release(grant.peer, grant.lease)
	return nil
}

func (p *linuxTunnelPolicy) Revoke(ctx context.Context, peer meshid.Device, lease tunnelLease) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	grant := p.grants[lease.Slot]
	if grant == nil || grant.peer != peer || grant.lease != lease {
		return nil
	}
	return p.revokeLocked(ctx, grant)
}

func (p *linuxTunnelPolicy) LeaseActive(peer meshid.Device, lease tunnelLease) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	grant := p.grants[lease.Slot]
	return !p.closed && p.sharing != "" && grant != nil && grant.peer == peer && grant.lease == lease && grant.tunnel != nil
}

func (p *linuxTunnelPolicy) LeaseDone(peer meshid.Device, lease tunnelLease) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	grant := p.grants[lease.Slot]
	if grant != nil && grant.peer == peer && grant.lease == lease {
		return grant.revoked
	}
	done := make(chan struct{})
	close(done)
	return done
}

func (p *linuxTunnelPolicy) SetGateway(ctx context.Context, peer meshid.Device) (err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("Wendy Net Internet policy closed")
	}
	if p.borrowed != nil && p.borrowed.peer == peer {
		select {
		case <-p.borrowed.done:
		default:
			return nil
		}
	}
	if !peer.IsValid() {
		// Withdrawal also abandons a negotiation that is still in flight.
		p.dialer.stop()
	}
	if err = p.stopBorrowingLocked(ctx); err != nil {
		return err
	}
	if !peer.IsValid() {
		return nil
	}
	if p.sharing != "" || len(p.grants) != 0 {
		return errors.New("cannot borrow Internet while sharing an uplink")
	}
	control, lease, err := p.dialer.await(ctx, peer)
	if err != nil {
		return err
	}
	b := &borrowedTunnel{grantedTunnel: grantedTunnel{peer: peer, lease: lease}, control: control, done: make(chan struct{}), stop: make(chan struct{})}
	p.borrowed = b
	defer func() {
		if err != nil {
			close(b.done)
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = errors.Join(err, p.stopBorrowingLocked(cleanup))
		}
	}()
	b.tunnel, err = newInternetTunnel(tunnelPrefix+"b", p.credentials.Device, peer)
	if err != nil {
		return err
	}
	for _, address := range []netip.Addr{lease.address4(), lease.address6()} {
		if address.Is6() && !lease.IPv6 {
			continue
		}
		if err = b.tunnel.assign(address); err != nil {
			return err
		}
		family := "iptables"
		if address.Is6() {
			family = "ip6tables"
		}
		if err = addInternetRule(ctx, &b.rules, family, "nat", "POSTROUTING", "-o", b.tunnel.link.Name, "-j", "MASQUERADE"); err != nil {
			return err
		}
		if err = addInternetRule(ctx, &b.rules, family, "filter", "INPUT", "-i", b.tunnel.link.Name, "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"); err != nil {
			return err
		}
		// Containers are IPv4-only. Their existing explicit CNI Internet
		// grants are the only forwarding grants on the borrower.
		if address.Is4() {
			for _, direction := range []string{"-i", "-o"} {
				if err = addInternetRule(ctx, &b.rules, family, "mangle", "FORWARD", direction, b.tunnel.link.Name, "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"); err != nil {
					return err
				}
				if err = addInternetRule(ctx, &b.rules, family, "filter", "FORWARD", direction, b.tunnel.link.Name, "-j", hostnetwork.BridgeInternetChain); err != nil {
					return err
				}
			}
		}
		if err = b.tunnel.route(address, true); err != nil {
			return err
		}
	}
	p.borrowers.Add(1)
	go p.watchBorrowed(b)
	return nil
}

func (p *linuxTunnelPolicy) watchBorrowed(b *borrowedTunnel) {
	defer p.borrowers.Done()
	defer close(b.done)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		ticker := time.NewTicker(leaseHeartbeatInterval)
		defer ticker.Stop()
		for {
			if err := b.control.SetWriteDeadline(time.Now().Add(leaseHeartbeatTimeout)); err != nil {
				_ = b.control.Close()
				return
			}
			if _, err := b.control.Write([]byte{leaseHeartbeat}); err != nil {
				_ = b.control.Close()
				return
			}
			select {
			case <-b.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	defer func() { b.stopOnce.Do(func() { close(b.stop) }); _ = b.control.Close(); <-writerDone }()
	for {
		if err := b.control.SetReadDeadline(time.Now().Add(leaseHeartbeatTimeout)); err != nil {
			break
		}
		var ack [1]byte
		if _, err := io.ReadFull(b.control, ack[:]); err != nil || ack[0] != leaseHeartbeat {
			break
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.borrowed == b {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = p.stopBorrowingLocked(ctx)
	}
}

func (p *linuxTunnelPolicy) stopBorrowingLocked(ctx context.Context) error {
	b := p.borrowed
	if b == nil {
		return nil
	}
	b.stopOnce.Do(func() { close(b.stop) })
	_ = b.control.Close()
	if b.tunnel != nil {
		if err := b.tunnel.close(); err != nil {
			return errors.Join(err, removeInternetRules(ctx, &b.rules))
		}
		b.tunnel = nil
	}
	if err := removeInternetRules(ctx, &b.rules); err != nil {
		return err
	}
	if err := flushLease(b.lease); err != nil {
		return err
	}
	p.borrowed = nil
	return nil
}

func (p *linuxTunnelPolicy) Close() error {
	p.mu.Lock()
	p.closed = true
	server := p.server
	p.server = nil
	p.mu.Unlock()
	var errs []error
	if server != nil {
		// The drained server can report a historical lease-cleanup failure.
		// Keep that error for this attempt; subsequent Close calls retry the
		// retained grants below instead of replaying the same stale error.
		errs = append(errs, server.Close())
	}
	p.mu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p.dialer.stop()
	errs = append(errs, p.stopBorrowingLocked(ctx), p.stopSharingLocked(ctx), p.resolver.Close())
	if p.borrowed == nil && len(p.grants) == 0 && !p.forwarding6 {
		errs = append(errs, removeInternetRules(ctx, &p.base), hostnetwork.ReleaseForwarding("mesh-internet"))
	}
	p.mu.Unlock()
	p.borrowers.Wait()
	return errors.Join(errs...)
}
