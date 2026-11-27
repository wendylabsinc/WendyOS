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
	halfClose, mux bool
	expires        time.Time
}

// cloudTCPFlow is what a device's agent supports for a raw TCP flow.
type cloudTCPFlow struct {
	halfClose bool
	// mux: the agent advertises TCPMuxFeature, so the flow is opened with
	// cloudrelay.OpenRawTCP.
	mux bool
}

func (o *cloudTCPFeatureObservation) fresh() bool {
	return o != nil && time.Now().Before(o.expires)
}

type cloudTCPFeatureProbeKey struct{}

// WithCloudTCPFeatureProbe uses an existing management channel to refresh
// capabilities when a TCP flow opens. The dialer reads only its own scoped
// observation, which the management channel's GetAgentVersion can populate.
func WithCloudTCPFeatureProbe(ctx context.Context, probe func(context.Context) error) context.Context {
	return context.WithValue(ctx, cloudTCPFeatureProbeKey{}, probe)
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
			mux:       err == nil && slices.Contains(version.GetFeatureset(), cloudrelay.TCPMuxFeature),
			expires:   time.Now().Add(cloudTCPFeatureTTL),
		})
	}
	return err
}

// TODO(2026-11-23): Make half-close unconditional once enough devices have
// upgraded that every supported agent, including rollback targets, can drain
// its final response after receiving caller EOF.
func (f *cloudTCPFeatures) flow(ctx context.Context, agent agentpb.WendyAgentServiceClient) cloudTCPFlow {
	if ctx.Err() != nil {
		return cloudTCPFlow{}
	}
	if observation := f.last.Load(); observation.fresh() {
		return cloudTCPFlow{observation.halfClose, observation.mux}
	}
	result := f.probe.DoChan("", func() (any, error) {
		if observation := f.last.Load(); observation.fresh() {
			return cloudTCPFlow{observation.halfClose, observation.mux}, nil
		}
		// The channel owns this bounded probe. One socket abandoning its
		// dial should not cut the check short for the other waiting sockets.
		probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cloudTCPFeatureTimeout)
		defer cancel()
		var err error
		if probe, ok := ctx.Value(cloudTCPFeatureProbeKey{}).(func(context.Context) error); ok {
			err = probe(probeCtx)
		} else {
			_, err = agent.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
		}
		observation := f.last.Load()
		if observation.fresh() {
			if err != nil || probeCtx.Err() != nil {
				return cloudTCPFlow{}, nil
			}
			return cloudTCPFlow{observation.halfClose, observation.mux}, nil
		}
		// The shared probe may have reached another credential or asset binding,
		// or failed before invoking an RPC. Throttle retries in this scope.
		f.last.CompareAndSwap(observation, &cloudTCPFeatureObservation{expires: time.Now().Add(cloudTCPFeatureTTL)})
		return cloudTCPFlow{}, nil
	})
	select {
	case <-ctx.Done():
		return cloudTCPFlow{}
	case r := <-result:
		return r.Val.(cloudTCPFlow)
	}
}
