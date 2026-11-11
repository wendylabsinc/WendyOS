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

// MeshResolver resolves names within the provisioned tenant. Signed mesh names
// work over every carrier without Cloud; mDNS discovers ordinary LAN peers even
// when mesh participation is disabled, and the Cloud roster covers remote peers.
// OrgSlug remains only for compatibility with org-qualified DNS aliases.
type MeshResolver struct {
	logger *zap.Logger
	roster *MeshRoster
	browse func(context.Context) ([]models.LANDevice, error)

	mu           sync.RWMutex
	meshSnapshot func() localmesh.NodeSnapshot
}

var _ mesh.Resolver = (*MeshResolver)(nil)
var _ mesh.ExactResolver = (*MeshResolver)(nil)

// FriendlyNameLANBrowseBudget allows delayed LAN answers and conflicting name
// claims to arrive before a fresh friendly-name binding is selected.
const FriendlyNameLANBrowseBudget = 3 * time.Second

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

// Resolve retains normalization for legacy org-qualified names.
func (r *MeshResolver) Resolve(name string) (meshid.Device, bool) {
	if result := r.resolveMesh(name, false); result.found || result.ambiguous {
		return result.device, result.found && !result.ambiguous
	}
	if result := r.resolveLAN(name, false); result.found || result.ambiguous {
		return result.device, result.found && !result.ambiguous
	}
	entry, ok := r.roster.Lookup(name)
	if !ok {
		return meshid.Device{}, false
	}
	return entry.Device()
}

// ResolveExact preserves valid device labels, including repeated hyphens. It
// deliberately needs no Cloud-supplied org slug: the enrollment scopes all name
// sources and the eventual connection authenticates the exact device principal.
func (r *MeshResolver) ResolveExact(name string) (meshid.Device, bool) {
	name, valid := meshname.FriendlyLabel(name)
	if !valid {
		return meshid.Device{}, false
	}
	// Every currently known binding must agree. A valid signature proves
	// the device, not exclusive ownership of its human-readable name. In
	// particular a stale rename must not shadow the Cloud or LAN binding.
	result := r.resolveMesh(name, true)
	lan := r.resolveLAN(name, true)
	if lan.found {
		result.add(lan.device)
	}
	entry, found, ambiguous := r.roster.lookupExact(name)
	if found {
		device, valid := entry.Device()
		if valid {
			result.add(device)
		}
	}
	if result.ambiguous || lan.ambiguous || ambiguous {
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
	label, valid := shortDeviceLabel(raw)
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
	now := time.Now().UnixMilli()
	for _, manifest := range snapshot.Devices {
		// Directory snapshots contain verified manifests. The current signed
		// origin additionally excludes expired, withdrawn or conflicting claims;
		// operator display names cannot become device DNS entries.
		if manifest.Owner != nil || manifest.Withdraw || manifest.Expires <= now || manifest.Route == nil || !snapshot.RouteAuthorizations[*manifest.Route] || !nameMatches(manifest.Name, name, exact) {
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

func (r *MeshResolver) resolveLAN(name string, exact bool) nameResolution {
	var result nameResolution
	own, ok := r.roster.OwnTenant()
	if !ok || r.browse == nil {
		return result
	}
	ctx, cancel := context.WithTimeout(context.Background(), FriendlyNameLANBrowseBudget)
	defer cancel()
	devices, err := r.browse(ctx)
	if err != nil {
		r.logger.Warn("wendy net: LAN browse failed for friendly-name resolution", zap.Error(err))
		return result
	}
	for _, d := range devices {
		device, err := meshid.DeviceFromPrincipal(d.PrincipalURI)
		if err != nil || device.Tenant() != own || !nameMatches(d.MeshName, name, exact) {
			continue
		}
		// LAN advertisements are discovery hints. The dialer still requires
		// the endpoint to prove this exact principal before any app data flows.
		result.add(device)
	}
	return result
}
