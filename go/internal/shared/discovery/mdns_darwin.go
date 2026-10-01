//go:build darwin

package discovery

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
)

var routeInterfaceForMDNSAddressFn = routeInterfaceForMDNSAddress

// resolveMDNSService resolves a browse result into an MDNSService. Its only
// caller, mdnsStreamResolveAndEmit, invokes it (via the resolveServiceFn
// seam) off mdnsStreamBackend's resolver-pool queue, so a slow lookup only
// holds up its own worker rather than the browse callback itself; the ctx it
// bounds the lookup with is that worker's own.
func resolveMDNSService(ctx context.Context, inst browseResult, serviceType string) (MDNSService, error) {
	hostname, port, txtRecords, err := dnssdResolveInstance(ctx, inst, serviceType)
	if err != nil {
		return MDNSService{}, err
	}

	ipAddr := ""
	if addrs, lookupErr := dnssdResolveAddresses(ctx, hostname, inst); lookupErr == nil {
		ipAddr = preferInterfaceRoutedAddr(addrs, inst.interfaceName)
	}

	return MDNSService{
		InstanceName: inst.instanceName,
		Hostname:     hostname,
		IPAddress:    ipAddr,
		Port:         port,
		TXTRecords:   txtRecords,
	}, nil
}

// preferInterfaceRoutedAddr chooses an address whose kernel route still uses
// the interface that produced the DNS-SD answer. This matters for direct links
// whose device-side IPv4 subnet overlaps a broader default route: mDNSResponder
// can correctly return 192.168.123.x on Ethernet while an ordinary dial to it
// would still leave through Wi-Fi. If no route maps exactly (a physical member
// may be represented by its bridge), prefer non-link-local IPv6; its advertised
// on-link prefix retains the interface scope and avoids the overlapping IPv4
// route.
func preferInterfaceRoutedAddr(addrs []string, interfaceName string) string {
	var exact []string
	for _, addr := range addrs {
		if routeInterfaceForMDNSAddressFn(addr) == interfaceName {
			exact = append(exact, addr)
		}
	}
	if len(exact) > 0 {
		return preferIPv4Addr(exact)
	}
	for _, addr := range addrs {
		ip := net.ParseIP(strings.SplitN(addr, "%", 2)[0])
		if ip != nil && ip.To4() == nil && !ip.IsLinkLocalUnicast() {
			return addr
		}
	}
	return preferIPv4Addr(addrs)
}

func routeInterfaceForMDNSAddress(raw string) string {
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	if addr.Zone() != "" {
		return addr.Zone()
	}
	remote := &net.UDPAddr{IP: net.ParseIP(addr.String()), Port: 9}
	conn, err := net.DialUDP("udp", nil, remote)
	if err != nil {
		return ""
	}
	localIP := conn.LocalAddr().(*net.UDPAddr).IP
	_ = conn.Close()
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		ifaceAddrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, candidate := range ifaceAddrs {
			ip, _, err := net.ParseCIDR(candidate.String())
			if err == nil && ip.Equal(localIP) {
				return iface.Name
			}
		}
	}
	return ""
}

// resolveWorkers bounds how many browse results mdnsStreamBackend resolves
// concurrently. A var, not a const, so tests can shrink it.
var resolveWorkers = 4

// mdnsStreamJobBuffer bounds how many pending browse results mdnsStreamBackend
// queues for its resolver pool. The browse callback only ever blocks on this
// channel filling up, never on a resolve itself.
const mdnsStreamJobBuffer = 32

// mdnsStreamBackend is the darwin implementation of the lanBackendFn and
// browseBackendFn seams (stream.go, mdns.go): it browses serviceType via
// mDNSResponder and resolves each answer on a small worker pool instead of
// inline in the browse callback — resolving inline would stall the socket
// pump for every other device on the network while a resolve runs up to its
// timeout.
//
// Every browse "Add" is queued and resolved, even a repeat of an instance
// already emitted: unlike those picker-facing continuous browses, this
// backend keeps no de-dup set of its own. mDNSResponder re-fires Add on
// ordinary answer refresh even when nothing changed, and the stream engine
// (lanStream.handleSighting) is what already decides whether a re-resolve
// changed anything — so a device that moves address without ever going
// offline is still caught, which a permanent de-dup set here would prevent.
//
// It returns nil once ctx ends (dnssdBrowseStream maps cancellation to nil)
// or a non-nil error if mDNSResponder itself misbehaves, restartable by the
// engine. Either way, every resolver goroutine has exited before it returns.
func mdnsStreamBackend(ctx context.Context, serviceType string, emit func(MDNSService)) error {
	logMDNSBackend("dns_sd") // WENDY_MDNS_DEBUG: which backend is running
	jobs := make(chan browseResult, mdnsStreamJobBuffer)

	var wg sync.WaitGroup
	for i := 0; i < resolveWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case inst, ok := <-jobs:
					if !ok {
						return
					}
					mdnsStreamResolveAndEmit(ctx, inst, serviceType, emit)
				}
			}
		}()
	}

	err := dnssdBrowseStream(ctx, serviceType, func(inst browseResult) {
		select {
		case jobs <- inst:
		case <-ctx.Done():
		}
	})

	// Closing jobs lets workers finish a naturally-ended browse. On cancellation
	// their ctx arm wins and intentionally abandons queued refreshes; draining a
	// full queue could otherwise multiply shutdown latency by resolve timeout.
	close(jobs)
	wg.Wait()
	return err
}

// resolveServiceFn is the resolve step mdnsStreamResolveAndEmit calls. A var,
// not a direct call to resolveMDNSService, so tests can force a resolve
// failure without depending on real dns-sd failure conditions.
var resolveServiceFn = resolveMDNSService

// mdnsStreamResolveAndEmit emits only resolved services. A browse result can
// come from mDNSResponder's cache after the device has left the network;
// inventing a hostname when resolution fails would report it as live and
// refresh its entry in Wendy's cache on every scan.
func mdnsStreamResolveAndEmit(ctx context.Context, inst browseResult, serviceType string, emit func(MDNSService)) {
	resolveCtx, cancel := context.WithTimeout(ctx, dnssdResolveTimeout)
	defer cancel()

	svc, err := resolveServiceFn(resolveCtx, inst, serviceType)
	if err != nil {
		return
	}
	svc.InterfaceName = inst.interfaceName
	emit(svc)
}
