//go:build linux || darwin

package netmesh

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Ordinary agent DNS-SD records can cross VLANs through a reflector even when
// the native mesh carrier's link-local query cannot. These are dial hints only.
// A fresh bounded snapshot drops disappeared advertisements and is never used
// as a device roster or a substitute for the peer's TLS identity.
type lanHints struct {
	mu    sync.Mutex
	peers []localmesh.LANPeer
}

func (h *lanHints) snapshot() []localmesh.LANPeer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]localmesh.LANPeer(nil), h.peers...)
}

func operatorHints(services []discovery.MDNSService, self meshid.Device) []localmesh.LANPeer {
	var peers []localmesh.LANPeer
	seen := map[string]bool{}
	for _, svc := range services {
		peer, err := meshid.DeviceFromPrincipal(svc.TXTRecords["principal"])
		ip := net.ParseIP(svc.IPAddress).To4()
		if err != nil || peer.Tenant() != self.Tenant() || ip == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
			continue
		}
		key := peer.Principal() + "/" + ip.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		peers = append(peers, localmesh.LANPeer{Principal: peer.Principal(), IP: ip})
		if len(peers) == 128 {
			break
		}
	}
	return peers
}

func (h *lanHints) run(ctx context.Context, self meshid.Device) {
	for ctx.Err() == nil {
		services, _ := discovery.BrowseMDNSServices(ctx, "_wendyos._udp", 3*time.Second)
		h.mu.Lock()
		h.peers = operatorHints(services, self)
		h.mu.Unlock()
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
