package services

import (
	"context"
	"net/netip"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshname"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// MeshResolver resolves short names from the provisioned tenant's signed mesh
// directory and authenticated Cloud roster. Legacy org-qualified aliases
// retain LAN discovery and use OrgSlug.
type MeshResolver struct {
	logger *zap.Logger
	roster *MeshRoster
	browse func(context.Context) ([]models.LANDevice, error)

	mu           sync.RWMutex
	meshSnapshot func() localmesh.NodeSnapshot
}

var _ mesh.Resolver = (*MeshResolver)(nil)
var _ mesh.ExactResolver = (*MeshResolver)(nil)

const legacyNameLANBrowseBudget = time.Second

func NewMeshResolver(logger *zap.Logger, roster *MeshRoster, browse func(context.Context) ([]models.LANDevice, error)) *MeshResolver {
	return &MeshResolver{logger: logger, roster: roster, browse: browse}
}

// SetMeshSnapshot supplies the currently admitted directory and routing view.
// The callback is read on each lookup so enrollment or carrier replacement never
// leaves the resolver holding a retired runtime's names.
func (r *MeshResolver) SetMeshSnapshot(snapshot func() localmesh.NodeSnapshot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.meshSnapshot = snapshot
}

func (r *MeshResolver) OrgSlug() string { return r.roster.OrgSlug() }

// Resolve retains the LAN and Cloud precedence of legacy org-qualified names.
// Signed mesh names can fill an absent binding without overriding an existing
// binding or hiding an ambiguous name.
func (r *MeshResolver) Resolve(name string) (meshid.Device, bool) {
	lan := r.resolveLAN(name)
	if lan.found && !lan.ambiguous {
		return lan.device, true
	}
	entry, found, ambiguous := r.roster.lookup(name)
	if ambiguous {
		return meshid.Device{}, false
	}
	if found {
		return entry.Device()
	}
	if lan.ambiguous {
		return meshid.Device{}, false
	}
	result := r.resolveMesh(name, false)
	return result.device, result.found && !result.ambiguous
}

// ResolveExact preserves valid device labels, including repeated hyphens. It
// deliberately needs no Cloud-supplied org slug: the enrollment scopes all name
// sources and the eventual connection authenticates the exact device principal.
func (r *MeshResolver) ResolveExact(name string) (meshid.Device, bool) {
	name, valid := meshname.FriendlyLabel(name)
	if !valid {
		return meshid.Device{}, false
	}
	// Only the signed directory and authenticated roster can bind a short
	// name. Comparing both also catches conflicting claims during a rename.
	result := r.resolveMesh(name, true)
	entry, found, ambiguous := r.roster.lookupExact(name)
	if found {
		device, valid := entry.Device()
		if valid {
			result.add(device)
		}
	}
	if result.ambiguous || ambiguous {
		return meshid.Device{}, false
	}
	return result.device, result.found
}

type nameResolution struct {
	device    meshid.Device
	found     bool
	ambiguous bool
}

func (r *nameResolution) add(device meshid.Device) {
	if r.found && r.device != device {
		r.ambiguous = true
	}
	r.device, r.found = device, true
}

func nameMatches(raw, name string, exact bool) bool {
	if !exact {
		return mesh.Normalize(raw) == name
	}
	label, valid := meshname.ShortDeviceLabel(raw)
	return valid && label == name
}

func (r *MeshResolver) resolveMesh(name string, exact bool) nameResolution {
	var result nameResolution
	own, ok := r.roster.OwnTenant()
	if !ok {
		return result
	}
	r.mu.RLock()
	read := r.meshSnapshot
	r.mu.RUnlock()
	if read == nil {
		return result
	}
	snapshot := read()
	if snapshot.Self.Tenant() != own {
		return result
	}
	now := time.Now()
	for _, manifest := range snapshot.Devices {
		// Directory snapshots contain verified manifests. The current signed
		// origin additionally excludes expired, withdrawn or conflicting claims;
		// operator display names cannot become device DNS entries.
		if manifest.Owner != nil || manifest.Withdraw || manifest.LeaseRemaining(now) <= 0 || manifest.Route == nil || !snapshot.RouteAuthorizations[*manifest.Route] || !nameMatches(manifest.Name, name, exact) {
			continue
		}
		device := meshid.NewDevice(own, manifest.Device)
		if manifest.Route.Prefix != netip.PrefixFrom(device.MeshAddress(), 128) {
			continue
		}
		result.add(device)
	}
	return result
}

func (r *MeshResolver) resolveLAN(name string) nameResolution {
	var result nameResolution
	own, ok := r.roster.OwnTenant()
	if !ok || r.browse == nil {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), legacyNameLANBrowseBudget)
	defer cancel()
	devices, err := r.browse(ctx)
	if err != nil {
		r.logger.Warn("wendy net: LAN browse failed for friendly-name resolution", zap.Error(err))
		return result
	}
	for _, d := range devices {
		device, err := meshid.DeviceFromPrincipal(d.PrincipalURI)
		if err != nil || device.Tenant() != own || mesh.Normalize(d.MeshName) != name {
			continue
		}
		// LAN advertisements are discovery hints. The dialer still requires
		// the endpoint to prove this exact principal before any app data flows.
		result.add(device)
	}
	return result
}
