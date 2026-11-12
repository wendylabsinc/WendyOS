package meshsession

import (
	"net"
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

// serverQUICConfig gives an incoming session the same startup profile the
// client would choose: faster only when the client's address has a current
// installed route through a forwarding-ready direct LAN link to that device.
// The Initial's source address is not yet validated, but it selects only
// congestion startup. Replies follow the installed route to that address,
// anti-amplification still bounds what is sent before validation, and TLS
// authenticates the client as before.
func serverQUICConfig(snapshot func() localmesh.NodeSnapshot, remote net.Addr) *quic.Config {
	udp, ok := remote.(*net.UDPAddr)
	if snapshot == nil || !ok {
		return quicConfig()
	}
	address, ok := netip.AddrFromSlice(udp.IP)
	if !ok {
		return quicConfig()
	}
	address = address.Unmap()
	view := snapshot()
	// Read the clock after the view, as the dialing side does: a refresh
	// between the two would otherwise look like a snapshot from the future.
	now := time.Now()
	for _, link := range view.Links {
		if link.Device.IsValid() && link.Device.MeshAddress() == address {
			return quicConfigForRoute(view, link.Device, now)
		}
	}
	return quicConfig()
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
