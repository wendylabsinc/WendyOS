package services

import (
	"context"
	"os"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshname"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// RosterEntry is a friendly name and one certificate device UUID. Cloud's
// asset_id wire field carries that same UUID. AssetUUID is retained as an alias
// for existing Cloud RPC adapters, never a second identity or address input.
type RosterEntry struct {
	Name       string // normalized DNS label
	ShortName  string // Cloud label with exact hyphens preserved
	DeviceUUID string // canonical lowercase device UUID; "" = no mesh identity
	AssetUUID  string // alias of DeviceUUID for Cloud asset_id adapters

	device    meshid.Device
	hasDevice bool
}

// Device returns the entry's mesh identity, parsed through the shared meshid
// contract against the roster's authenticated tenant. ok=false when the row
// carries no usable device binding: callers must not invent an address.
func (e RosterEntry) Device() (meshid.Device, bool) { return e.device, e.hasDevice }

// Dialable reports whether this entry can produce a mesh identity at all.
func (e RosterEntry) Dialable() bool { return e.hasDevice }

// MeshRoster caches this device's tenant {normalized name -> roster entry}
// directory and the tenant's own slug, refreshed from the cloud GetMeshRoster
// RPC. It is the off-LAN half of the hybrid friendly-name resolver.
type MeshRoster struct {
	logger   *zap.Logger
	cloudURL string
	certPEM  string
	keyPEM   string
	chainPEM string

	mu             sync.RWMutex
	generation     uint64
	identity       string        // proof identity read from the leaf cert; "" until provisioned
	tenant         meshid.Tenant // tenant that identity authenticates; valid when hasTenant
	hasTenant      bool
	slug           string
	byName         map[string]RosterEntry
	byAsset        map[string]RosterEntry // canonical asset UUID -> entry
	ambiguous      map[string]struct{}
	byExactName    map[string]RosterEntry
	exactAmbiguous map[string]struct{}
	localNameFile  string
	localName      registeredDeviceName
	nameNeedsSave  bool
	refresh        chan struct{}
}

func NewMeshRoster(logger *zap.Logger, cloudURL, certPEM, keyPEM, chainPEM string) *MeshRoster {
	r := &MeshRoster{
		logger:         logger,
		cloudURL:       cloudURL,
		certPEM:        certPEM,
		keyPEM:         keyPEM,
		chainPEM:       chainPEM,
		byName:         make(map[string]RosterEntry),
		byAsset:        make(map[string]RosterEntry),
		ambiguous:      make(map[string]struct{}),
		byExactName:    make(map[string]RosterEntry),
		exactAmbiguous: make(map[string]struct{}),
		refresh:        make(chan struct{}, 1),
	}
	r.loadIdentity()
	return r
}

// loadIdentity requires this device's canonical mesh principal. Unsupported
// certificates cannot establish the tenant for roster or LAN mesh discovery.
// The caller must hold the write lock.
func (r *MeshRoster) loadIdentity() {
	r.identity, r.tenant, r.hasTenant = "", meshid.Tenant{}, false
	if r.certPEM == "" || r.keyPEM == "" {
		return
	}
	leaves, err := certs.ParseCertsFromPEM([]byte(r.certPEM))
	if err != nil || len(leaves) == 0 {
		r.logger.Warn("wendy net roster: device certificate could not be read; sync stays disabled until provisioning completes", zap.Error(err))
		return
	}
	device, err := meshid.DeviceFromCert(leaves[0])
	if err != nil {
		r.logger.Warn("Wendy Net roster: certificate has no supported net device identity; sync stays disabled", zap.Error(err))
		return
	}
	r.identity = device.Principal()
	r.tenant, r.hasTenant = device.Tenant(), true
}

// OrgSlug returns this device's tenant slug as the cloud normalized it.
func (r *MeshRoster) OrgSlug() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.slug
}

// OwnTenant returns the tenant of the provisioned identity the roster
// authenticates with; false until provisioning completes. The friendly-name
// resolver filters LAN sightings through it so tenant scope lives in exactly
// one place.
func (r *MeshRoster) OwnTenant() (meshid.Tenant, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tenant, r.hasTenant
}

// UpdateIdentity swaps the identity used to authenticate GetMeshRoster calls.
// Called when BLE first-boot enrollment provisions the device while the agent
// is already running, so the boot-time snapshot (no certificate, no principal)
// needs to be replaced with the freshly issued identity. The cached entries are
// dropped with it: they were scoped to whatever tenant the previous identity
// authenticated — possibly none — and a tenant change must never serve stale
// names from the old scope.
func (r *MeshRoster) UpdateIdentity(cloudURL, certPEM, keyPEM, chainPEM string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	previous := r.identity
	r.generation++
	r.cloudURL, r.certPEM, r.keyPEM, r.chainPEM = cloudURL, certPEM, keyPEM, chainPEM
	r.loadIdentity()
	r.slug = ""
	r.byName = make(map[string]RosterEntry)
	r.byAsset = make(map[string]RosterEntry)
	r.ambiguous = make(map[string]struct{})
	r.byExactName = make(map[string]RosterEntry)
	r.exactAmbiguous = make(map[string]struct{})
	if r.identity != previous {
		r.restoreLocalName()
		if r.identity == "" && r.localNameFile != "" {
			if err := os.Remove(r.localNameFile); err != nil && !os.IsNotExist(err) {
				r.logger.Warn("wendy net: clearing cached Cloud device name failed", zap.Error(err))
			}
		}
	}
}

// Lookup returns the roster entry for a normalized device name. Unknown and
// ambiguous (duplicate-normalized) names return ok=false.
func (r *MeshRoster) Lookup(name string) (RosterEntry, bool) {
	entry, found, ambiguous := r.lookup(name)
	return entry, found && !ambiguous
}

func (r *MeshRoster) lookup(name string) (RosterEntry, bool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, dup := r.ambiguous[name]; dup {
		return RosterEntry{}, false, true
	}
	entry, ok := r.byName[name]
	return entry, ok, false
}

// LookupExact looks up a short private DNS name. Valid device labels are
// compared without changing hyphens. Legacy display names may still have a
// sanitized alias, but collisions with either legacy or exact names fail closed.
func (r *MeshRoster) LookupExact(name string) (RosterEntry, bool) {
	entry, found, ambiguous := r.lookupExact(name)
	return entry, found && !ambiguous
}

func (r *MeshRoster) lookupExact(name string) (RosterEntry, bool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ambiguous := r.exactAmbiguous[name]
	entry, found := r.byExactName[name]
	return entry, found, ambiguous
}

// LookupAsset returns the roster entry for a Cloud asset UUID. Asset UUIDs
// are globally unique, so unlike names this has no ambiguity case; unknown
// and non-canonical UUIDs return ok=false.
func (r *MeshRoster) LookupAsset(assetUUID string) (RosterEntry, bool) {
	if !canonicalRosterUUID(assetUUID) {
		return RosterEntry{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.byAsset[assetUUID]
	return entry, ok
}

// LookupDevice finds this authenticated device under the same UUID used by
// Cloud. The tenant remains part of the full principal and must still match.
func (r *MeshRoster) LookupDevice(device meshid.Device) (RosterEntry, bool) {
	if device == (meshid.Device{}) {
		return RosterEntry{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.byAsset[device.UUID().String()]
	if !ok || !entry.hasDevice || entry.device != device {
		return RosterEntry{}, false
	}
	return entry, true
}

// applyResponse rebuilds the cache from a roster response. Names that collide
// after normalization are recorded as ambiguous and never resolve. Device
// UUIDs become mesh identities through meshid against the authenticated
// tenant; rows with no device UUID — or a device component that is not a
// canonical UUID — stay in the directory as non-dialable rather than acquiring
// an invented address.
func (r *MeshRoster) applyResponse(generation uint64, resp *cloudpbv2.GetMeshRosterResponse) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if generation != r.generation {
		return
	}
	tenant, hasTenant := r.tenant, r.hasTenant

	byName := make(map[string]RosterEntry, len(resp.GetEntries()))
	byAsset := make(map[string]RosterEntry, len(resp.GetEntries()))
	ambiguous := make(map[string]struct{})
	byExactName := make(map[string]RosterEntry, len(resp.GetEntries()))
	exactAmbiguous := make(map[string]struct{})
	for _, e := range resp.GetEntries() {
		n := mesh.Normalize(e.GetName())

		entry := RosterEntry{Name: n, DeviceUUID: e.GetAssetId(), AssetUUID: e.GetAssetId()}
		if hasTenant && entry.DeviceUUID != "" {
			if device, err := meshid.ParseDevice(tenant, entry.DeviceUUID); err == nil {
				entry.device, entry.hasDevice = device, true
			}
		}
		if !canonicalRosterUUID(entry.DeviceUUID) {
			continue
		}
		if exact, valid := meshname.ShortDeviceLabel(e.GetName()); valid {
			entry.ShortName = exact
			if existing, ok := byExactName[exact]; ok && existing.AssetUUID != entry.AssetUUID {
				exactAmbiguous[exact] = struct{}{}
			} else {
				byExactName[exact] = entry
			}
		}
		byAsset[entry.AssetUUID] = entry
		if n == "" {
			continue
		}
		if existing, ok := byName[n]; ok && existing.AssetUUID != entry.AssetUUID {
			ambiguous[n] = struct{}{}
		} else {
			byName[n] = entry
		}
	}
	r.slug = mesh.Normalize(resp.GetOrgSlug())
	r.byName = byName
	r.byAsset = byAsset
	r.ambiguous = ambiguous
	r.byExactName = byExactName
	r.exactAmbiguous = exactAmbiguous
	if self, err := meshid.DeviceFromPrincipal(r.identity); err == nil {
		name := byAsset[self.UUID().String()].ShortName
		if _, ambiguous := exactAmbiguous[name]; ambiguous {
			name = ""
		}
		r.rememberLocalName(registeredDeviceName{Principal: r.identity, Name: name})
	}
}

func canonicalRosterUUID(text string) bool {
	_, err := meshid.ParseDeviceUUID(text)
	return err == nil
}

// Sync performs one GetMeshRoster refresh over the cloud gRPC endpoint using
// the device's certificate directly. Cloud scopes the response to the tenant
// authenticated at its device mTLS listener.
func (r *MeshRoster) Sync(ctx context.Context) error {
	r.mu.RLock()
	cloudURL, identity, generation := r.cloudURL, r.identity, r.generation
	certPEM, keyPEM, chainPEM := r.certPEM, r.keyPEM, r.chainPEM
	r.mu.RUnlock()

	if identity == "" {
		// No provisioned device identity yet: there is no certificate to
		// authenticate a roster call with, so don't dial at all — this is
		// expected before enrollment (and briefly during BLE first-boot
		// provisioning) and would otherwise just fail the RPC every tick.
		r.logger.Debug("wendy net roster: skipping sync, device identity not provisioned")
		return nil
	}

	endpoint, err := cloudrelay.DeviceEndpoint(cloudURL, os.Getenv("WENDY_DEVICE_CLOUD_URL"))
	if err != nil {
		return err
	}
	conn, err := cloudrelay.DialCloud(endpoint, certPEM, chainPEM, []byte(keyPEM))
	if err != nil {
		return err
	}
	defer conn.Close()
	client := cloudpbv2.NewMeshRosterServiceClient(conn)
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.GetMeshRoster(callCtx, &cloudpbv2.GetMeshRosterRequest{})
	if err != nil {
		return err
	}
	r.applyResponse(generation, resp)
	return nil
}

// RunSync refreshes immediately, then every interval, until ctx is done.
// Failures retry sooner with bounded backoff, preserving confirmed names.
func (r *MeshRoster) RunSync(ctx context.Context, interval time.Duration, publishName func(principal, name string)) {
	publish := func() {
		r.mu.RLock()
		principal, local := r.identity, r.localName
		r.mu.RUnlock()
		if principal == "" || publishName == nil {
			return
		}
		name := ""
		if local.Principal == principal {
			name = local.Name
		}
		publishName(principal, name)
	}
	publish()
	const firstRetry = 5 * time.Second
	retry := firstRetry
	for {
		err := r.Sync(ctx)
		if ctx.Err() != nil {
			return
		}
		delay := interval
		if err != nil {
			r.logger.Warn("wendy net roster sync failed", zap.Error(err))
			delay = min(retry, interval)
			retry = min(2*retry, time.Minute)
		} else {
			retry = firstRetry
		}
		publish()
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
		case <-timer.C:
		case <-r.refresh:
		}
		timer.Stop()
		if ctx.Err() != nil {
			return
		}
	}
}

// RequestSync wakes the roster worker after enrollment or a local rename.
func (r *MeshRoster) RequestSync() {
	select {
	case r.refresh <- struct{}{}:
	default:
	}
}
