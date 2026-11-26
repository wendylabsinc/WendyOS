package spatial_test

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// These tests hold the root package to the simulator every feature is
// proven against. Truth is read only to score.

// Every measurement of a real stream must have its own ancestry id, and a
// stream's worth of them must reduce to valid provenance.
func TestSimulatedStreamInputIDs(t *testing.T) {
	site, err := spatialsim.Office(1, spatialsim.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sim := world.SimulationContext{RunID: "run", ScenarioVersion: "office", Seed: 1, ClockDomain: "sim"}
	seen := map[string]spatial.Measurement{}
	var inputs, round []world.Input // round: the ranges of the first UWB round
	var first spatial.Range
	for m := range site.Stream(site.At(0), site.At(2*time.Second), spatialsim.Filter{}) {
		id := spatial.InputID(m)
		if other, dup := seen[id]; dup {
			t.Fatalf("input id %q is shared by %T and %T", id, other, m)
		}
		seen[id] = m
		in := world.Input{ID: id, Origin: world.Simulated, Simulation: &sim}
		inputs = append(inputs, in)
		if r, ok := m.(spatial.Range); ok {
			if len(round) == 0 {
				first = r
			}
			if r.Header == first.Header {
				round = append(round, in)
			}
		}
	}
	const minMeasurements, minRound = 500, 2
	if len(inputs) < minMeasurements || len(round) < minRound {
		t.Fatalf("stream too thin to test: %d measurements, %d ranges in one round", len(inputs), len(round))
	}
	if _, err := spatial.InferredProvenance("localize", "1", "test", round); err != nil {
		t.Fatalf("one UWB round as ancestry: %v", err)
	}
	reduced := spatial.ReduceInputs(inputs)
	if len(reduced) > spatial.MaxInputs {
		t.Fatalf("%d inputs after reduction", len(reduced))
	}
	if _, err := spatial.InferredProvenance("cslam", "1", "test", reduced); err != nil {
		t.Fatalf("reduced stream as ancestry: %v", err)
	}
}

// OdometryNoise.Covariance must describe the simulated odometry at every
// keyframe spacing a feature may choose: the squared error of an increment,
// divided by its predicted variance, averages 1 when the model is calibrated.
// Reading the coefficients as plain "error per metre" gives about 19 at 5 cm.
func TestOdometryCovarianceIsCalibrated(t *testing.T) {
	const (
		seeds          = 16
		minNormalised  = 0.6
		maxNormalised  = 1.6
		minIncrements  = 500
		runSeconds     = 120
		robotSpeedMPS  = 1.0
		nativePeriodMS = 50
	)
	for _, stepM := range []float64{0.05, 0.2, 1} {
		period := time.Duration(stepM/robotSpeedMPS*1000) * time.Millisecond
		if period < nativePeriodMS*time.Millisecond {
			t.Fatalf("step %g m is below the odometry rate", stepM)
		}
		var sum [3]float64
		var count [3]int
		for seed := uint64(1); seed <= seeds; seed++ {
			site, err := spatialsim.Corridor(seed, spatialsim.Options{})
			if err != nil {
				t.Fatal(err)
			}
			truth := site.Truth()
			var prevOdom spatial.Odometry
			var prevTrue geom.Pose2
			for at := time.Duration(0); at <= runSeconds*time.Second; at += period {
				odom, ok := site.Odometry(spatialsim.Rover1, site.At(at))
				state, okTruth := truth.Device(spatialsim.Rover1, site.At(at))
				if !ok || !okTruth {
					t.Fatalf("no odometry at %v", at)
				}
				if at > 0 {
					delta := prevOdom.Pose.Between(odom.Pose)
					want := prevTrue.Between(state.Pose)
					cov := odom.Noise.Covariance(delta)
					errs := [3]float64{delta.X - want.X, delta.Z - want.Z, geom.AngleDiff(delta.Yaw, want.Yaw)}
					for axis, e := range errs {
						if v := cov[axis*4]; v > 0 {
							sum[axis] += e * e / v
							count[axis]++
						}
					}
				}
				prevOdom, prevTrue = odom, state.Pose
			}
		}
		for axis, name := range []string{"forward", "lateral", "yaw"} {
			mean := sum[axis] / float64(count[axis])
			t.Logf("step %.2f m, %s: normalised squared error %.2f over %d increments", stepM, name, mean, count[axis])
			if count[axis] < minIncrements || !(mean >= minNormalised && mean <= maxNormalised) {
				t.Errorf("step %.2f m, %s: normalised squared error %.2f over %d increments, want within [%g, %g]", stepM, name, mean, count[axis], float64(minNormalised), float64(maxNormalised))
			}
		}
	}
}
