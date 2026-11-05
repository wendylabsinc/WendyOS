package carriers

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
)

// Gadget links have a stable IPv6 link-local endpoint even without DHCP or
// mDNS. The interface zone keeps separate USB devices distinct. As with BLE,
// this is only a hint; connectCandidate authenticates the enrolled TLS identity.
func usbCandidates(interfaces []discovery.USBDirectCandidate) []Candidate {
	out := make([]Candidate, 0, len(interfaces))
	for _, iface := range interfaces {
		if iface.Interface == "" || iface.Zone == "" {
			continue
		}
		out = append(out, Candidate{Transport: TCP, Address: iface.HostPort(localmesh.TLSLANPort), InterfaceName: iface.Interface, Cost: localmesh.EthernetLinkCost})
		if len(out) >= 64 {
			break
		}
	}
	return out
}

func usbSource() Source {
	return func(ctx context.Context, emit func([]Candidate)) error {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			emit(usbCandidates(discovery.USBDirectCandidates()))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
	}
}
