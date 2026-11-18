package commands

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/internal/shared/terminaloutput"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// A foreground account generation owns channels. Keys include the credential
// snapshot and verified target identity, so LAN and Cloud can share a channel
// without sharing trust between accounts, renewed credentials, or devices.
func desktopSessionConnection(ctx context.Context, auth *config.AuthConfig, target string, dial func(context.Context) (*grpcclient.AgentConnection, error)) (*grpcclient.AgentConnection, error) {
	return desktopSessionConnect(ctx, auth, target, dial, false)
}

func desktopSessionFreshConnection(ctx context.Context, auth *config.AuthConfig, target string, dial func(context.Context) (*grpcclient.AgentConnection, error)) (*grpcclient.AgentConnection, error) {
	return desktopSessionConnect(ctx, auth, target, dial, true)
}

func desktopSessionConnect(ctx context.Context, auth *config.AuthConfig, target string, dial func(context.Context) (*grpcclient.AgentConnection, error), fresh bool) (*grpcclient.AgentConnection, error) {
	g, ok := ctx.Value(tunnelContextKey{}).(*tunnelGeneration)
	if !ok {
		return dial(ctx)
	}
	g.mu.Lock()
	closed := g.closed
	g.mu.Unlock()
	if closed {
		return nil, context.Canceled
	}
	key, err := desktopConnectionKey(auth, target)
	if err != nil {
		return nil, err
	}
	if fresh {
		return g.devices.Refresh(ctx, key, dial)
	}
	return g.devices.Acquire(ctx, key, dial)
}

func desktopConnectionKey(auth *config.AuthConfig, target string) (string, error) {
	credentials, err := json.Marshal(auth.Certificates)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s\x00%s\x00%x\x00%s", auth.Name, auth.CloudGRPC, sha256.Sum256(credentials), target), nil
}

func desktopRememberCloudTarget(ctx context.Context, auth *config.AuthConfig, asset cloudDiscoveryDevice, identity string) {
	g, ok := ctx.Value(tunnelContextKey{}).(*tunnelGeneration)
	if !ok {
		return
	}
	selector, err := cloudDiscoveryDeviceDefault(auth, asset)
	if err != nil {
		return
	}
	alias, err := desktopConnectionKey(auth, selector)
	if err != nil {
		return
	}
	key, err := desktopConnectionKey(auth, identity)
	if err != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return
	}
	if g.deviceAliases == nil {
		g.deviceAliases = make(map[string]string)
	}
	g.deviceAliases[alias] = key
}

// A selector resolved in this account generation can reuse its authenticated
// channel even when Cloud's roster is temporarily unavailable. Never infer its
// PKI identity from the asset UUID: Cloud can bind it to a different device name.
func desktopExistingCloudConnection(ctx context.Context, auth *config.AuthConfig, selector string) (*grpcclient.AgentConnection, error) {
	g, ok := ctx.Value(tunnelContextKey{}).(*tunnelGeneration)
	if !ok {
		return nil, nil
	}
	alias, err := desktopConnectionKey(auth, selector)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	key, closed := g.deviceAliases[alias], g.closed
	g.mu.Unlock()
	if closed {
		return nil, context.Canceled
	}
	if key == "" {
		return nil, nil
	}
	return g.devices.Lookup(ctx, key)
}

// Older saved camera rows omitted the advertised mTLS flag. A trusted saved
// identity still requires mTLS and can use the same native connection pool.
func desktopPinnedLAN(ctx context.Context, selector string) bool {
	if _, native := ctx.Value(tunnelContextKey{}).(*tunnelGeneration); !native {
		return false
	}
	if strings.Contains(selector, "://") || strings.HasPrefix(selector, "mesh:") || strings.HasPrefix(selector, "vm:") || strings.HasPrefix(selector, "wendy-lite:") {
		return false
	}
	target := newDialTarget(pinKeyForAddr(mcpStartupAddress(selector)), selector)
	return target.pinned()
}

var desktopLocalCandidates = localCloudCandidates
var desktopLocalDial = dialDesktopLocal
var desktopCloudDial func(context.Context, *config.AuthConfig, cloudDiscoveryDevice, string) (*grpcclient.AgentConnection, error)

// Restart recovery explicitly requests a fresh channel. Keep the Cloud target
// identity even when the original route was LAN; a DHCP address is not identity.
func desktopReconnectableCloudConnection(ctx context.Context, auth *config.AuthConfig, asset cloudDiscoveryDevice, brokerURL string) (*grpcclient.AgentConnection, error) {
	conn, err := connectCloudDevicePreferLAN(ctx, auth, asset, brokerURL)
	if err != nil {
		return nil, err
	}
	conn.Reconnect = func(next context.Context) (*grpcclient.AgentConnection, error) {
		next = carryTunnelSession(next, ctx)
		return desktopSessionFreshConnection(next, auth, cloudDiscoveryIdentity(auth, asset).IdentityKey(), func(ctx context.Context) (*grpcclient.AgentConnection, error) {
			return desktopReconnectableCloudConnection(ctx, auth, asset, brokerURL)
		})
	}
	return conn, nil
}

func connectCloudDevicePreferLAN(ctx context.Context, auth *config.AuthConfig, asset cloudDiscoveryDevice, brokerURL string) (*grpcclient.AgentConnection, error) {
	expected := cloudDiscoveryIdentity(auth, asset)
	// A stale local sighting must not turn a remote open into a long timeout.
	localCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	candidates := desktopLocalCandidates(localCtx, asset, expected)
	if os.Getenv("WENDY_TLS_DEBUG") != "" {
		fmt.Fprintf(terminaloutput.Stderr, "[tls-debug] LAN candidates for %s: %v\n", expected.IdentityKey(), candidates)
	}
	conn := raceDesktopLocal(localCtx, candidates, auth.Certificates[0], expected)
	cancel()
	if conn != nil {
		// A CLI restart must retain the selected identity and wait for the
		// device to return, even if its LAN address changes during the restart.
		conn.Reconnect = func(next context.Context) (*grpcclient.AgentConnection, error) {
			return asset.reconnect(carryTunnelSession(next, ctx), auth, brokerURL)
		}
		cliLogln("Connected to %s via LAN (%s).", asset.GetName(), conn.Addr)
		return conn, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dial := desktopCloudDial
	if dial == nil {
		dial = connectCloudDiscoveryTransport
	}
	cliLogln("Connecting to %s via cloud tunnel...", asset.GetName())
	conn, err := dial(ctx, auth, asset, brokerURL)
	if err != nil {
		return nil, err
	}
	if _, native := ctx.Value(tunnelContextKey{}).(*tunnelGeneration); !native {
		return conn, nil
	}
	// Force the lazy gRPC handshake before sharing the channel with waiters.
	version, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.CacheAgentVersion(version)
	return conn, nil
}

const desktopLocalCandidateLimit = 8

// Multiple stale DHCP or service-port hints must not consume the whole local
// budget before a live address gets a chance. Each successful loser owns its
// cleanup; returning the winner never waits for another endpoint to time out.
func raceDesktopLocal(ctx context.Context, candidates []string, cert config.CertificateInfo, expected certs.WendyIdentity) *grpcclient.AgentConnection {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan *grpcclient.AgentConnection)
	dial := desktopLocalDial
	count := min(len(candidates), desktopLocalCandidateLimit)
	for _, address := range candidates[:count] {
		go func() {
			conn, err := dial(ctx, address, cert, expected)
			if err != nil && os.Getenv("WENDY_TLS_DEBUG") != "" {
				fmt.Fprintf(terminaloutput.Stderr, "[tls-debug] LAN candidate %s: %v\n", address, err)
			}
			if err != nil && conn != nil {
				conn.Close()
				conn = nil
			}
			select {
			case results <- conn:
			case <-ctx.Done():
				if conn != nil {
					conn.Close()
				}
			}
		}()
	}
	for range count {
		select {
		case conn := <-results:
			if conn != nil {
				if ctx.Err() != nil {
					conn.Close()
					return nil
				}
				return conn
			}
		case <-ctx.Done():
			return nil
		}
	}
	return nil
}

func dialDesktopLocal(ctx context.Context, address string, cert config.CertificateInfo, expected certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
	conn, err := grpcclient.ConnectWithTLSExpecting(ctx, address, &cert, openPinStore(), &expected)
	if err != nil {
		return nil, err
	}
	version, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		conn.Close()
		return nil, err
	}
	conn.CacheAgentVersion(version)
	return conn, nil
}

// Discovery names and IPs are routing hints only. Every attempted address must
// prove the exact Cloud-selected identity over mTLS, including PKI name bindings.
func localCloudCandidates(ctx context.Context, asset cloudDiscoveryDevice, expected certs.WendyIdentity) []string {
	var addresses []string
	add := func(ip string, port int, iface string) {
		for _, address := range desktopLocalAddressCandidates(ip, port, iface, discovery.OnLink, desktopLocalInterface) {
			if len(addresses) >= desktopLocalCandidateLimit {
				return
			}
			duplicate := false
			for _, existing := range addresses {
				if existing == address {
					duplicate = true
					break
				}
			}
			if !duplicate {
				addresses = append(addresses, address)
			}
		}
	}
	matches := func(id, name, hostname string) bool {
		return id == expected.IdentityKey() || id == expected.EntityID || id == asset.key ||
			(name != "" && name == asset.GetName()) ||
			(hostname != "" && strings.TrimSuffix(strings.TrimSuffix(hostname, "."), ".local") == expected.EntityID)
	}
	// Reserve room for the Cloud hint before cached aliases fill the bounded list.
	add(asset.GetIpAddress(), 0, "")
	if cache, err := discoverycache.Load(); err == nil {
		for _, e := range cache.Fresh(time.Now()) {
			if e.MTLS && matches(e.ID, e.DisplayName, e.Hostname) {
				add(e.IP, e.Port, e.InterfaceName)
			}
		}
	}
	if len(addresses) != 0 {
		return addresses
	}
	// On a cold launch the background browse may not have populated the cache.
	browseCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	for event := range discovery.StreamLAN(browseCtx, discovery.StreamOptions{UseCache: true}) {
		d := event.Device
		if event.Kind != discovery.LANFound && event.Kind != discovery.LANUpdated {
			continue
		}
		if d.IsMTLS && (matches(d.ID, d.DisplayName, d.Hostname) || matches(models.CanonicalDevicePrincipal(d.PrincipalURI), d.DisplayName, d.Hostname)) {
			add(d.IPAddress, d.Port, d.NetworkInterface)
			if len(addresses) != 0 {
				return addresses
			}
		}
	}
	return addresses
}

func desktopLocalInterface(zone string) (*net.Interface, error) {
	if index, err := strconv.Atoi(zone); err == nil {
		return net.InterfaceByIndex(index)
	}
	return net.InterfaceByName(zone)
}

// A link-local address is meaningful only on a known, currently up interface.
// Private addresses can cross LAN subnets or a VPN; public addresses must be
// on a local subnet. These are routing hints only: mTLS verifies the identity.
func desktopLocalAddressCandidates(ip string, port int, iface string, onLink func(string) bool, lookup func(string) (*net.Interface, error)) []string {
	address, err := netip.ParseAddr(strings.TrimSpace(ip))
	if err != nil || address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() {
		return nil
	}
	if address.IsLinkLocalUnicast() {
		zone := address.Zone()
		if zone == "" {
			zone = iface
		} else if iface != "" && iface != zone {
			return nil
		}
		if zone == "" {
			return nil
		}
		link, err := lookup(zone)
		if err != nil || link == nil || link.Flags&net.FlagUp == 0 || link.Flags&net.FlagLoopback != 0 {
			return nil
		}
		if address.Is6() {
			address = address.WithZone(zone)
		}
	} else if address.Zone() != "" || (!address.IsPrivate() && !onLink(address.String())) {
		return nil
	}
	if port > 65535 {
		return nil
	}
	defaultPort := port <= 0
	if defaultPort {
		port = defaultAgentPort + 1
	}
	result := []string{net.JoinHostPort(address.String(), strconv.Itoa(port))}
	// Older advertisements can retain the plaintext agent port after their
	// probe discovers enrollment. Keep the existing ladder's TLS-only +1 rung.
	if !defaultPort && port < 65535 {
		result = append(result, net.JoinHostPort(address.String(), strconv.Itoa(port+1)))
	}
	return result
}
