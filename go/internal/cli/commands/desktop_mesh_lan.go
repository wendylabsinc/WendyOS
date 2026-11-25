package commands

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

var desktopMeshLocalCandidates = cachedMeshLocalCandidates

// Keep the canonical mesh selection and its authenticated app/registry routes.
// Only native agent RPCs may take a verified LAN shortcut. CLI selections and
// VPN flows retain the original mesh connection unchanged.
func connectDesktopMeshAgent(ctx context.Context, selector string) (*grpcclient.AgentConnection, error) {
	if _, native := ctx.Value(tunnelContextKey{}).(*tunnelGeneration); !native || cloudTunnelOnly(ctx) {
		return connectOperatorMeshSelector(ctx, selector)
	}
	device, matched, err := sessionbroker.ParseMeshSelector(selector)
	if err != nil || !matched {
		return nil, errors.New("invalid mesh device selector")
	}
	auth, err := desktopMeshAuth()
	if err != nil {
		return nil, err
	}
	operator, err := meshid.PeerFromPrincipal(auth.Certificates[0].PrincipalURI)
	if err != nil || !operator.IsOperator() || operator.Tenant() != device.Tenant() {
		return nil, errors.New("selected mesh device and sign-in profile belong to different organizations")
	}
	// This namespace keeps mesh auxiliary capabilities separate from ordinary
	// LAN/Cloud pool entries for the same device identity.
	target := "mesh-agent:" + selector
	key, err := desktopConnectionKey(auth, target)
	if err != nil {
		return nil, err
	}
	return desktopSessionConnection(ctx, auth, target, func(ctx context.Context) (*grpcclient.AgentConnection, error) {
		return dialDesktopMeshAgent(ctx, selector, device, auth, target, key)
	})
}

func desktopMeshAuth() (*config.AuthConfig, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	auth, err := config.ResolveAuth(cfg, "", nil)
	if err != nil {
		return nil, err
	}
	if len(auth.Certificates) == 0 {
		return nil, errors.New("the selected sign-in profile has no mesh certificate")
	}
	return auth, nil
}

func desktopMeshScopeCurrent(ctx context.Context, target, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if g, ok := ctx.Value(tunnelContextKey{}).(*tunnelGeneration); ok {
		g.mu.Lock()
		closed := g.closed
		g.mu.Unlock()
		if closed {
			return context.Canceled
		}
	}
	auth, err := desktopMeshAuth()
	if err != nil {
		return err
	}
	current, err := desktopConnectionKey(auth, target)
	if err != nil {
		return err
	}
	if current != key {
		return errors.New("selected sign-in profile or mesh certificate changed; reconnect to the device")
	}
	return nil
}

func dialDesktopMeshAgent(ctx context.Context, selector string, device meshid.Device, auth *config.AuthConfig, target, key string) (_ *grpcclient.AgentConnection, err error) {
	if err = desktopMeshScopeCurrent(ctx, target, key); err != nil {
		return nil, err
	}
	mesh, err := connectOperatorMeshSelector(ctx, selector)
	if err != nil {
		if mesh != nil {
			mesh.Close()
		}
		return nil, err
	}
	result := mesh
	defer func() {
		if err != nil {
			result.Close()
		}
	}()
	if mesh == nil || mesh.CertInfo == nil || *mesh.CertInfo != auth.Certificates[0] || mesh.OperatorMeshTarget != device {
		return nil, errors.New("mesh connection did not retain the selected device and certificate")
	}
	if err = desktopMeshScopeCurrent(ctx, target, key); err != nil {
		return nil, err
	}
	expected, err := certs.ParseDevicePrincipal(device.Principal())
	if err != nil {
		return nil, err
	}
	// A stale DHCP hint cannot hold a usable mesh connection for longer than
	// the same bounded local-authentication budget used by Cloud selection.
	localCtx, cancel := context.WithTimeout(ctx, cloudLANBudget)
	local := raceDesktopLocal(localCtx, desktopMeshLocalCandidates(device), *mesh.CertInfo, expected)
	cancel()
	if local != nil {
		local.OperatorMeshTarget = device
		local.AppDialer = mesh.AppDialer
		local.RegistryDialer = mesh.RegistryDialer
		local.AddCloser(mesh)
		result = local
	}
	if err = desktopMeshScopeCurrent(ctx, target, key); err != nil {
		return nil, err
	}
	result.Reconnect = func(next context.Context) (*grpcclient.AgentConnection, error) {
		next = carryTunnelSession(next, ctx)
		if err := desktopMeshScopeCurrent(next, target, key); err != nil {
			return nil, err
		}
		return desktopSessionFreshConnection(next, auth, target, result, func(ctx context.Context) (*grpcclient.AgentConnection, error) {
			return dialDesktopMeshAgent(ctx, selector, device, auth, target, key)
		})
	}
	return result, nil
}

// Cached addresses are routing hints only. Do not use display names, arbitrary
// hostnames, or a first-hop mesh address as the selected device's LAN address.
func cachedMeshLocalCandidates(device meshid.Device) []string {
	cache, err := discoverycache.Load()
	if err != nil {
		return nil
	}
	addresses := []string{}
	seen := map[string]bool{}
	for _, entry := range cache.Fresh(time.Now()) {
		if !entry.MTLS || (entry.ID != device.UUID().String() && entry.ID != device.Principal()) {
			continue
		}
		for _, address := range desktopLocalAddressCandidates(entry.IP, entry.Port, entry.InterfaceName, discovery.OnLink, desktopLocalInterface) {
			if !seen[address] {
				seen[address] = true
				addresses = append(addresses, address)
				if len(addresses) == desktopLocalCandidateLimit {
					return addresses
				}
			}
		}
	}
	return addresses
}
