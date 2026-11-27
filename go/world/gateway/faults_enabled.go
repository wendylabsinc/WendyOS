//go:build world_simulation_faults

package gateway

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"strings"
	"time"
)

func validateSimulationFaults(cfg config) error {
	f := cfg.SimulationFaults
	if f == nil {
		return nil
	}
	if !cfg.AllowInsecureSimulation || !strings.HasPrefix(cfg.ClockDomain, "simulation:") || len(cfg.ClockDomain) <= 11 || f.ReplayCommitDelayMS < 0 || f.ReplayCommitDelayMS > 1000 || f.TaskCommitDelayMS < 0 || f.TaskCommitDelayMS > 1000 || (f.TaskCommitDelayMS > 0 && cfg.Tasks == nil) {
		return errors.New("invalid simulation-only storage fault configuration")
	}
	return nil
}
func applySimulationFaults(cfg config, replay *authority.ReplayCache, tasks *tasking.Store) error {
	if err := validateSimulationFaults(cfg); err != nil {
		return err
	}
	if cfg.SimulationFaults == nil {
		return nil
	}
	f := cfg.SimulationFaults
	if err := replay.SetCommitDelayForSimulation(time.Duration(f.ReplayCommitDelayMS) * time.Millisecond); err != nil {
		return err
	}
	if tasks != nil {
		return tasks.SetCommitDelayForSimulation(time.Duration(f.TaskCommitDelayMS) * time.Millisecond)
	}
	return nil
}
