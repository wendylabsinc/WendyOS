package services

import (
	"context"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"go.uber.org/zap"
	"google.golang.org/grpc/metadata"
)

// LegacyMeshResolver preserves the numeric directory contract. Roster data is
// never shared across enrollments; an unavailable v1 roster leaves numeric DNS
// usable and friendly names unresolved, as on the original agent.
type LegacyMeshResolver struct {
	peers     *LegacyPeerDialer
	mu        sync.Mutex
	binding   CloudEnrollment
	slug      string
	names     map[string]int32
	ambiguous map[string]bool
	wake      chan struct{}
}

func NewLegacyMeshResolver(peers *LegacyPeerDialer) *LegacyMeshResolver {
	return &LegacyMeshResolver{peers: peers, wake: make(chan struct{}, 1)}
}
func (r *LegacyMeshResolver) LegacyOrgID() int32 { return r.peers.LegacyOrgID() }
func (r *LegacyMeshResolver) UpdateIdentity() {
	r.mu.Lock()
	r.binding = CloudEnrollment{}
	r.slug = ""
	r.names = nil
	r.ambiguous = nil
	r.mu.Unlock()
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
func (r *LegacyMeshResolver) ResolveLegacy(name, slug string) (int32, bool) {
	e, err := r.peers.provisioning.CloudEnrollment()
	if err != nil || e.Mode != CloudLegacy {
		return 0, false
	}
	r.mu.Lock()
	if r.binding != e || slug == "" || slug != r.slug {
		r.mu.Unlock()
		return 0, false
	}
	cached, ok := r.names[name]
	ambiguous := r.ambiguous[name]
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), lanBudget)
	defer cancel()
	devices, err := r.peers.browse(ctx)
	if err == nil {
		var found int32
		for _, d := range devices {
			if d.OrgID != e.OrganizationID || d.AssetID <= 0 || d.AssetID > 65534 || mesh.Normalize(d.MeshName) != name {
				continue
			}
			if found != 0 && found != d.AssetID {
				return 0, false
			}
			found = d.AssetID
		}
		if found > 0 {
			return found, true
		}
	}
	return cached, ok && !ambiguous
}
func (r *LegacyMeshResolver) apply(e CloudEnrollment, resp *cloudpb.GetMeshRosterResponse) {
	names := make(map[string]int32)
	ambiguous := make(map[string]bool)
	for _, row := range resp.GetEntries() {
		name := mesh.Normalize(row.GetName())
		id := row.GetAssetId()
		if name == "" || id <= 0 || id > 65534 {
			continue
		}
		if previous, ok := names[name]; ok && previous != id {
			ambiguous[name] = true
		}
		names[name] = id
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	current, err := r.peers.provisioning.CloudEnrollment()
	if err != nil || current != e || e.Mode != CloudLegacy {
		return
	}
	r.binding = e
	r.slug = mesh.Normalize(resp.GetOrgSlug())
	r.names = names
	r.ambiguous = ambiguous
}
func (r *LegacyMeshResolver) Sync(ctx context.Context) error {
	e, cert, chain, key, err := r.peers.provisioning.LegacyCloudCredentials()
	if err != nil {
		return err
	}
	if e.Mode != CloudLegacy {
		return nil
	}
	conn, proof, err := dialLegacyCloud(NewTunnelBrokerClient(r.peers.logger, e.Host, cert, key, chain, 0))
	if err != nil {
		return err
	}
	defer conn.Close()
	md, err := proof(cloudpb.MeshRosterService_GetMeshRoster_FullMethodName)
	if err != nil {
		return err
	}
	call, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, md), 10*time.Second)
	defer cancel()
	resp, err := cloudpb.NewMeshRosterServiceClient(conn).GetMeshRoster(call, &cloudpb.GetMeshRosterRequest{})
	if err != nil {
		return err
	}
	r.apply(e, resp)
	return nil
}
func (r *LegacyMeshResolver) RunSync(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := r.Sync(ctx); err != nil {
			r.peers.logger.Debug("legacy mesh roster unavailable", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-r.wake:
		}
	}
}

var _ mesh.LegacyResolver = (*LegacyMeshResolver)(nil)
var _ mesh.LegacyAppDialer = (*LegacyPeerDialer)(nil)
