package fusion

import (
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// CaptureDecoder belongs to the caller's authenticated source adapter. It must
// bind the exact unchanged component to an original verified device proof. This
// domain package does not verify certificates or reconstruct signed records.
type CaptureDecoder func(world.Component) (CapturedSample, error)

// CapturedSample separates native sensor sequence from agent source/version.
// Sample describes the evidence actually consumed. OriginalEvidence links to
// an original rendered PNG when a SensorLink JPEG is the delivered evidence.
type CapturedSample struct {
	Sample           sensing.Sample
	ProofSHA256      string
	OriginalEvidence *sensing.EvidenceRef
	SceneBootID      string
	SimulationAnchor time.Time
}

// CaptureOrigin persists the exact proof and capture mapping beside the agent
// Source/Version in a derived input. It does not imply transfer of source media.
type CaptureOrigin struct {
	ProofSHA256      string               `json:"proof_sha256"`
	SceneBootID      string               `json:"scene_boot_id"`
	SimulationAnchor time.Time            `json:"simulation_anchor"`
	OriginalEvidence *sensing.EvidenceRef `json:"original_evidence,omitempty"`
}

func (s CapturedSample) origin() CaptureOrigin {
	o := CaptureOrigin{ProofSHA256: s.ProofSHA256, SceneBootID: s.SceneBootID, SimulationAnchor: s.SimulationAnchor}
	if s.OriginalEvidence != nil {
		ref := *s.OriginalEvidence
		o.OriginalEvidence = &ref
	}
	return o
}

func validateCaptured(c world.Component, decoded CapturedSample) error {
	s := decoded.Sample
	anchor := decoded.SimulationAnchor.UnixNano()
	if s.Validate() != nil || s.SensorLink != nil || !sensing.DigestValid(decoded.ProofSHA256) || decoded.SceneBootID == "" || len(decoded.SceneBootID) > 128 || decoded.SimulationAnchor.IsZero() || anchor <= 0 || !time.Unix(0, anchor).Equal(decoded.SimulationAnchor) || s.SampleNS > math.MaxInt64-anchor {
		return errors.New("invalid authenticated capture metadata")
	}
	if c.Tombstone || c.Key.Source.Validate() != nil || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.TTL <= 0 || c.Key.Source.Stream != s.SensorID || c.Key.Entity != "sensor/"+s.SensorID || c.Key.Site != s.SiteID || c.Frame != s.Frame || c.Timing.Validate() != nil || !c.Timing.SampleTime.Equal(decoded.SimulationAnchor.Add(time.Duration(s.SampleNS))) {
		return errors.New("capture adapter changed source, scope or source time")
	}
	p := c.Provenance
	if p.Validate() != nil || p.Origin != world.Simulated || p.Simulation == nil || p.Simulation.RunID != s.RunID || p.Simulation.ScenarioVersion != s.ScenarioVersion || p.Simulation.Seed != s.Seed || p.Simulation.ClockDomain != "simulation:"+s.RunID || c.Timing.ClockDomain != p.Simulation.ClockDomain || p.ModelID != s.SensorModel || p.ModelVersion != s.ScenarioVersion {
		return errors.New("capture adapter changed simulation ancestry")
	}
	if original := decoded.OriginalEvidence; original != nil {
		if original.Validate() != nil || s.Camera == nil || s.Evidence == nil || original.ContentType != "image/png" || s.Evidence.ContentType != "image/jpeg" {
			return errors.New("original source link requires PNG to received JPEG conversion")
		}
	}
	return nil
}
