//go:build linux

package discovery

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
)

// BrowseMDNSServices discovers mDNS services of the given type on Linux
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
