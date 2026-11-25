//go:build world_simulation_faults

package gateway

import "testing"

func TestTaggedFaultsRequireExplicitSimulationAndBounds(t *testing.T) {
	good := config{AllowInsecureSimulation: true, ClockDomain: "simulation:load", Tasks: &taskConfig{}, SimulationFaults: &simulationFaults{ReplayCommitDelayMS: 10, TaskCommitDelayMS: 20}}
	if err := validateSimulationFaults(good); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*config){func(c *config) { c.AllowInsecureSimulation = false }, func(c *config) { c.ClockDomain = "physical" }, func(c *config) { c.Tasks = nil }, func(c *config) { c.SimulationFaults = &simulationFaults{ReplayCommitDelayMS: 1001} }, func(c *config) { c.SimulationFaults = &simulationFaults{TaskCommitDelayMS: -1} }} {
		bad := good
		change(&bad)
		if err := validateSimulationFaults(bad); err == nil {
			t.Fatal("unsafe fault configuration accepted")
		}
	}
}
