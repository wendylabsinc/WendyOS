//go:build !world_simulation_faults

package gateway

import "testing"

func TestNormalBuildRefusesSimulationFaultConfiguration(t *testing.T) {
	if err := validateSimulationFaults(config{}); err != nil {
		t.Fatal(err)
	}
	if err := validateSimulationFaults(config{SimulationFaults: &simulationFaults{}}); err == nil {
		t.Fatal("normal binary accepted fault configuration")
	}
}
