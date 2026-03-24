//go:build windows

package discovery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
	"github.com/wendylabsinc/wendy/internal/shared/models"
)

func discoverEthernet(_ context.Context) ([]models.EthernetInterface, error) {
	return nil, nil
}

// discoverLAN uses hashicorp/mdns to find WendyOS devices on Windows.
func discoverLAN(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error) {
	if timeout == 0 {
		timeout = defaultTimeout
	}

	entriesCh := make(chan *mdns.ServiceEntry, 16)
	var devices []models.LANDevice

	done := make(chan struct{})
	go func() {
		defer close(done)
		seen := make(map[string]bool)
		for entry := range entriesCh {
			// Filter out entries that don't match the queried service type.
			// hashicorp/mdns can return unrelated mDNS responders (e.g. iPhones
			// advertising _remotepairing._tcp).
			if !strings.Contains(entry.Name, wendyServiceType) {
				continue
			}

			hostname := strings.TrimSuffix(entry.Host, ".")

			key := fmt.Sprintf("%s-%s-%d", entry.Name, hostname, entry.Port)
			if seen[key] {
				continue
			}
			seen[key] = true

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

	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	params := mdns.DefaultParams(wendyServiceType)
	params.Entries = entriesCh
	params.Timeout = timeout
	params.Logger = silentLogger

	err := mdns.Query(params)
	close(entriesCh)
	<-done

	if lookupCtx.Err() != nil || err != nil {
		return devices, nil
	}

	return devices, nil
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
			if !strings.Contains(entry.Name, serviceType) {
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
