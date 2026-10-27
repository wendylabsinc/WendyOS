package simenv

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func sampleEnv() Env {
	return Env{
		SchemaVersion: SchemaVersion,
		Campaign:      "roundtrip",
		Revision:      "rev-7",
		Profile:       "go2",
		ControlRateHz: 50,
		Observations: []Observation{
			{
				Name:        "camera_front",
				Kind:        "image",
				Source:      "v4l2:/dev/video0",
				Dims:        []int{720, 1280, 3},
				Dtype:       "uint8",
				PixelFormat: "yuyv422",
				RateHz:      15,
			},
			{
				Name:   "ros2_imu_data",
				Kind:   "vector",
				Source: "ros2:cyclone:domain-0:/imu/data",
				Dims:   []int{10},
				Dtype:  "float32",
			},
		},
		Action: &Action{
			Stream:   "/cmd_vel",
			Channels: []string{"linear.x", "angular.z"},
			Low:      []float64{-0.8, -1},
			High:     []float64{0.8, 1},
			Unit:     "m/s",
		},
		Warnings: []string{"ros2 /joint_states (sensor_msgs/msg/JointState): joint count unknown without the robot; set dims manually"},
	}
}

func TestRenderEnvJSONRoundTrips(t *testing.T) {
	env := sampleEnv()
	out, err := RenderEnvJSON(env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got Env
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(env, got) {
		t.Fatalf("round-trip mismatch:\nwant %+v\ngot  %+v", env, got)
	}
}

func TestRenderEnvJSONShape(t *testing.T) {
	out, err := RenderEnvJSON(sampleEnv())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	s := string(out)
	for _, want := range []string{
		`"schema_version": 1`,
		`"profile": "go2"`,
		`"control_rate_hz": 50`,
		`"observations"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("env.json missing %q\n---\n%s", want, s)
		}
	}
	// Indented with two spaces, like the compatibility manifests.
	if !strings.Contains(s, "\n  \"campaign\"") {
		t.Fatalf("env.json is not two-space indented:\n%s", s)
	}
}

func TestRenderEnvJSONNilDimsOmitted(t *testing.T) {
	env := Env{
		SchemaVersion: SchemaVersion,
		Campaign:      "c",
		Profile:       ProfileCustom,
		Observations:  []Observation{{Name: "v", Kind: "vector", Source: "s"}},
	}
	out, err := RenderEnvJSON(env)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(string(out), `"dims"`) {
		t.Fatalf("nil dims should be omitted, got:\n%s", out)
	}
}
