package services

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// MeshResolver implements mesh.Resolver with the hybrid strategy: an mDNS
// browse (filtered to our own tenant) resolves LAN peers cloud-free; anything
// not found on the LAN falls back to the cloud-synced roster cache. OrgSlug
// and the tenant filter both come from the roster's provisioned identity, so
// roster.UpdateIdentity alone re-scopes every resolution path.
type MeshResolver struct {
	logger *zap.Logger
	roster *MeshRoster
	browse func(context.Context) ([]models.LANDevice, error)
}

var _ mesh.Resolver = (*MeshResolver)(nil)

func NewMeshResolver(logger *zap.Logger, roster *MeshRoster, browse func(context.Context) ([]models.LANDevice, error)) *MeshResolver {
	return &MeshResolver{logger: logger, roster: roster, browse: browse}
}

func (r *MeshResolver) OrgSlug() string { return r.roster.OrgSlug() }

// Resolve returns the device a normalized name names: mDNS first, roster
// second. A LAN sighting is only a discovery hint — the advertised principal
// must parse as a device under our own tenant, and TLS verifies it at dial
// time. A roster row with no device UUID is not dialable and resolves to
// nothing rather than an invented identity.
func (r *MeshResolver) Resolve(name string) (meshid.Device, bool) {
	if device, ok := r.resolveLAN(name); ok {
		return device, true
	}
	entry, ok := r.roster.Lookup(name)
	if !ok {
		return meshid.Device{}, false
	}
	return entry.Device()
}

func (r *MeshResolver) resolveLAN(name string) (meshid.Device, bool) {
	own, ok := r.roster.OwnTenant()
	if !ok {
		return meshid.Device{}, false // unenrolled: no tenant to filter by
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	devices, err := r.browse(ctx)
	if err != nil {
		r.logger.Warn("wendy net: LAN browse failed for friendly-name resolution", zap.Error(err))
		return meshid.Device{}, false
	}
	var found meshid.Device
	matches := 0
	for _, d := range devices {
		if d.PrincipalURI == "" {
			continue
		}
		device, err := meshid.DeviceFromPrincipal(d.PrincipalURI)
		if err != nil || device.Tenant() != own {
			continue
		}
		if mesh.Normalize(d.MeshName) != name {
			continue
		}
		if matches == 0 {
			found = device
		} else if device != found {
			// Two different LAN devices share the name -> ambiguous. The same
			// device sighted on several interfaces repeats one principal and
			// stays unambiguous.
			return meshid.Device{}, false
		}
		matches++
	}
	if matches == 0 {
		return meshid.Device{}, false
	}
	return found, true
}
