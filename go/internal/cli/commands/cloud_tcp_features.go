package commands

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
)

const (
	cloudTCPFeatureTTL     = 10 * time.Second
	cloudTCPFeatureTimeout = 2 * time.Second
)

type cloudTCPFeatures struct {
	last  atomic.Pointer[cloudTCPFeatureObservation]
	probe singleflight.Group
}

type cloudTCPFeatureObservation struct {
	halfClose bool
	expires   time.Time
}

func cloudTCPFeaturesFor(ctx context.Context, auth *config.AuthConfig, asset cloudDiscoveryDevice) *cloudTCPFeatures {
	g, shared := ctx.Value(tunnelContextKey{}).(*tunnelGeneration)
	if !shared || !cloudTunnelOnly(ctx) {
		return new(cloudTCPFeatures)
	}
	// The VPN owns its flow channels separately. Share only their observation
	// with the management probe for the same credential, asset and device.
	key, err := desktopConnectionKey(auth, asset.key+"\x00"+cloudDiscoveryIdentity(auth, asset).IdentityKey())
	if err != nil {
		return new(cloudTCPFeatures)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return new(cloudTCPFeatures)
	}
	if existing := g.tcpFeatures[key]; existing != nil {
		return existing
	}
	if g.tcpFeatures == nil {
		g.tcpFeatures = make(map[string]*cloudTCPFeatures)
	}
	features := new(cloudTCPFeatures)
	g.tcpFeatures[key] = features
	return features
}

func (f *cloudTCPFeatures) observeVersion(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	err := invoke(ctx, method, req, reply, cc, opts...)
	if method == agentpb.WendyAgentService_GetAgentVersion_FullMethodName {
		version, _ := reply.(*agentpb.GetAgentVersionResponse)
		f.last.Store(&cloudTCPFeatureObservation{
			halfClose: err == nil && slices.Contains(version.GetFeatureset(), cloudrelay.TCPHalfCloseFeature),
			expires:   time.Now().Add(cloudTCPFeatureTTL),
		})
	}
	return err
}

// TODO(2026-11-23): Make half-close unconditional once enough devices have
// upgraded that every supported agent, including rollback targets, can drain
// its final response after receiving caller EOF.
func (f *cloudTCPFeatures) halfClose(ctx context.Context, agent agentpb.WendyAgentServiceClient) bool {
	if ctx.Err() != nil {
		return false
	}
	if observation := f.last.Load(); observation != nil && time.Now().Before(observation.expires) {
		return observation.halfClose
	}
	result := f.probe.DoChan("", func() (any, error) {
		if observation := f.last.Load(); observation != nil && time.Now().Before(observation.expires) {
			return observation.halfClose, nil
		}
		// The channel owns this bounded probe. One socket abandoning its
		// dial should not cut the check short for the other waiting sockets.
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloudTCPFeatureTimeout)
		defer cancel()
		version, err := agent.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
		return err == nil && slices.Contains(version.GetFeatureset(), cloudrelay.TCPHalfCloseFeature), nil
	})
	select {
	case <-ctx.Done():
		return false
	case r := <-result:
		return r.Val.(bool)
	}
}
