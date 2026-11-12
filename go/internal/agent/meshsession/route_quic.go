package meshsession

import (
	"net/netip"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func quicConfigForRoute(snapshot localmesh.NodeSnapshot, peer meshid.Device, now time.Time) *quic.Config {
	config := quicConfig()
	// Saved snapshots cannot justify optimistic startup after route changes.
	age := now.Sub(snapshot.CapturedAt)
	if snapshot.CapturedAt.IsZero() || age < 0 || age > time.Second {
		return config
	}
	prefix := netip.PrefixFrom(peer.MeshAddress(), 128)
	for _, route := range snapshot.ForwardingRoutes {
		if route.Prefix != prefix || route.Local || route.Unreachable || route.Metric == babel.Infinity {
			continue
		}
		for _, link := range snapshot.Links {
			// A LAN first hop to another device says nothing about downstream BLE.
			if link.ID != route.Link || link.Device != peer || !link.ForwardingReady || link.Retiring || !link.DirectLAN() {
				continue
			}
			config.InitialRTT = 100 * time.Millisecond
			config.InitialCongestionWindowPackets = 10
			return config
		}
	}
	return config
}

// Keep TCP and UDP startup selection behind the same current-route check.
func routeQUICConfig(peer meshid.Device, address netip.AddrPort, snapshot func() localmesh.NodeSnapshot) func() (*quic.Config, error) {
	return func() (*quic.Config, error) {
		if snapshot == nil {
			return nil, ErrNoRoute
		}
		view := snapshot()
		endpoint, err := ResolveApp(view, peer)
		if err != nil {
			return nil, err
		}
		if endpoint.Transport != localmesh.AppQUIC || endpoint.Address != address {
			return nil, ErrNoRoute
		}
		return quicConfigForRoute(view, peer, time.Now()), nil
	}
}
