package cloudenroll

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
)

// uplinkReportVersion is the first agent release that marks the interface
// carrying internet traffic (NetworkInterface.is_default_route, WDY-4102). The
// field has no capability bit: an older agent leaves it false on every
// interface, so its absence means "no route" only from this release on.
const uplinkReportVersion = "2026.11.08-183227"

// deviceInternetCheckTimeout bounds the whole preflight. The agent's own probe
// is bounded to a couple of seconds, so this only stops a stalled device from
// holding up an enrollment the preflight would have allowed anyway.
const deviceInternetCheckTimeout = 10 * time.Second

// carrierGradeNAT is shared address space (RFC 6598). It is not on the public
// internet, and netip.Addr.IsPrivate does not cover it.
var carrierGradeNAT = netip.MustParsePrefix("100.64.0.0/10")

// lookupPKIHost resolves the ACME directory host on this machine; a variable
// so tests can answer without a network.
var lookupPKIHost = net.DefaultResolver.LookupNetIP

// errDeviceOffline is CheckDeviceInternet's refusal. Its text is the whole
// recovery: the MCP enroll tool shows it as is.
var errDeviceOffline = errors.New("this device has no route to the internet, which enrollment needs to reach PKI; connect it to a network (for example with 'wendy device wifi connect', or plug in Ethernet), then enroll again (no Cloud reservation made)")

// CheckDeviceInternet refuses a device that provably cannot reach a PKI on the
// public internet, before Cloud reserves an asset for it. The device contacts
// the ACME directory only after the reservation, so a board tethered over USB
// alone otherwise fails there and leaves its name taken (WendyOS#2936).
//
// It refuses only when all of these hold, and otherwise enrolls as before:
//   - the agent reports no internet access. Its probe targets Docker Hub, so on
//     its own this is advisory: a network may block that registry and allow PKI.
//   - no interface carries a route to a public address: the agent lists no
//     interface with a routable address at all, or it is a release that marks
//     the uplink and marked none.
//   - every address the ACME directory host resolves to here is public. A PKI
//     on the device's own network needs no internet route.
//
// An agent that cannot answer either read is never refused on that account.
func CheckDeviceInternet(ctx context.Context, conn grpc.ClientConnInterface, directoryURL string) error {
	ctx, cancel := context.WithTimeout(ctx, deviceInternetCheckTimeout)
	defer cancel()
	caps, err := agentpbv2.NewWendyBuildServiceClient(conn).GetBuildCapabilities(ctx, &agentpbv2.GetBuildCapabilitiesRequest{})
	if err != nil || !caps.GetInternetCheckSupported() || caps.GetInternetReachable() {
		return nil
	}
	agent, err := agentpb.NewWendyAgentServiceClient(conn).GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return nil
	}
	ifaces := agent.GetNetworkInterfaces()
	for _, iface := range ifaces {
		if iface.GetIsDefaultRoute() {
			return nil
		}
	}
	if len(ifaces) > 0 && version.CompareVersions(agent.GetVersion(), uplinkReportVersion) < 0 {
		return nil
	}
	if !pkiIsPublic(ctx, directoryURL) {
		return nil
	}
	return errDeviceOffline
}

// pkiIsPublic reports whether the ACME directory is known to be on the public
// internet: its host resolves here, and only to public addresses.
func pkiIsPublic(ctx context.Context, directoryURL string) bool {
	u, err := url.Parse(directoryURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	addrs, err := lookupPKIHost(ctx, "ip", u.Hostname())
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !addr.IsGlobalUnicast() || addr.IsPrivate() || carrierGradeNAT.Contains(addr) {
			return false
		}
	}
	return true
}
