package agentmodule

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func knownSchema(name string) bool {
	switch name {
	case "wendy.sensor.scalar.v1", "wendy.sensor.image.v1", "wendy.sensor.audio.v1", "wendy.inference.claim.v1", sensorrecord.ComponentType, resultrecord.ComponentType:
		return true
	}
	return false
}

func (m *Manager) admitSchema(ctx context.Context, v serviceorigin.VerifiedRecord) error {
	defer rpctrace.Start(ctx, rpctrace.StageSchema)()
	if err := m.policyCurrent(ctx); err != nil {
		return err
	}
	c := v.Component()
	if !knownSchema(c.Key.Type) || c.Timing.ClockDomain != m.cfg.Policy.SampleClockDomain || !c.Timing.ErrorKnown {
		return ErrDenied
	}
	frame := false
	for _, f := range m.cfg.Policy.Frames {
		if f == c.Frame {
			frame = true
			break
		}
	}
	if !frame {
		return ErrDenied
	}
	if c.Tombstone {
		return nil
	}
	if c.Key.Type == resultrecord.ComponentType {
		_, err := m.derivedFromArchive(ctx, v)
		return err
	}
	owner := m.owner(serviceaccess.Source{App: c.Key.Source.App, Service: c.Key.Source.Service, ComponentType: c.Key.Type, Stream: c.Key.Source.Stream})
	if c.Key.Type == sensorrecord.ComponentType {
		capture, e := sensorrecord.FromComponent(c)
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
		// The source clock can pause, but an old capture must not become live
		// forever. This conservative physical bound includes authority-clock
		// uncertainty and uses the original scene assertion, never receipt time.
		if !m.captureCurrent(capture) {
			return ErrDenied
		}
		ref, e := capture.Evidence()
		if e != nil {
			return ErrDenied
		}
		if ref != nil {
			if m.evidence == nil {
				return ErrDenied
			}
			_, data, e := m.readInputMedia(ctx, owner, ref.SHA256, ref.Size, ref.ContentType)
			if e != nil || sensorrecord.ValidateMedia(capture, data) != nil {
				return ErrDenied
			}
		}
		if !m.captureCurrent(capture) {
			return ErrDenied
		}
		return nil
	}
	o := world.Observation{ID: c.Key.Entity, Tenant: c.Key.Tenant, Site: c.Key.Site, Source: c.Key.Source, Frame: c.Frame, Timing: c.Timing, Provenance: c.Provenance}
	var target any
	var media *mediaPayload
	switch c.Key.Type {
	case "wendy.sensor.scalar.v1":
		o.Scalar = &world.ScalarSample{}
		target = o.Scalar
	case "wendy.sensor.image.v1":
		media = &mediaPayload{}
		target = media
	case "wendy.sensor.audio.v1":
		media = &mediaPayload{}
		target = media
	case "wendy.inference.claim.v1":
		o.Claim = &world.DerivedClaim{}
		target = o.Claim
	default:
		return ErrDenied
	}
	if jws.DecodeStrict(c.Payload, target) != nil {
		return ErrDenied
	}
	if media != nil {
		sample := &world.MediaSample{SHA256: media.SHA256, ContentType: media.ContentType, Duration: media.Duration, CalibrationRevision: media.CalibrationRevision}
		if c.Key.Type == "wendy.sensor.image.v1" {
			o.Image = sample
		} else {
			o.Audio = sample
		}
	}
	if o.Validate() != nil {
		return ErrDenied
	}
	if media != nil {
		if m.evidence == nil || media.Size <= 0 {
			return ErrDenied
		}
		if _, err := m.matchSchemaMedia(ctx, owner, media.SHA256, media.Size, media.ContentType); err != nil {
			return ErrDenied
		}
	}
	return nil
}

func (m *Manager) captureCurrent(capture sensorrecord.Capture) bool {
	window, e := m.physicalNow()
	return e == nil && sensorrecord.PhysicalCurrent(capture, window) == nil
}

// Private image/audio wire payloads include the exact byte size so a digest
// reference cannot be admitted before the corresponding upload is complete.
type mediaPayload struct {
	SHA256              string        `json:"sha256"`
	ContentType         string        `json:"content_type"`
	Duration            time.Duration `json:"duration_ns"`
	CalibrationRevision string        `json:"calibration_revision"`
	Size                int64         `json:"size"`
}
