package operatormesh

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshcatalog"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsharing"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// InternetGateway requires the reserved offer in a live, route-authorized
// manifest. A user-selected device alone does not authorize internet sharing.
func InternetGateway(view localmesh.NodeSnapshot, gateway meshid.Device, now time.Time) bool {
	if gateway.IsOperator() || !gateway.IsValid() || gateway.Tenant() != view.Self.Tenant() {
		return false
	}
	for _, manifest := range view.Devices {
		if manifest.Owner != nil || manifest.Device != gateway.UUID() || manifest.Withdraw || manifest.Expires <= now.UnixMilli() || manifest.Route == nil || !view.RouteAuthorizations[*manifest.Route] {
			continue
		}
		routed := false
		for _, route := range view.Routes {
			if !route.Local && !route.Unreachable && route.Link != 0 && route.Prefix == netip.PrefixFrom(gateway.MeshAddress(), 128) && route.Prefix == manifest.Route.Prefix && route.RouterID == manifest.Route.RouterID && route.Seqno == manifest.Route.Seqno {
				routed = true
				break
			}
		}
		if !routed {
			continue
		}
		for _, service := range manifest.Services {
			if service.AppID == meshcatalog.GatewayAppID && service.ServiceID == meshcatalog.GatewayServiceID {
				return true
			}
		}
	}
	return false
}

// OpenInternet retains the authenticated lease and transports framed inner IP
// packets. Closing the stream revokes the lease. No host-wide routes are changed.
func (s *Session) OpenInternet(setup context.Context, gateway meshid.Device) (net.Conn, error) {
	// A newly launched mobile extension may still be receiving the initial
	// signed directory. Allow that bounded convergence before refusing the offer.
	wait, cancelWait := context.WithTimeout(setup, 15*time.Second)
	defer cancelWait()
	timer := time.NewTicker(200 * time.Millisecond)
	defer timer.Stop()
	for !InternetGateway(s.Snapshot(), gateway, time.Now()) {
		select {
		case <-wait.Done():
			return nil, errors.New("selected mesh device has no reachable signed internet uplink")
		case <-timer.C:
		}
	}
	raw, err := s.Endpoint.DialContext(setup, net.JoinHostPort(gateway.MeshAddress().String(), "18867"))
	if err != nil {
		return nil, err
	}
	control, config, err := meshsharing.Borrow(setup, s.Node.Credentials, gateway, raw)
	if err != nil {
		return nil, err
	}
	tunnel, err := s.Endpoint.OpenInternetTunnel(gateway.MeshAddress(), config)
	if err != nil {
		control.Close()
		return nil, err
	}
	local, remote := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { defer cancel(); _ = meshsharing.KeepLease(ctx, control) }()
	go func() { <-ctx.Done(); remote.Close(); control.Close(); tunnel.Close() }()
	go func() {
		defer cancel()
		header, _ := json.Marshal(config)
		if meshsharing.WriteFrame(remote, header) != nil {
			return
		}
		for {
			packet, err := tunnel.ReadPacket(ctx)
			if err != nil {
				return
			}
			if meshsharing.WriteFrame(remote, packet) != nil {
				return
			}
		}
	}()
	go func() {
		defer cancel()
		for {
			packet, err := meshsharing.ReadFrame(remote)
			if err != nil {
				return
			}
			if tunnel.WritePacket(packet) != nil {
				return
			}
		}
	}()
	// Withdrawal is checked independently of the control heartbeat: a donor may
	// disappear from the signed directory while an old route/socket survives.
	go func() {
		defer cancel()
		timer := time.NewTicker(time.Second)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				if !InternetGateway(s.Snapshot(), gateway, time.Now()) {
					return
				}
			}
		}
	}()
	return local, nil
}
