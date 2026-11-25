//go:build !world_simulation_faults

package gateway

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

func validateSimulationFaults(cfg config) error {
	if cfg.SimulationFaults != nil {
		return errors.New("simulation faults require a separately tagged test binary")
	}
	return nil
}
func applySimulationFaults(cfg config, _ *authority.ReplayCache, _ *tasking.Store) error {
	return validateSimulationFaults(cfg)
}
