package agentmodule

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// HostOptions is not part of commissioned authority or any app API. The host
// owns the optional sink and its separate writer lifecycle.
type HostOptions struct{ Trace *rpctrace.Sink }

func LoadWithOptions(ctx context.Context, path string, identity Identity, peer appservice.PeerCheck, options HostOptions) (*Manager, error) {
	return load(ctx, path, identity, peer, false, options)
}

func (m *Manager) withCurrent(ctx context.Context, token uint64, run func() error) error {
	return traceIdentity(ctx, token, run, m.deps.Identity.WithCurrent)
}

func (m *Manager) withCurrentClock(ctx context.Context, token uint64, run func() error) error {
	enter := m.deps.Identity.WithCurrent
	if clock, ok := m.deps.Identity.(ClockIdentity); ok {
		enter = clock.WithCurrentClock
	}
	return traceIdentity(ctx, token, run, enter)
}

func traceIdentity(ctx context.Context, token uint64, run func() error, enter func(uint64, func() error) error) error {
	waiting := rpctrace.Start(ctx, rpctrace.StageIdentityWait)
	entered := false
	defer func() {
		if !entered {
			waiting()
		}
	}()
	return enter(token, func() error {
		entered = true
		waiting()
		// This is the existing callback while provisioning holds its fence.
		// The final unlock after callback return is not included.
		defer rpctrace.Start(ctx, rpctrace.StageIdentityHeld)()
		return run()
	})
}

func (m *Manager) configCurrent(ctx context.Context) error {
	defer rpctrace.Start(ctx, rpctrace.StageConfig)()
	return m.deps.PolicyCurrent(ctx)
}

func (m *Manager) sampleNow(ctx context.Context) time.Time {
	defer rpctrace.Start(ctx, rpctrace.StageSourceClock)()
	return m.deps.SampleNow()
}

func (m *Manager) verifyRecord(ctx context.Context, signed serviceorigin.SignedRecord) (serviceorigin.VerifiedRecord, error) {
	defer rpctrace.Start(ctx, rpctrace.StageVerify)()
	return m.verifier.Verify(ctx, signed)
}
