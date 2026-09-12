package data

import (
	"strings"
	"testing"
)

// A camera source no longer deploy-warns that its pre-roll starts at the
// trigger: camera pre-roll is honored. Audio and ROS 2 sources still do.
func TestBufferWarningExcludesCameraButNotROS2(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cameraOnly := []byte(`version: 1
name: camera-only
fleet: lab
sources:
  - camera: front
capture:
  buffer: 10s
  after_trigger: 20s
  triggers:
    - event: emergency_stop
upload:
  when: wifi
  destination: episodes
export:
  annotation: cvat
`)
	campaign, err := manager.DeployCampaign(cameraOnly)
	if err != nil {
		t.Fatal(err)
	}
	for _, warning := range campaign.Warnings {
		if strings.Contains(warning, "start at the trigger") {
			t.Fatalf("camera-only campaign still warns that its stream starts at the trigger: %q", warning)
		}
	}

	withROS2 := []byte(`version: 1
name: with-ros2
fleet: lab
sources:
  - camera: front
  - ros2: /lidar/points
capture:
  buffer: 10s
  after_trigger: 20s
  triggers:
    - event: emergency_stop
upload:
  when: wifi
  destination: episodes
export:
  annotation: cvat
`)
	campaign, err = manager.DeployCampaign(withROS2)
	if err != nil {
		t.Fatal(err)
	}
	var warned bool
	for _, warning := range campaign.Warnings {
		if strings.Contains(warning, "audio and ROS 2 streams start at the trigger") {
			warned = true
			if strings.Contains(warning, "camera streams start at the trigger") {
				t.Fatalf("warning wrongly claims camera starts at the trigger: %q", warning)
			}
		}
	}
	if !warned {
		t.Fatalf("a buffered campaign with a ROS 2 source did not warn about trigger-start streams: %v", campaign.Warnings)
	}
}

// bufferedCameraCampaign builds a deployable campaign whose camera source
// carries the given capture policy body (empty for none) and the given buffer.
func bufferedCameraCampaign(name, buffer, capture string) []byte {
	source := "  - camera: front\n"
	if capture != "" {
		source += "    capture: {" + capture + "}\n"
	}
	// capture.buffer is a required field, so "no pre-roll" is spelled 0s.
	if buffer == "" {
		buffer = "0s"
	}
	plan := "version: 1\nname: " + name + "\nfleet: lab\nsources:\n" + source +
		"capture:\n  buffer: " + buffer + "\n"
	return []byte(plan + `  after_trigger: 20s
  triggers:
    - event: emergency_stop
upload:
  when: wifi
  destination: episodes
export:
  annotation: cvat
`)
}

func hasPreRollParameterWarning(warnings []string) bool {
	for _, warning := range warnings {
		if strings.Contains(warning, "mutually exclusive in this release") {
			return true
		}
	}
	return false
}

// Pre-roll subscribes without asserting stream parameters, so a camera source
// that sets BOTH a buffer and an explicit max_resolution or rate does not get
// those parameters. The operator must learn that at deploy time, not by reading
// the manifest after the episode.
func TestDeployWarnsWhenPreRollMeetsExplicitCameraParameters(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deploy := func(t *testing.T, plan []byte) Campaign {
		t.Helper()
		campaign, err := manager.DeployCampaign(plan)
		if err != nil {
			t.Fatal(err)
		}
		return campaign
	}

	t.Run("buffer with max_resolution warns", func(t *testing.T) {
		campaign := deploy(t, bufferedCameraCampaign("res-conflict", "10s", "max_resolution: 1280x720"))
		if !hasPreRollParameterWarning(campaign.Warnings) {
			t.Fatalf("no mutual-exclusivity warning: %v", campaign.Warnings)
		}
		for _, warning := range campaign.Warnings {
			if !strings.Contains(warning, "mutually exclusive in this release") {
				continue
			}
			for _, want := range []string{"sources[0]", "camera:front", "max_resolution 1280x720", "producer's running parameters", "requested but not achieved"} {
				if !strings.Contains(warning, want) {
					t.Errorf("warning %q does not name %q", warning, want)
				}
			}
		}
	})

	t.Run("buffer with rate warns", func(t *testing.T) {
		campaign := deploy(t, bufferedCameraCampaign("rate-conflict", "10s", "rate: 5"))
		if !hasPreRollParameterWarning(campaign.Warnings) {
			t.Fatalf("no mutual-exclusivity warning for an explicit rate: %v", campaign.Warnings)
		}
	})

	t.Run("buffer alone does not warn", func(t *testing.T) {
		campaign := deploy(t, bufferedCameraCampaign("buffer-only", "10s", ""))
		if hasPreRollParameterWarning(campaign.Warnings) {
			t.Fatalf("buffer-only campaign warned about parameters it never set: %v", campaign.Warnings)
		}
	})

	t.Run("explicit parameters alone do not warn", func(t *testing.T) {
		campaign := deploy(t, bufferedCameraCampaign("params-only", "", "max_resolution: 1280x720, rate: 5"))
		if hasPreRollParameterWarning(campaign.Warnings) {
			t.Fatalf("unbuffered campaign warned though its explicit parameters are honored: %v", campaign.Warnings)
		}
	})

	// A snapshot-mode source is never armed, so its explicit parameters are still
	// honored through the normal capture join and must not be warned about.
	t.Run("snapshot mode with buffer does not warn", func(t *testing.T) {
		campaign := deploy(t, bufferedCameraCampaign("snapshot-buffered", "10s", "mode: snapshot, interval: 2s, max_resolution: 1280x720"))
		if hasPreRollParameterWarning(campaign.Warnings) {
			t.Fatalf("snapshot source warned though it is never armed: %v", campaign.Warnings)
		}
	})
}
