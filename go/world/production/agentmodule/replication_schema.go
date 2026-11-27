package agentmodule

import (
	"context"

	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// Remote typed results are the enrolled origin's attested inference. Inline
// captures retain the same commissioned simulation source. Neither path claims
// to fetch camera/audio media. Local publication keeps full input validation.
func (m *Manager) admitReplicaSchema(ctx context.Context, v serviceorigin.VerifiedRecord) error {
	if v.Record.Publication.ComponentType != resultrecord.ComponentType && v.Record.Publication.ComponentType != sensorrecord.ComponentType {
		return m.admitSchema(ctx, v)
	}
	defer rpctrace.Start(ctx, rpctrace.StageSchema)()
	if m.policyCurrent(ctx) != nil || v.Record.Scope.Device == m.cfg.Policy.Device || v.Record.Scope.Tenant != m.cfg.Policy.Tenant || v.Record.Scope.Site != m.cfg.Policy.Site {
		return ErrDenied
	}
	p := v.Record.Publication
	if p.Timing.ClockDomain != m.cfg.Policy.SampleClockDomain || !p.Timing.ErrorKnown {
		return ErrDenied
	}
	frame := false
	for _, f := range m.cfg.Policy.Frames {
		if f == p.Frame {
			frame = true
			break
		}
	}
	if !frame {
		return ErrDenied
	}
	if _, e := replication.RecordInterpretationUntil(v.Record); e != nil {
		return ErrDenied
	}
	if p.ComponentType == sensorrecord.ComponentType && !p.Tombstone {
		capture, e := sensorrecord.FromComponent(v.Component())
		if e != nil || m.cfg.Policy.ExecutionDomain != "simulation" || m.cfg.CaptureSource == nil {
			return ErrDenied
		}
		sample, e := capture.Sample()
		if e != nil {
			return ErrDenied
		}
		binding := m.cfg.CaptureSource
		if capture.SceneBootID != binding.BootID || capture.SimulationAnchorUnixNS != binding.AnchorUnixNS || sample.RunID != binding.RunID || sample.PackageSHA256 != binding.PackageSHA256 || sample.Frame != binding.Frame {
			return ErrDenied
		}
		// Expired originals may repair version high-water. Snapshot and the
		// final Query check apply LiveDeadlines before exposing a live capture.
	}
	return nil
}
