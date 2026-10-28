package meshsession

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"net/netip"
	"time"
)

var ErrUnsupportedTransport = errors.New("no supported authenticated app session transport")

type AppEndpoint struct {
	Transport string
	Address   netip.AddrPort
}

// ResolveApp uses only installed authenticated routes and the verified device
// manifest. It never derives transport support from unauthenticated discovery
// or retries another protocol after an authentication/connection failure.
func ResolveApp(snapshot localmesh.NodeSnapshot, target meshid.Device) (AppEndpoint, error) {
	base, err := Resolve(snapshot, target)
	if err != nil {
		return AppEndpoint{}, err
	}
	for _, device := range snapshot.Devices {
		if device.Device != target.UUID() {
			continue
		}
		if device.Withdraw || device.Expires <= time.Now().UnixMilli() {
			return AppEndpoint{}, ErrNoRoute
		}
		if device.AppEndpoints == nil {
			break
		} // Earlier authenticated WendyOS manifests use QUIC.
		for _, transport := range []string{localmesh.AppQUIC, localmesh.AppTLS} {
			for _, endpoint := range device.AppEndpoints {
				if endpoint.Transport == transport && endpoint.Port != 0 {
					return AppEndpoint{Transport: transport, Address: netip.AddrPortFrom(base.Addr(), endpoint.Port)}, nil
				}
			}
		}
		return AppEndpoint{}, ErrUnsupportedTransport
	}
	return AppEndpoint{Transport: localmesh.AppQUIC, Address: base}, nil
}

// ResolveAgentMTLS permits routed TCP only for the exact, signed native mTLS
// agent service. Its end-to-end TLS client must verify the target device and
// tenant as it does on a physical LAN. No arbitrary app port uses this path.
func ResolveAgentMTLS(snapshot localmesh.NodeSnapshot, target meshid.Device, port uint16) (netip.AddrPort, bool, error) {
	for _, device := range snapshot.Devices {
		if device.Device != target.UUID() || device.AgentTransport != localmesh.AgentMTLS || device.AgentPort != port {
			continue
		}
		if device.Withdraw || device.Expires <= time.Now().UnixMilli() {
			return netip.AddrPort{}, true, ErrNoRoute
		}
		address, err := Resolve(snapshot, target)
		if err != nil {
			return netip.AddrPort{}, true, err
		}
		return netip.AddrPortFrom(address.Addr(), port), true, nil
	}
	return netip.AddrPort{}, false, nil
}
