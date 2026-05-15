//go:build windows

package discovery

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/mdns"
	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// discoverLAN uses hashicorp/mdns to find WendyOS devices on Windows.
func discoverLAN(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
	if timeout == 0 {
		timeout = defaultTimeout
	}

	queries := []*net.Interface{nil} // Keep the existing default/all-interface query.
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if !isMDNSInterfaceEligible(iface) {
				continue
			}
			iface := iface
			queries = append(queries, &iface)
		}
	}

	resultsCh := make(chan []models.LANDevice, len(queries))
	var wg sync.WaitGroup
	for _, iface := range queries {
		wg.Add(1)
		go func(iface *net.Interface) {
			defer wg.Done()
			resultsCh <- queryLANMDNS(ctx, iface, timeout)
		}(iface)
	}

	wg.Wait()
	close(resultsCh)

	var candidates []models.LANDevice
	for devices := range resultsCh {
		candidates = append(candidates, devices...)
	}

	return deduplicateLANDevices(candidates), nil
}

func isMDNSInterfaceEligible(iface net.Interface) bool {
	if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
		return false
	}
	return iface.Flags&net.FlagLoopback == 0
}

// queryLANMDNS runs a single mDNS query. If iface is nil, hashicorp/mdns uses
// its default all-interface behavior. Otherwise, the query is scoped to iface.
func queryLANMDNS(ctx context.Context, iface *net.Interface, timeout time.Duration) []models.LANDevice {
	if ctx.Err() != nil {
		return nil
	}

	entriesCh := make(chan *mdns.ServiceEntry, 16)
	var devices []models.LANDevice

	done := make(chan struct{})
	go func() {
		defer close(done)
		seen := make(map[string]bool)
		for entry := range entriesCh {
			dev, ok := lanDeviceFromMDNSEntry(entry, iface)
			if !ok {
				continue
			}

			key := fmt.Sprintf("%s-%s-%d", entry.Name, dev.Hostname, dev.Port)
			if seen[key] {
				continue
			}
			seen[key] = true

			devices = append(devices, dev)
		}
	}()

	params := mdns.DefaultParams(wendyServiceType)
	params.Interface = iface
	params.Entries = entriesCh
	params.Timeout = timeout
	params.Logger = silentLogger

	_ = mdns.Query(params)
	close(entriesCh)
	<-done

	return devices
}

func lanDeviceFromMDNSEntry(entry *mdns.ServiceEntry, iface *net.Interface) (models.LANDevice, bool) {
	if entry == nil || !mdnsEntryMatchesServiceType(entry.Name, wendyServiceType) {
		return models.LANDevice{}, false
	}

	hostname := strings.TrimSuffix(entry.Host, ".")
	txtRecords := parseMDNSTXTRecords(entry.InfoFields)

	displayName := strings.TrimSuffix(hostname, ".local")
	if dn, ok := txtRecords["displayname"]; ok {
		displayName = dn
	}

	id := ""
	if v, ok := txtRecords["wendyosdevice"]; ok {
		id = v
	} else if v, ok := txtRecords["id"]; ok {
		id = v
	}
	if id == "" {
		id = displayName
	}

	return models.LANDevice{
		ID:            id,
		DisplayName:   displayName,
		Hostname:      hostname,
		IPAddress:     mdnsEntryIPAddress(entry, iface),
		Port:          entry.Port,
		IsMTLS:        txtRecords["tls"] == "true",
		InterfaceType: string(models.InterfaceLAN),
		IsWendyDevice: true,
	}, true
}

func parseMDNSTXTRecords(fields []string) map[string]string {
	records := make(map[string]string)
	for _, txt := range fields {
		if k, v, ok := strings.Cut(txt, "="); ok {
			records[k] = v
		}
	}
	return records
}

func mdnsEntryIPAddress(entry *mdns.ServiceEntry, iface *net.Interface) string {
	if entry.AddrV4 != nil {
		return entry.AddrV4.String()
	}
	if entry.AddrV6 == nil {
		return ""
	}

	ipAddr := entry.AddrV6.String()
	if iface != nil && entry.AddrV6.IsLinkLocalUnicast() && !strings.Contains(ipAddr, "%") {
		ipAddr += "%" + iface.Name
	}
	return ipAddr
}

func deduplicateLANDevices(devices []models.LANDevice) []models.LANDevice {
	if len(devices) < 2 {
		return devices
	}

	deduped := make([]models.LANDevice, 0, len(devices))
	indexes := make(map[string]int)

	for _, dev := range devices {
		key := lanDeviceDedupKey(dev)
		if idx, ok := indexes[key]; ok {
			if preferLANDevice(dev, deduped[idx]) {
				deduped[idx] = dev
			}
			continue
		}

		indexes[key] = len(deduped)
		deduped = append(deduped, dev)
	}

	return deduped
}

func lanDeviceDedupKey(dev models.LANDevice) string {
	if dev.ID != "" {
		return "id:" + dev.ID
	}
	if dev.Hostname != "" {
		return fmt.Sprintf("host:%s:%d", dev.Hostname, dev.Port)
	}
	return fmt.Sprintf("fallback:%s:%s:%d", dev.DisplayName, dev.Hostname, dev.Port)
}

func preferLANDevice(candidate, existing models.LANDevice) bool {
	if existing.IPAddress == "" && candidate.IPAddress != "" {
		return true
	}
	if existing.IPAddress != "" && candidate.IPAddress == "" {
		return false
	}

	candidateIsIPv4 := isIPv4Address(candidate.IPAddress)
	existingIsIPv4 := isIPv4Address(existing.IPAddress)
	if candidateIsIPv4 != existingIsIPv4 {
		return candidateIsIPv4
	}

	if hasScopedLinkLocalIPv6(candidate.IPAddress) && hasUnscopedLinkLocalIPv6(existing.IPAddress) {
		return true
	}

	return lanDeviceMetadataScore(candidate) > lanDeviceMetadataScore(existing)
}

func lanDeviceMetadataScore(dev models.LANDevice) int {
	score := 0
	if dev.ID != "" {
		score++
	}
	if dev.DisplayName != "" {
		score++
	}
	if dev.Hostname != "" {
		score++
	}
	if dev.Port != 0 {
		score++
	}
	if dev.IsMTLS {
		score++
	}
	return score
}

func isIPv4Address(ipAddr string) bool {
	ip := net.ParseIP(stripIPv6Zone(ipAddr))
	return ip != nil && ip.To4() != nil
}

func hasScopedLinkLocalIPv6(ipAddr string) bool {
	base, zone, _ := strings.Cut(ipAddr, "%")
	return zone != "" && isLinkLocalIPv6(base)
}

func hasUnscopedLinkLocalIPv6(ipAddr string) bool {
	base, zone, _ := strings.Cut(ipAddr, "%")
	return zone == "" && isLinkLocalIPv6(base)
}

func isLinkLocalIPv6(ipAddr string) bool {
	ip := net.ParseIP(ipAddr)
	return ip != nil && ip.To4() == nil && ip.IsLinkLocalUnicast()
}

func stripIPv6Zone(ipAddr string) string {
	base, _, _ := strings.Cut(ipAddr, "%")
	return base
}

// discoverLANContinuous periodically queries mDNS and sends newly discovered
// devices to ch. Runs until ctx is cancelled.
func discoverLANContinuous(ctx context.Context, ch chan<- models.LANDevice) {
	defer close(ch)
	seen := make(map[string]bool)

	for {
		devices, _ := discoverLAN(ctx, 3*time.Second)
		for _, dev := range devices {
			key := fmt.Sprintf("%s-%s-%d", dev.DisplayName, dev.Hostname, dev.Port)
			if seen[key] {
				continue
			}
			seen[key] = true
			select {
			case ch <- dev:
			case <-ctx.Done():
				return
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// BrowseMDNSServices discovers mDNS services of the given type on Windows
// using hashicorp/mdns. Returns all services found within the timeout.
func BrowseMDNSServices(ctx context.Context, serviceType string, timeout time.Duration) ([]MDNSService, error) {
	if timeout == 0 {
		timeout = defaultTimeout
	}

	entriesCh := make(chan *mdns.ServiceEntry, 16)
	var services []MDNSService

	done := make(chan struct{})
	go func() {
		defer close(done)
		seen := make(map[string]bool)
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
			}

			txtRecords := make(map[string]string)
			for _, txt := range entry.InfoFields {
				if k, v, ok := strings.Cut(txt, "="); ok {
					txtRecords[k] = v
				}
			}

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
	params.Entries = entriesCh
	params.Timeout = timeout
	params.Logger = silentLogger

	_ = mdns.Query(params)
	close(entriesCh)
	<-done

	_ = ctx // respect context via timeout

	return services, nil
}
