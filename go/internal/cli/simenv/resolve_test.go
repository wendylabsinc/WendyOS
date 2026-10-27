package simenv

import (
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestResolveProfiles(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
		want    string
		wantErr bool
	}{
		{name: "empty defaults to custom", profile: "", want: ProfileCustom},
		{name: "custom", profile: "custom", want: "custom"},
		{name: "known go2", profile: "go2", want: "go2"},
		{name: "known rosmaster", profile: "rosmaster-r2", want: "rosmaster-r2"},
		{name: "unknown", profile: "spot", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := Resolve(Input{Campaign: "c", Profile: tc.profile})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for profile %q", tc.profile)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if env.Profile != tc.want {
				t.Fatalf("profile = %q, want %q", env.Profile, tc.want)
			}
		})
	}
}

func TestResolveCamera(t *testing.T) {
	devices := []*agentpb.VideoDevice{
		{Id: 2, Name: "Logitech Brio", Path: "/dev/video2", PixelFormats: []string{"yuyv422", "mjpeg"}},
	}
	sources := []*agentpbv2.DataSource{
		{Id: "v4l2:/dev/video2", Kind: "camera", Healthy: true, Detail: "Logitech Brio"},
	}

	t.Run("resolution and pixel format", func(t *testing.T) {
		env, err := Resolve(Input{
			Campaign:      "cam",
			ControlRateHz: 20,
			Sources: []data.CampaignSource{{
				Camera:  "front",
				Capture: &data.SourceCapture{MaxResolution: "1280x720", Rate: 15},
			}},
			DataSources:  sources,
			VideoDevices: devices,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(env.Observations) != 1 {
			t.Fatalf("observations = %d, want 1", len(env.Observations))
		}
		obs := env.Observations[0]
		if obs.Kind != "image" {
			t.Fatalf("kind = %q, want image", obs.Kind)
		}
		if want := []int{720, 1280, 3}; !reflect.DeepEqual(obs.Dims, want) {
			t.Fatalf("dims = %v, want %v", obs.Dims, want)
		}
		if obs.Source != "v4l2:/dev/video2" {
			t.Fatalf("source = %q, want the resolved v4l2 id", obs.Source)
		}
		if obs.PixelFormat != "yuyv422" {
			t.Fatalf("pixel format = %q, want yuyv422", obs.PixelFormat)
		}
		if obs.RateHz != 15 {
			t.Fatalf("rate = %v, want 15 (capture rate)", obs.RateHz)
		}
		if len(env.Warnings) != 0 {
			t.Fatalf("warnings = %v, want none", env.Warnings)
		}
	})

	t.Run("missing max_resolution warns and leaves dims nil", func(t *testing.T) {
		env, err := Resolve(Input{
			Campaign:      "cam",
			ControlRateHz: 20,
			Sources:       []data.CampaignSource{{Camera: "front"}},
			DataSources:   sources,
			VideoDevices:  devices,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		obs := env.Observations[0]
		if obs.Dims != nil {
			t.Fatalf("dims = %v, want nil", obs.Dims)
		}
		if obs.RateHz != 20 {
			t.Fatalf("rate = %v, want 20 (training fallback)", obs.RateHz)
		}
		if !hasWarningContaining(env.Warnings, "no capture.max_resolution") {
			t.Fatalf("warnings = %v, want a max_resolution warning", env.Warnings)
		}
	})
}

func TestResolveROS2(t *testing.T) {
	sources := []*agentpbv2.DataSource{
		{Id: "ros2:cyclone:domain-0:/cmd_vel", Kind: "ros2", Healthy: true, Detail: "geometry_msgs/msg/Twist"},
		{Id: "ros2:cyclone:domain-0:/imu/data", Kind: "ros2", Healthy: true, Detail: "sensor_msgs/msg/Imu"},
		{Id: "ros2:cyclone:domain-0:/odom", Kind: "ros2", Healthy: true, Detail: "nav_msgs/msg/Odometry"},
		{Id: "ros2:cyclone:domain-0:/joint_states", Kind: "ros2", Healthy: true, Detail: "sensor_msgs/msg/JointState"},
	}

	for _, tc := range []struct {
		name     string
		selector string
		wantDims []int
		wantWarn string
	}{
		{name: "twist", selector: "/cmd_vel", wantDims: []int{6}},
		{name: "imu", selector: "/imu/data", wantDims: []int{10}},
		{name: "odometry", selector: "/odom", wantDims: []int{13}},
		{name: "jointstate variable length", selector: "/joint_states", wantDims: nil, wantWarn: "joint count unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, err := Resolve(Input{
				Campaign:    "ros",
				Sources:     []data.CampaignSource{{ROS2: tc.selector}},
				DataSources: sources,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(env.Observations) != 1 {
				t.Fatalf("observations = %d, want 1", len(env.Observations))
			}
			obs := env.Observations[0]
			if obs.Kind != "vector" {
				t.Fatalf("kind = %q, want vector", obs.Kind)
			}
			if !reflect.DeepEqual(obs.Dims, tc.wantDims) {
				t.Fatalf("dims = %v, want %v", obs.Dims, tc.wantDims)
			}
			if tc.wantWarn != "" && !hasWarningContaining(env.Warnings, tc.wantWarn) {
				t.Fatalf("warnings = %v, want one containing %q", env.Warnings, tc.wantWarn)
			}
			if tc.wantWarn == "" && len(env.Warnings) != 0 {
				t.Fatalf("warnings = %v, want none", env.Warnings)
			}
		})
	}

	t.Run("domain selector expands to topics", func(t *testing.T) {
		env, err := Resolve(Input{
			Campaign:    "ros",
			Sources:     []data.CampaignSource{{ROS2: "ros2:cyclone:domain-0"}},
			DataSources: sources,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(env.Observations) != 4 {
			t.Fatalf("observations = %d, want 4 (one per topic)", len(env.Observations))
		}
		// Sorted by identifier for deterministic output.
		if env.Observations[0].Source != "ros2:cyclone:domain-0:/cmd_vel" {
			t.Fatalf("first source = %q, want /cmd_vel sorted first", env.Observations[0].Source)
		}
	})

	t.Run("unmatched selector warns", func(t *testing.T) {
		env, err := Resolve(Input{
			Campaign:    "ros",
			Sources:     []data.CampaignSource{{ROS2: "/nonexistent"}},
			DataSources: sources,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(env.Observations) != 0 {
			t.Fatalf("observations = %d, want 0", len(env.Observations))
		}
		if !hasWarningContaining(env.Warnings, "no resolved source matches") {
			t.Fatalf("warnings = %v, want an unmatched warning", env.Warnings)
		}
	})
}

func TestResolveAction(t *testing.T) {
	action := &Action{
		Stream:   "/cmd_vel",
		Channels: []string{"linear.x", "linear.y", "angular.z"},
		Low:      []float64{-0.8, -0.5, -1.0},
		High:     []float64{0.8, 0.5, 1.0},
		Unit:     "m/s",
	}

	t.Run("action without echo", func(t *testing.T) {
		env, err := Resolve(Input{Campaign: "act", ActionSpec: action})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if env.Action == nil {
			t.Fatalf("action = nil, want the action spec")
		}
		if !reflect.DeepEqual(env.Action.Low, action.Low) {
			t.Fatalf("low = %v, want %v", env.Action.Low, action.Low)
		}
		if len(env.Observations) != 0 {
			t.Fatalf("observations = %d, want 0 when the action is not observed", len(env.Observations))
		}
	})

	t.Run("action echoed when stream appears as a source", func(t *testing.T) {
		env, err := Resolve(Input{
			Campaign:   "act",
			Sources:    []data.CampaignSource{{ROS2: "/cmd_vel"}},
			ActionSpec: action,
			DataSources: []*agentpbv2.DataSource{
				{Id: "ros2:cyclone:domain-0:/cmd_vel", Kind: "ros2", Healthy: true, Detail: "geometry_msgs/msg/Twist"},
			},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var echo *Observation
		for i := range env.Observations {
			if env.Observations[i].Source == "/cmd_vel" {
				echo = &env.Observations[i]
			}
		}
		if echo == nil {
			t.Fatalf("observations = %v, want an action echo for /cmd_vel", env.Observations)
		}
		if want := []int{3}; !reflect.DeepEqual(echo.Dims, want) {
			t.Fatalf("echo dims = %v, want %v", echo.Dims, want)
		}
	})
}

func TestResolveTelemetryAndAudioExcluded(t *testing.T) {
	env, err := Resolve(Input{
		Campaign: "mix",
		Sources: []data.CampaignSource{
			{Telemetry: true},
			{Audio: "mic0"},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(env.Observations) != 0 {
		t.Fatalf("observations = %d, want 0", len(env.Observations))
	}
	if !hasWarningContaining(env.Warnings, "telemetry excluded") {
		t.Fatalf("warnings = %v, want a telemetry exclusion", env.Warnings)
	}
	if !hasWarningContaining(env.Warnings, "audio mic0 excluded") {
		t.Fatalf("warnings = %v, want an audio exclusion", env.Warnings)
	}
}

func hasWarningContaining(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
