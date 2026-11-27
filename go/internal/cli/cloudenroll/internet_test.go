package cloudenroll

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// deviceConn answers the two reads the internet preflight makes. A nil
// response is an agent that does not implement that RPC.
type deviceConn struct {
	caps  *agentpbv2.GetBuildCapabilitiesResponse
	agent *agentpb.GetAgentVersionResponse
}

func (c deviceConn) Invoke(_ context.Context, method string, _, reply any, _ ...grpc.CallOption) error {
	var resp proto.Message
	switch method {
	case agentpbv2.WendyBuildService_GetBuildCapabilities_FullMethodName:
		if c.caps != nil {
			resp = c.caps
		}
	case agentpb.WendyAgentService_GetAgentVersion_FullMethodName:
		if c.agent != nil {
			resp = c.agent
		}
	}
	if resp == nil {
		return status.Error(codes.Unimplemented, method)
	}
	proto.Merge(reply.(proto.Message), resp)
	return nil
}

func (deviceConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("unused")
}

const offlineRefusal = "this device has no route to the internet, which enrollment needs to reach PKI; connect it to a network (for example with 'wendy device wifi connect', or plug in Ethernet), then enroll again (no Cloud reservation made)"

// The Cloud asset is reserved before the device contacts PKI, so a device that
// provably cannot get there is refused first. Anything short of proof enrolls
// as before: the agent's reachability probe targets Docker Hub, not PKI.
func TestCheckDeviceInternet(t *testing.T) {
	offline := &agentpbv2.GetBuildCapabilitiesResponse{InternetCheckSupported: true, InternetDetail: "cannot resolve registry-1.docker.io"}
	online := &agentpbv2.GetBuildCapabilitiesResponse{InternetCheckSupported: true, InternetReachable: true}
	// What GetAgentVersion reports from an agent of the given release.
	agent := func(version string, ifaces ...*agentpb.NetworkInterface) *agentpb.GetAgentVersionResponse {
		return &agentpb.GetAgentVersionResponse{Version: version, NetworkInterfaces: ifaces}
	}
	lan := &agentpb.NetworkInterface{Name: "eth0", IpAddresses: []string{"192.168.123.18"}}
	uplink := &agentpb.NetworkInterface{Name: "wlan0", IpAddresses: []string{"192.168.1.42"}, IsDefaultRoute: true}
	const (
		publicPKI    = "https://203.0.113.10/tenant/acme/directory"
		privatePKI   = "https://10.0.0.5/tenant/acme/directory"
		marksUplink  = "2026.11.08-183227"
		beforeUplink = "2026.11.07-165019"
	)
	for _, tc := range []struct {
		name      string
		conn      deviceConn
		directory string
		want      string
	}{
		{"USB-only device with no routable address", deviceConn{offline, agent(beforeUplink)}, publicPKI, offlineRefusal},
		{"LAN address but no interface carries internet traffic", deviceConn{offline, agent(marksUplink, lan)}, publicPKI, offlineRefusal},
		{"development agent with no uplink", deviceConn{offline, agent("dev", lan)}, publicPKI, offlineRefusal},
		{"agent predating the uplink mark cannot rule a route out", deviceConn{offline, agent(beforeUplink, lan)}, publicPKI, ""},
		{"Docker Hub blocked but an interface carries internet traffic", deviceConn{offline, agent(marksUplink, lan, uplink)}, publicPKI, ""},
		{"device reports internet access", deviceConn{online, agent(marksUplink)}, publicPKI, ""},
		{"agent without the internet check", deviceConn{&agentpbv2.GetBuildCapabilitiesResponse{}, agent(marksUplink)}, publicPKI, ""},
		{"agent without the build service", deviceConn{nil, agent(marksUplink)}, publicPKI, ""},
		{"interfaces unreadable", deviceConn{offline, nil}, publicPKI, ""},
		{"PKI on a private network needs no internet route", deviceConn{offline, agent(marksUplink)}, privatePKI, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ""
			if err := CheckDeviceInternet(context.Background(), tc.conn, tc.directory); err != nil {
				got = err.Error()
			}
			if got != tc.want {
				t.Fatalf("preflight = %q, want %q", got, tc.want)
			}
		})
	}
}

// A PKI named by host is public only when every address it resolves to is;
// one the CLI cannot place is never grounds for refusing.
func TestCheckDeviceInternetResolvesPKIHost(t *testing.T) {
	conn := deviceConn{
		caps:  &agentpbv2.GetBuildCapabilitiesResponse{InternetCheckSupported: true},
		agent: &agentpb.GetAgentVersionResponse{Version: "2026.11.08-183227"},
	}
	for _, tc := range []struct {
		name  string
		addrs []string
		err   error
		want  string
	}{
		{"public addresses", []string{"203.0.113.10", "2001:db8::10"}, nil, offlineRefusal},
		{"private address", []string{"10.0.0.5"}, nil, ""},
		{"split between public and private", []string{"203.0.113.10", "192.168.1.5"}, nil, ""},
		{"carrier-grade NAT address", []string{"100.64.0.5"}, nil, ""},
		{"IPv6 unique local address", []string{"fd00::5"}, nil, ""},
		{"does not resolve", nil, errors.New("no such host"), ""},
		{"resolves to nothing", nil, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := lookupPKIHost
			t.Cleanup(func() { lookupPKIHost = old })
			lookupPKIHost = func(_ context.Context, _, host string) ([]netip.Addr, error) {
				if host != "acme.pki.example" {
					t.Fatalf("resolved %q, want the ACME directory host", host)
				}
				var out []netip.Addr
				for _, a := range tc.addrs {
					out = append(out, netip.MustParseAddr(a))
				}
				return out, tc.err
			}
			got := ""
			if err := CheckDeviceInternet(context.Background(), conn, "https://acme.pki.example/tenant/acme/directory"); err != nil {
				got = err.Error()
			}
			if got != tc.want {
				t.Fatalf("preflight = %q, want %q", got, tc.want)
			}
		})
	}
}
