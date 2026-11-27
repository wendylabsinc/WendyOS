package agentmodule

import (
	"bytes"
	"context"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func (m *Manager) readClock(ctx context.Context, scope serviceorigin.Scope, nonce []byte, fence *appservice.Fence) (*appv1.ClockReply, error) {
	if len(nonce) != appv1.ClockNonceSize {
		return nil, ErrDenied
	}
	var reply *appv1.ClockReply
	err := m.accessClock(ctx, scope, fence, func(ctx context.Context) error {
		// The source clock may perform bounded I/O. Sample physical time after
		// it; source time is never promoted into certificate authority time.
		sample := m.sampleNow(ctx)
		w, err := m.physicalNow()
		if err != nil || sample.IsZero() || sample.UnixNano() <= 0 || !time.Unix(0, sample.UnixNano()).Equal(sample) || !time.Unix(0, w.Earliest.UnixNano()).Equal(w.Earliest) || !time.Unix(0, w.Latest.UnixNano()).Equal(w.Latest) || ctx.Err() != nil {
			return ErrDenied
		}
		reply = &appv1.ClockReply{Nonce: bytes.Clone(nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: w.Earliest.UnixNano(), LatestUnixNs: w.Latest.UnixNano(), BootId: w.BootID, MonotonicNs: int64(w.Monotonic), Source: w.Source}, SampleClockDomain: m.cfg.Policy.SampleClockDomain, SampleUnixNs: sample.UnixNano(), Tenant: scope.Tenant, Site: scope.Site, Device: scope.Device, App: scope.App, Service: scope.Service, ExecutionDomain: scope.ExecutionDomain, DeploymentGeneration: scope.DeploymentGeneration, ServerBootId: m.clockIncarnation}
		_, err = appv1.DecodeClockReply(reply)
		return err
	})
	if err != nil {
		return nil, err
	}
	return reply, nil
}

// Preserve the already charged sample bytes. Clients account for the entire
// RPC duration with their suspend-inclusive local clock; refreshing the reply
// here would both change that sample and potentially enlarge its protobuf size.
func (m *Manager) clockResponseCurrent(scope serviceorigin.Scope, out *appv1.ClockReply) bool {
	original, err := appv1.DecodeClockReply(out)
	if err != nil || out.ServerBootId != m.clockIncarnation {
		return false
	}
	return m.clockIntervalCurrent(scope, original)
}
