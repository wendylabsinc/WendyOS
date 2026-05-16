//go:build linux

package discovery

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
)

// BrowseMDNSServices discovers mDNS services of the given type on Linux.
// Prefers avahi-browse when available; falls back to hashicorp/mdns with
// per-interface queries.
func BrowseMDNSServices(ctx context.Context, serviceType string, timeout time.Duration) ([]MDNSService, error) {
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if hasAvahiBrowse() {
		return browseMDNSAvahi(ctx, serviceType, timeout)
	}
	return browseMDNSHashicorp(ctx, serviceType, timeout)
}

// browseMDNSAvahi uses avahi-browse to discover services.
func browseMDNSAvahi(ctx context.Context, serviceType string, timeout time.Duration) ([]MDNSService, error) {
	browseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(browseCtx, "avahi-browse", "-rptl", serviceType)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("avahi-browse: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting avahi-browse: %w", err)
	}

	var services []MDNSService
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		svc, ok := parseAvahiMDNSService(scanner.Text())
		if !ok {
			continue
		}
		key := fmt.Sprintf("%s-%s-%d", svc.InstanceName, svc.Hostname, svc.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		services = append(services, svc)
	}

	scanErr := scanner.Err()
	waitErr := cmd.Wait()

	// If the context timed out, return whatever we collected.
	if browseCtx.Err() == context.DeadlineExceeded {
		return services, nil
	}

	// If avahi-browse failed at runtime (e.g. avahi-daemon not running),
	// fall back to hashicorp/mdns.
	if scanErr != nil || waitErr != nil {
		return browseMDNSHashicorp(ctx, serviceType, timeout)
	}

	return services, nil
}

// parseAvahiMDNSService parses a resolved entry from avahi-browse -rpt output
// into an MDNSService.
func parseAvahiMDNSService(line string) (MDNSService, bool) {
	if !strings.HasPrefix(line, "=") {
		return MDNSService{}, false
	}

	fields := strings.Split(line, ";")
	if len(fields) < 10 {
		return MDNSService{}, false
	}

	ifaceName := fields[1]
	instanceName := avahiUnescape(fields[3])
	hostname := strings.TrimSuffix(fields[6], ".")
	ipAddr := fields[7]
	port, err := strconv.Atoi(fields[8])
	if err != nil || port < 1 || port > 65535 {
		return MDNSService{}, false
	}

	// IPv6 link-local addresses need a zone ID (%iface) to be routable.
	if addr, err := netip.ParseAddr(ipAddr); err == nil && addr.Is6() && addr.IsLinkLocalUnicast() {
		ipAddr = ipAddr + "%" + ifaceName
	}

	txtRecords := parseAvahiTXT(fields[9])

	return MDNSService{
		InstanceName: instanceName,
		Hostname:     hostname,
		IPAddress:    ipAddr,
		Port:         port,
		TXTRecords:   txtRecords,
	}, true
}

// browseMDNSHashicorp uses hashicorp/mdns as a fallback, querying each
// interface individually.
func browseMDNSHashicorp(ctx context.Context, serviceType string, timeout time.Duration) ([]MDNSService, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing interfaces: %w", err)
	}

	var allServices []MDNSService
	seen := make(map[string]bool)

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		services := queryInterfaceMDNS(ctx, &iface, serviceType, timeout)
		for _, svc := range services {
			key := fmt.Sprintf("%s-%s-%d", svc.InstanceName, svc.Hostname, svc.Port)
			if seen[key] {
				continue
			}
			seen[key] = true
			allServices = append(allServices, svc)
		}
	}

	return allServices, nil
}

// queryInterfaceMDNS runs a single hashicorp/mdns query on a specific interface.
func queryInterfaceMDNS(_ context.Context, iface *net.Interface, serviceType string, timeout time.Duration) []MDNSService {
	entriesCh := make(chan *mdns.ServiceEntry, 16)
	var services []MDNSService
	seen := make(map[string]bool)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for entry := range entriesCh {
			// Filter out entries that don't match the queried service type.
			// hashicorp/mdns can return unrelated mDNS responders.
			if !mdnsEntryMatchesServiceType(entry.Name, serviceType) {
				continue
			}

			hostname := strings.TrimSuffix(entry.Host, ".")

			key := fmt.Sprintf("%s-%s-%d", entry.Name, hostname, entry.Port)
			if seen[key] {
				continue
			}
			seen[key] = true

			ipAddr := ""
			if entry.AddrV4 != nil {
				ipAddr = entry.AddrV4.String()
			} else if entry.AddrV6 != nil {
				ipAddr = entry.AddrV6.String()
				// IPv6 link-local addresses need a zone ID (%iface) to be routable.
				if entry.AddrV6.IsLinkLocalUnicast() {
					ipAddr = ipAddr + "%" + iface.Name
				}
			}

			txtRecords := parseMDNSInfoFields(entry.InfoFields)

			services = append(services, MDNSService{
				InstanceName: entry.Name,
				Hostname:     hostname,
				IPAddress:    ipAddr,
				Port:         entry.Port,
				TXTRecords:   txtRecords,
			})
		}
	}()

	params := mdns.DefaultParams(serviceType)
	params.Interface = iface
	params.Entries = entriesCh
	params.Timeout = timeout
	params.Logger = silentLogger

	_ = mdns.Query(params)
	close(entriesCh)
	<-done

	return services
}
