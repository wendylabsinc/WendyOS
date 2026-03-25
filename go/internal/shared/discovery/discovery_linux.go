//go:build linux

package discovery

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/mdns"
	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// hasAvahiBrowse reports whether avahi-browse is available on the system.
// The result is cached after the first call.
var hasAvahiBrowse = sync.OnceValue(func() bool {
	_, err := exec.LookPath("avahi-browse")
	return err == nil
})

// discoverLAN finds WendyOS devices via mDNS on Linux.
// Prefers avahi-browse when available (works across all interfaces);
// falls back to hashicorp/mdns with per-interface queries.
func discoverLAN(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
	if timeout == 0 {
		timeout = defaultTimeout
	}
	if hasAvahiBrowse() {
		return discoverLANAvahi(ctx, timeout)
	}
	return discoverLANMDNS(ctx, timeout)
}

// discoverLANAvahi uses avahi-browse to find WendyOS devices.
// avahi-browse delegates to the Avahi daemon which maintains persistent
// multicast group membership across all network interfaces, making it
// reliable on USB-OTG and other non-default-route interfaces.
func discoverLANAvahi(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
	browseCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// -r: resolve, -p: parsable output, -t: terminate when done, -l: ignore local
	cmd := exec.CommandContext(browseCtx, "avahi-browse", "-rptl", wendyServiceType)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("avahi-browse: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting avahi-browse: %w", err)
	}

	var devices []models.LANDevice
	seen := make(map[string]bool)

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		dev, ok := parseAvahiResolveLine(scanner.Text())
		if !ok {
			continue
		}
		key := fmt.Sprintf("%s-%s-%d", dev.DisplayName, dev.Hostname, dev.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		devices = append(devices, dev)
	}

	// avahi-browse exits on its own with -t; ignore context-cancelled errors.
	_ = cmd.Wait()
	return devices, nil
}

// parseAvahiResolveLine parses a resolved entry from avahi-browse -rpt output.
// Format: =;iface;protocol;name;type;domain;hostname;address;port;txt
func parseAvahiResolveLine(line string) (models.LANDevice, bool) {
	if !strings.HasPrefix(line, "=") {
		return models.LANDevice{}, false
	}

	fields := strings.Split(line, ";")
	if len(fields) < 10 {
		return models.LANDevice{}, false
	}

	// Unescape avahi's \NNN octal sequences (e.g. \032 for space).
	instanceName := avahiUnescape(fields[3])
	hostname := strings.TrimSuffix(fields[6], ".")
	ipAddr := fields[7]
	port, _ := strconv.Atoi(fields[8])

	// Parse TXT records from the remaining field.
	txtRecords := parseAvahiTXT(fields[9])

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
		id = instanceName
	}

	return models.LANDevice{
		ID:            id,
		DisplayName:   displayName,
		Hostname:      hostname,
		IPAddress:     ipAddr,
		Port:          port,
		InterfaceType: string(models.InterfaceLAN),
		IsWendyDevice: true,
	}, true
}

// avahiUnescape replaces avahi's \NNN decimal escape sequences with the
// corresponding characters (e.g. \032 = space).
func avahiUnescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 10, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// parseAvahiTXT parses avahi's TXT record field.
// Format: "key1=val1" "key2=val2" ...
func parseAvahiTXT(field string) map[string]string {
	records := make(map[string]string)
	// Split on `" "` to preserve spaces within values.
	for _, part := range strings.Split(field, "\" \"") {
		part = strings.Trim(part, "\"")
		if k, v, ok := strings.Cut(part, "="); ok {
			records[k] = v
		}
	}
	return records
}

// discoverLANMDNS uses hashicorp/mdns as a fallback when avahi-browse is not
// available. It queries on each network interface individually to ensure
// non-default-route interfaces (like USB-OTG) are covered.
func discoverLANMDNS(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("listing interfaces: %w", err)
	}

	var allDevices []models.LANDevice
	seen := make(map[string]bool)

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
			continue
		}
		// Skip loopback.
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}

		devices := queryInterface(ctx, &iface, timeout)
		for _, dev := range devices {
			key := fmt.Sprintf("%s-%s-%d", dev.DisplayName, dev.Hostname, dev.Port)
			if seen[key] {
				continue
			}
			seen[key] = true
			allDevices = append(allDevices, dev)
		}
	}

	return allDevices, nil
}

// queryInterface runs a single mDNS query on a specific network interface.
func queryInterface(ctx context.Context, iface *net.Interface, timeout time.Duration) []models.LANDevice {
	entriesCh := make(chan *mdns.ServiceEntry, 16)
	var devices []models.LANDevice

	done := make(chan struct{})
	go func() {
		defer close(done)
		for entry := range entriesCh {
			// Filter out entries that don't match the queried service type.
			// hashicorp/mdns can return unrelated mDNS responders.
			if !strings.Contains(entry.Name, wendyServiceType) {
				continue
			}

			hostname := strings.TrimSuffix(entry.Host, ".")

			displayName := strings.TrimSuffix(hostname, ".local")

			ipAddr := ""
			if entry.AddrV4 != nil {
				ipAddr = entry.AddrV4.String()
			} else if entry.AddrV6 != nil {
				ipAddr = entry.AddrV6.String()
			}

			id := ""
			for _, txt := range entry.InfoFields {
				if k, v, ok := strings.Cut(txt, "="); ok && k == "id" {
					id = v
				}
			}
			if id == "" {
				id = displayName
			}

			devices = append(devices, models.LANDevice{
				ID:            id,
				DisplayName:   displayName,
				Hostname:      hostname,
				IPAddress:     ipAddr,
				Port:          entry.Port,
				InterfaceType: string(models.InterfaceLAN),
				IsWendyDevice: true,
			})
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
