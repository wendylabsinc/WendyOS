package derivedreader

import (
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/appclock"
)

// Clock records the observation used by this read. It is not a continuing
// clock, and its sample time does not advance while a simulation is paused.
type Clock struct {
	SampleTime           time.Time `json:"sample_time"`
	SampleClockDomain    string    `json:"sample_clock_domain"`
	PhysicalEarliest     time.Time `json:"physical_earliest"`
	PhysicalLatest       time.Time `json:"physical_latest"`
	ServerBootID         string    `json:"server_boot_id"`
	DeploymentGeneration uint64    `json:"deployment_generation"`
}

func observedClock(o appclock.Observation) *Clock {
	return &Clock{SampleTime: o.SampleTime, SampleClockDomain: o.SampleClockDomain,
		PhysicalEarliest: o.Physical.Earliest, PhysicalLatest: o.Physical.Latest,
		ServerBootID: o.ServerBootID, DeploymentGeneration: o.DeploymentGeneration}
}
