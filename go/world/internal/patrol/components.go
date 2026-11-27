package patrol

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"time"
)

const CoverageType = "wendy.nav.patrol.coverage.v1"
const StatusType = "wendy.nav.patrol.status.v1"

// Components retain patrol progress through the existing World replica format.
// Their clock is executor wall time; sensor visit timestamps remain simulation seconds.
func Components(state State, runID string, frame world.FrameRef, sequence uint64, now time.Time) ([]world.Component, error) {
	source := world.Source{Device: "go2", App: "world-patrol", Service: "patrol", Stream: "progress"}
	clock := "simulation-executor-wall:" + runID
	output := []world.Component{}
	for _, item := range []struct {
		kind    string
		payload any
	}{{StatusType, state}, {CoverageType, map[string]any{"mission_id": state.MissionID, "rooms": state.Rooms}}} {
		raw, err := json.Marshal(item.payload)
		if err != nil {
			return nil, err
		}
		output = append(output, world.Component{Key: world.Key{Tenant: "simulation", Site: "studio-01", Entity: state.MissionID, Type: item.kind, Source: source}, Version: world.Version{Epoch: 1, Sequence: sequence}, Frame: frame, Timing: world.Timing{SampleTime: now, ClockDomain: clock, ErrorKnown: true}, TTL: 5 * time.Second, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "world-patrol-v1", Simulation: &world.SimulationContext{RunID: runID, ScenarioVersion: "go2-passive-patrol-v1", Seed: 7, ClockDomain: clock}}, Payload: raw})
	}
	return output, nil
}
