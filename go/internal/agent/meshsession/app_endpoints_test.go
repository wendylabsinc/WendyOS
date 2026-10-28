package meshsession

import (
	"errors"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"net/netip"
	"testing"
	"time"
)

func TestResolveAuthenticatedAppTransport(t *testing.T) {
	peer := fixtureDevice(t, fixtureDeviceB)
	snapshot := localmesh.NodeSnapshot{Routes: []babel.Route{{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128)}}}
	endpoint, err := ResolveApp(snapshot, peer)
	if err != nil || endpoint.Transport != localmesh.AppQUIC || endpoint.Address.Port() != Port {
		t.Fatal(endpoint, err)
	}
	snapshot.Devices = []localmesh.Manifest{{Device: peer.UUID(), Expires: time.Now().Add(time.Minute).UnixMilli(), AppEndpoints: []localmesh.AppEndpoint{{Transport: localmesh.AppTLS, Port: localmesh.AppSessionTLSPort}}}}
	endpoint, err = ResolveApp(snapshot, peer)
	if err != nil || endpoint.Transport != localmesh.AppTLS || endpoint.Address.Port() != localmesh.AppSessionTLSPort {
		t.Fatal(endpoint, err)
	}
	snapshot.Devices[0].AppEndpoints = append(snapshot.Devices[0].AppEndpoints, localmesh.AppEndpoint{Transport: localmesh.AppQUIC, Port: Port})
	endpoint, err = ResolveApp(snapshot, peer)
	if err != nil || endpoint.Transport != localmesh.AppQUIC {
		t.Fatal(endpoint, err)
	}
	snapshot.Devices[0].AppEndpoints = []localmesh.AppEndpoint{}
	if _, err = ResolveApp(snapshot, peer); !errors.Is(err, ErrUnsupportedTransport) {
		t.Fatalf("explicitly unavailable app transports: %v", err)
	}
	snapshot.Devices[0].AppEndpoints = nil
	if legacy, err := ResolveApp(snapshot, peer); err != nil || legacy.Transport != localmesh.AppQUIC {
		t.Fatalf("legacy absent capabilities: %+v %v", legacy, err)
	}
	snapshot.Devices[0].Expires = 0
	if _, err = ResolveApp(snapshot, peer); !errors.Is(err, ErrNoRoute) {
		t.Fatal(err)
	}
	snapshot.Routes = nil
	if _, err = ResolveApp(snapshot, peer); !errors.Is(err, ErrNoRoute) {
		t.Fatal(err)
	}
}

func TestResolveDelegatedEndpointUsesForwardingRoute(t *testing.T) {
	peer := fixtureDevice(t, fixtureDeviceB)
	prefix := netip.PrefixFrom(peer.MeshAddress(), 128)
	snapshot := localmesh.NodeSnapshot{
		Routes:           []babel.Route{{Prefix: prefix, Local: true}},
		ForwardingRoutes: []babel.Route{{Prefix: prefix, Link: 1}},
		Devices:          []localmesh.Manifest{{Device: peer.UUID(), Expires: time.Now().Add(time.Minute).UnixMilli(), AppEndpoints: []localmesh.AppEndpoint{{Transport: localmesh.AppTLS, Port: TLSPort}}}},
	}
	if endpoint, err := ResolveApp(snapshot, peer); err != nil || endpoint.Transport != localmesh.AppTLS {
		t.Fatal(endpoint, err)
	}
	snapshot.ForwardingRoutes = []babel.Route{}
	if _, err := ResolveApp(snapshot, peer); !errors.Is(err, ErrNoRoute) {
		t.Fatal(err)
	}
	snapshot.ForwardingRoutes = []babel.Route{{Prefix: prefix, Link: 1, Unreachable: true}}
	if _, err := ResolveApp(snapshot, peer); !errors.Is(err, ErrNoRoute) {
		t.Fatal(err)
	}
}

func TestNativeAgentMTLSRequiresExactSignedPortAndForwarding(t *testing.T) {
	peer := fixtureDevice(t, fixtureDeviceB)
	snapshot := localmesh.NodeSnapshot{
		Routes:  []babel.Route{{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128)}},
		Devices: []localmesh.Manifest{{Device: peer.UUID(), Expires: time.Now().Add(time.Minute).UnixMilli(), AgentPort: 5054, AgentTransport: localmesh.AgentMTLS}},
	}
	address, native, err := ResolveAgentMTLS(snapshot, peer, 5054)
	if err != nil || !native || address != netip.AddrPortFrom(peer.MeshAddress(), 5054) {
		t.Fatal(address, native, err)
	}
	if _, native, _ = ResolveAgentMTLS(snapshot, peer, 50051); native {
		t.Fatal("arbitrary port bypassed app authorization")
	}
	snapshot.Devices[0].AgentTransport = ""
	if _, native, _ = ResolveAgentMTLS(snapshot, peer, 5054); native {
		t.Fatal("absent capability selected native TCP")
	}
	snapshot.Devices[0].AgentTransport = localmesh.AgentMTLS
	snapshot.ForwardingRoutes = []babel.Route{}
	if _, native, err = ResolveAgentMTLS(snapshot, peer, 5054); !native || !errors.Is(err, ErrNoRoute) {
		t.Fatal(native, err)
	}
	snapshot.ForwardingRoutes = nil
	snapshot.Devices[0].Withdraw = true
	if _, native, err = ResolveAgentMTLS(snapshot, peer, 5054); !native || !errors.Is(err, ErrNoRoute) {
		t.Fatal(native, err)
	}
}
