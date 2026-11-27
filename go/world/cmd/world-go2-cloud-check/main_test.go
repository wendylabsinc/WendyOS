package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
)

func validRequest() request {
	return request{Schema: "wendy.world.go2-cloud-check.v1", Simulation: true,
		Observation: observationInput{Model: go2nav.CloudModelV1, SourceDigest: go2nav.CloudSourceDigest,
			Frame: "utlidar_lidar", PublisherGID: strings.Repeat("1", 48), SerializedSHA256: strings.Repeat("a", 64), CapturedUnixNS: 1000000000,
			Ranges: make([]*float64, 9000), Scan: scanInput{TiltedPose: &go2nav.TiltedPose{
				QuaternionWXYZ: [4]float64{1, 0, 0, 0}, OdomPositionXYZ: [3]float64{0, 0, .33}, MountTranslationXYZ: [3]float64{.28, 0, .08},
				MountQuaternionWXYZ: [4]float64{1, 0, 0, 0}, OdomCapturedUnixNS: 1000000000, BaseHeightMinM: .27, BaseHeightMaxM: .39},
				Sequence: 1, CapturedUnixNS: 1000000000, WorldEpoch: 1, Frame: "odom", Ranges: make([]*float64, 360), AngleMin: -3.141592653589793,
				AngleIncrement: 2 * 3.141592653589793 / 360, RangeMin: .1, RangeMax: 12, SensorMount: go2nav.Pose{X: .28}, MountKnown: true,
				PositionUncertaintyM: .07, YawUncertaintyRad: .02, UncertaintyKnown: true}},
		Bounds: go2nav.Bounds{Min: go2nav.Point{X: -2, Y: -2}, Max: go2nav.Point{X: 2, Y: 2}},
		Profile: go2nav.Profile{TiltedScan: &go2nav.TiltedScanProfile{Model: "solid-vertical-slab-v1", BaseHeightMinM: .15, BaseHeightMaxM: .6,
			ObstacleMinHeightM: .8, SlabMarginM: .03, BodyHalfHeightM: .4, MaxSupportRangeM: 4, RangeErrorM: .03, AttitudeErrorRad: .005,
			MaxTranslationMPerWallS: .5, MaxRotationRadPerWallS: 1, HeightCalibrationErrorM: .01}, MaxAbsRollRad: .08, MaxAbsPitchRad: .08,
			MotionEnvelope: go2nav.IsotropicGaitV1, FlatFloor: true, HalfLengthM: .425, HalfWidthM: .3, ClearanceM: .05,
			MaxPositionUncertaintyM: .25, MaxYawUncertaintyRad: .2}}
}

func TestMissingRaysRemainUnknown(t *testing.T) {
	raw, _ := json.Marshal(validRequest())
	var out bytes.Buffer
	if err := run(bytes.NewReader(raw), &out); err != nil {
		t.Fatal(err)
	}
	var r struct {
		OperationalAuthority bool                 `json:"operational_authority"`
		Evidence             go2nav.CloudEvidence `json:"evidence"`
	}
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.OperationalAuthority || len(r.Evidence.Rays) != 9000 || len(r.Evidence.Cells) == 0 {
		t.Fatal("bad diagnostic result")
	}
	for _, c := range r.Evidence.Cells {
		if c.State != "unknown" || c.WitnessRings != 0 {
			t.Fatal("missing rays created clearance")
		}
	}
}

func TestStrictRequestBoundary(t *testing.T) {
	raw, _ := json.Marshal(validRequest())
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"omitted_uncertainty", func(m map[string]any) {
			delete(m["observation"].(map[string]any)["scan"].(map[string]any), "PositionUncertaintyM")
		}},
		{"null_uncertainty", func(m map[string]any) {
			m["observation"].(map[string]any)["scan"].(map[string]any)["PositionUncertaintyM"] = nil
		}},
		{"short_quaternion", func(m map[string]any) {
			m["observation"].(map[string]any)["scan"].(map[string]any)["TiltedPose"].(map[string]any)["quaternion_wxyz"] = []any{1, 0, 0}
		}},
		{"null_cloud", func(m map[string]any) { m["observation"].(map[string]any)["ranges"] = nil }},
		{"short_cloud", func(m map[string]any) { m["observation"].(map[string]any)["ranges"] = []any{nil} }},
		{"wrong_stamp", func(m map[string]any) { m["observation"].(map[string]any)["captured_unix_ns"] = 2 }},
		{"unknown_field", func(m map[string]any) { m["hardware"] = true }},
		{"not_simulation", func(m map[string]any) { m["simulation"] = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			_ = json.Unmarshal(raw, &m)
			tc.mutate(m)
			b, _ := json.Marshal(m)
			var out bytes.Buffer
			if run(bytes.NewReader(b), &out) == nil || out.Len() != 0 {
				t.Fatal("invalid request produced result")
			}
		})
	}
	for _, b := range [][]byte{append(raw, []byte("{}")...), []byte(`{"schema":"a","schema":"b"}`), bytes.Repeat([]byte(" "), maxInput+1)} {
		var out bytes.Buffer
		if run(bytes.NewReader(b), &out) == nil || out.Len() != 0 {
			t.Fatal("malformed input accepted")
		}
	}
}

func TestContinuationCannotSubstitutePose(t *testing.T) {
	r := validRequest()
	r.Continuation = &continuationInput{Pose: go2nav.Pose{X: 1}, Prior: []go2nav.Command{}}
	b, _ := json.Marshal(r)
	var out bytes.Buffer
	if err := run(bytes.NewReader(b), &out); err == nil || out.Len() != 0 {
		t.Fatal(err)
	}
}
