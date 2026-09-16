package services

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// MeshRoster caches this org's {normalized name -> asset id} directory and the
// org's own slug, refreshed from the cloud GetMeshRoster RPC. It is the
// off-LAN half of the hybrid friendly-name resolver.
type MeshRoster struct {
	logger   *zap.Logger
	cloudURL string
	orgID    int32
	assetID  int32
	certPEM  string
	keyPEM   string
	chainPEM string

	mu        sync.RWMutex
	slug      string
	byName    map[string]int32
	ambiguous map[string]struct{}
}

func NewMeshRoster(logger *zap.Logger, cloudURL string, orgID, assetID int32, certPEM, keyPEM, chainPEM string) *MeshRoster {
	return &MeshRoster{
		logger:    logger,
		cloudURL:  cloudURL,
		orgID:     orgID,
		assetID:   assetID,
		certPEM:   certPEM,
		keyPEM:    keyPEM,
		chainPEM:  chainPEM,
		byName:    make(map[string]int32),
		ambiguous: make(map[string]struct{}),
	}
}

func (r *MeshRoster) OrgSlug() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.slug
}

// UpdateIdentity swaps the identity used to authenticate GetMeshRoster calls.
// Called when BLE first-boot enrollment provisions the device while the
// agent is already running, so the boot-time snapshot (org=0/asset=0/empty
// chain) needs to be replaced with the freshly issued identity.
func (r *MeshRoster) UpdateIdentity(cloudURL string, orgID, assetID int32, certPEM, keyPEM, chainPEM string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cloudURL = cloudURL
	r.orgID = orgID
	r.assetID = assetID
	r.certPEM = certPEM
	r.keyPEM = keyPEM
	r.chainPEM = chainPEM
}

// Lookup returns the asset id for a normalized device name. Unknown and
// ambiguous (duplicate-normalized) names return ok=false.
func (r *MeshRoster) Lookup(name string) (int32, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, dup := r.ambiguous[name]; dup {
		return 0, false
	}
	id, ok := r.byName[name]
	return id, ok
}

// applyResponse rebuilds the cache from a roster response. Names that collide
// after normalization are recorded as ambiguous and never resolve.
func (r *MeshRoster) applyResponse(resp *cloudpb.GetMeshRosterResponse) {
	byName := make(map[string]int32, len(resp.GetEntries()))
	ambiguous := make(map[string]struct{})
	for _, e := range resp.GetEntries() {
		n := mesh.Normalize(e.GetName())
		if n == "" {
			continue
		}
		if existing, ok := byName[n]; ok && existing != e.GetAssetId() {
			ambiguous[n] = struct{}{}
			continue
		}
		byName[n] = e.GetAssetId()
	}
	r.mu.Lock()
	r.slug = mesh.Normalize(resp.GetOrgSlug())
	r.byName = byName
	r.ambiguous = ambiguous
	r.mu.Unlock()
}

// Sync performs one GetMeshRoster refresh over the cloud gRPC endpoint using
// the same asset-cert identity the tunnel broker client uses.
func (r *MeshRoster) Sync(ctx context.Context) error {
	r.mu.RLock()
	cloudURL := r.cloudURL
	orgID := r.orgID
	assetID := r.assetID
	certPEM := r.certPEM
	keyPEM := r.keyPEM
	chainPEM := r.chainPEM
	r.mu.RUnlock()

	if assetID == 0 {
		// Unprovisioned identity: no asset cert to authenticate with, so
		// don't bother dialing — this is expected before enrollment (and
		// briefly during BLE first-boot provisioning) and would otherwise
		// just fail the RPC every tick.
		r.logger.Debug("mesh roster: skipping sync, device not provisioned")
		return nil
	}

	dialOpts, requestMetadata, err := brokerDialOpts(r.logger, orgID, assetID, certPEM, keyPEM, chainPEM)
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(cloudURL, dialOpts...)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := cloudpb.NewMeshRosterServiceClient(conn)
	md, err := requestMetadata(cloudpb.MeshRosterService_GetMeshRoster_FullMethodName)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, md), 10*time.Second)
	defer cancel()
	resp, err := client.GetMeshRoster(callCtx, &cloudpb.GetMeshRosterRequest{})
	if err != nil {
		return err
	}
	r.applyResponse(resp)
	return nil
}

// RunSync refreshes immediately, then every interval, until ctx is done.
// Failures are logged and retried on the next tick; the cache keeps its last
// good contents so a transient cloud outage never breaks LAN-cached names.
func (r *MeshRoster) RunSync(ctx context.Context, interval time.Duration) {
	if err := r.Sync(ctx); err != nil {
		r.logger.Warn("mesh roster initial sync failed", zap.Error(err))
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := r.Sync(ctx); err != nil {
				r.logger.Warn("mesh roster sync failed", zap.Error(err))
			}
		}
	}
}
