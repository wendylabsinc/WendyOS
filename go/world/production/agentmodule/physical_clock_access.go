package agentmodule

import (
	"bytes"
	"context"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

// readPhysicalClock uses the same commissioned reader/lifecycle fence as Clock,
// but never invokes SampleNow. Physical authority availability is independent
// of a blocked or expired source clock and does not establish source freshness.
func (m *Manager) readPhysicalClock(ctx context.Context, scope serviceorigin.Scope, nonce []byte, fence *appservice.Fence) (*appv1.PhysicalClockReply, error) {
	if len(nonce) != appv1.ClockNonceSize {
		return nil, ErrDenied
	}
	var reply *appv1.PhysicalClockReply
	err := m.accessClock(ctx, scope, fence, func(ctx context.Context) error {
		w, err := m.physicalNow()
		if err != nil || !time.Unix(0, w.Earliest.UnixNano()).Equal(w.Earliest) || !time.Unix(0, w.Latest.UnixNano()).Equal(w.Latest) || ctx.Err() != nil {
			return ErrDenied
		}
		reply = &appv1.PhysicalClockReply{Nonce: bytes.Clone(nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: w.Earliest.UnixNano(), LatestUnixNs: w.Latest.UnixNano(), BootId: w.BootID, MonotonicNs: int64(w.Monotonic), Source: w.Source}, Tenant: scope.Tenant, Site: scope.Site, Device: scope.Device, App: scope.App, Service: scope.Service, ExecutionDomain: scope.ExecutionDomain, DeploymentGeneration: scope.DeploymentGeneration, ServerBootId: m.clockIncarnation}
		_, err = appv1.DecodePhysicalClockReply(reply)
		return err
	})
	if err != nil {
		return nil, err
	}
	return reply, nil
}

func (m *Manager) physicalClockResponseCurrent(scope serviceorigin.Scope, out *appv1.PhysicalClockReply) bool {
	original, err := appv1.DecodePhysicalClockReply(out)
	if err != nil || out.ServerBootId != m.clockIncarnation {
		return false
	}
	return m.clockIntervalCurrent(scope, original)
}

// The final clock observation may itself block. Check the commissioned policy
// window against this interval, not only the earlier policyCurrent read. Never
// refresh the already charged reply bytes to hide a delayed response.
func (m *Manager) clockIntervalCurrent(scope serviceorigin.Scope, original trustedtime.Interval) bool {
	now, err := m.physicalNow()
	if err != nil {
		return false
	}
	if !m.readerCurrent(scope, now) {
		return false
	}
	return now.BootID == original.BootID && now.Monotonic >= original.Monotonic && now.Monotonic-original.Monotonic <= appv1.MaxClockResponseAge && !now.Earliest.Before(original.Earliest) && !now.Latest.Before(original.Latest)
}
