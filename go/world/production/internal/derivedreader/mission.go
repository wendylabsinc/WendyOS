package derivedreader

import (
	"encoding/json"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/missiontrigger"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// MissionInput preserves the Go canonical TriggerRef bytes for a client that
// reviews an observed location. The enclosing Item retains its exact original
// envelope. This is not a proposal, task, or authorization to move.
type MissionInput struct {
	Version            int        `json:"version"`
	TriggerJSON        []byte     `json:"trigger_json"`
	TriggerSHA256      string     `json:"trigger_sha256"`
	Target             [3]float64 `json:"target"`
	EnvelopeSHA256     string     `json:"envelope_sha256"`
	ResultSchema       string     `json:"result_schema"`
	SampleValidUntil   time.Time  `json:"sample_valid_until"`
	PhysicalValidUntil time.Time  `json:"physical_valid_until"`
}

func prepareMissionInput(v serviceorigin.VerifiedRecord, envelope []byte, c Config) (*MissionInput, error) {
	prepared, err := missiontrigger.Prepare(missiontrigger.Input{Verified: v, Envelope: envelope},
		missiontrigger.Binding{Tenant: c.Tenant, Site: c.Site, Frame: c.Frame, FloorY: *c.MissionFloorY})
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(prepared.Trigger)
	if err != nil {
		return nil, err
	}
	return &MissionInput{Version: 1, TriggerJSON: raw, TriggerSHA256: prepared.Trigger.SHA256,
		Target: prepared.Target, EnvelopeSHA256: prepared.EnvelopeSHA256, ResultSchema: prepared.ResultSchema,
		SampleValidUntil: prepared.SampleValidUntil, PhysicalValidUntil: prepared.PhysicalValidUntil}, nil
}
