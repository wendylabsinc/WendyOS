package robotprobe

import (
	"context"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

type fakeLive struct {
	stats *LiveHostStats
	err   error
}

func (f fakeLive) LiveHostStats(context.Context) (*LiveHostStats, error) { return f.stats, f.err }

func liveEnv(source LiveHostSource) *robotinspect.Env {
	return robotinspect.NewEnv().Offer(robotinspect.RequirementHostStats, source)
}

func orinLive() *LiveHostStats {
	gpuTemp := 47.5
	return &LiveHostStats{
		MemoryAvailableBytes: 9_500_000_000,
		ThermalZones: []ThermalZone{
			{Name: "cpu-thermal", Celsius: 52.4},
			{Name: "tj-thermal", Celsius: 61.8},
			{Name: "soc0-thermal", Celsius: 49.1},
		},
		GPUs: []GPUStats{{Index: 0, Name: "Orin", UtilPercent: 12.5, Celsius: &gpuTemp}},
	}
}

func TestThermalReportsEveryZoneAndNamesTheHottest(t *testing.T) {
	properties, err := Thermal{}.Observe(context.Background(), liveEnv(fakeLive{stats: orinLive()}))
	if err != nil {
		t.Fatal(err)
	}

	if got := findIn(t, properties, "thermal.cpu-thermal").Observations[0].Quantity.Value(); got != 52.4 {
		t.Errorf("cpu zone = %v, want 52.4", got)
	}
	hottest := findIn(t, properties, "thermal.max").Observations[0]
	if hottest.Quantity.Value() != 61.8 {
		t.Errorf("hottest = %v, want 61.8", hottest.Quantity.Value())
	}
	if got := hottest.Conditions["zone"]; got != "tj-thermal" {
		t.Errorf("hottest zone = %q, want tj-thermal", got)
	}

	// A temperature is a reading at a moment, not a sample over a window, and it is a
	// measurement rather than something the host claims.
	if hottest.Kind != robotinspect.Measured {
		t.Errorf("kind = %q, want measured", hottest.Kind)
	}
	if !hottest.Instant() {
		t.Error("a temperature should be recorded as an instant reading")
	}
	if hottest.Sampling != nil {
		t.Error("a level carries no sampling window")
	}

	if got := findIn(t, properties, "compute.memory.available").Observations[0].Quantity.Value(); got != 9_500_000_000 {
		t.Errorf("memory available = %v", got)
	}
	findIn(t, properties, "compute.gpu.0.utilisation")
	if got := findIn(t, properties, "compute.gpu.0.temperature").Observations[0].Quantity.Value(); got != 47.5 {
		t.Errorf("gpu temperature = %v, want 47.5", got)
	}
}

// A GPU that reports load and no temperature — an Orin does exactly this — leaves the row
// with a reason rather than leaving it out.
func TestThermalReportsAGPUWithoutATemperature(t *testing.T) {
	stats := orinLive()
	stats.GPUs[0].Celsius = nil
	properties, err := Thermal{}.Observe(context.Background(), liveEnv(fakeLive{stats: stats}))
	if err != nil {
		t.Fatal(err)
	}
	temperature := findIn(t, properties, "compute.gpu.0.temperature")
	if temperature.Unknown == nil || temperature.Unknown.Reason != robotinspect.ReasonSourceAbsent {
		t.Errorf("gpu temperature = %+v, want an unknown", temperature.Unknown)
	}
	findIn(t, properties, "compute.gpu.0.utilisation")
}

func TestThermalReportsAHostWithNoZones(t *testing.T) {
	properties, err := Thermal{}.Observe(context.Background(),
		liveEnv(fakeLive{stats: &LiveHostStats{MemoryAvailableBytes: 1 << 30}}))
	if err != nil {
		t.Fatal(err)
	}
	max := findIn(t, properties, "thermal.max")
	if max.Unknown == nil || max.Unknown.Reason != robotinspect.ReasonSourceAbsent {
		t.Errorf("thermal.max = %+v, want an unknown", max.Unknown)
	}
}

// An agent too old for the RPC is a finding about the fleet, not a failed inspection.
func TestThermalReportsAnOldAgentAsAFinding(t *testing.T) {
	properties, err := Thermal{}.Observe(context.Background(),
		liveEnv(fakeLive{err: errors.New("the device's agent does not support resource stats")}))
	if err != nil {
		t.Fatalf("an old agent should not fail the probe: %v", err)
	}
	for _, id := range (Thermal{}).Provides() {
		p := findIn(t, properties, id)
		if p.Unknown == nil || p.Unknown.Reason != robotinspect.ReasonProbeFailed {
			t.Errorf("%s = %+v, want an unknown", id, p.Unknown)
		}
	}
}

func TestThermalKeepsEveryPromise(t *testing.T) {
	properties, err := Thermal{}.Observe(context.Background(), liveEnv(fakeLive{stats: orinLive()}))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range properties {
		seen[p.ID] = true
	}
	for _, promised := range (Thermal{}).Provides() {
		if !seen[promised] {
			t.Errorf("Provides names %q and no row was emitted", promised)
		}
	}
}

func TestThermalIsPassive(t *testing.T) {
	if got := (Thermal{}).Class(); got != robotinspect.ClassPassive {
		t.Errorf("class = %q, want passive", got)
	}
}
