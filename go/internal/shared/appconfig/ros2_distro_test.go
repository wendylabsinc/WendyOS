package appconfig

import (
	"strings"
	"testing"
)

// validateROS2Config checked domainId, rmw and scope but never distro —
// even though distro is the field that gets interpolated into an image reference
// (docker.io/library/ros:<distro>) and into a shell command inside the sidecar
// (source /opt/ros/<distro>/setup.bash). A typo therefore passed `wendy run` and
// only surfaced much later as "invalid ROS 2 distro %q in container label" from
// `wendy device ros2 …`, or as a failed image pull.

func TestValidateROS2Config_AcceptsWellFormedDistros(t *testing.T) {
	for _, distro := range []string{"", "humble", "jazzy", "iron", "rolling", "foxy", "kilted", "ros2"} {
		cfg := &ROS2Config{Distro: distro}
		if err := validateROS2Config("frameworks.ros2", cfg, nil); err != nil {
			t.Errorf("distro %q rejected: %v", distro, err)
		}
	}
	// Case is normalised by ResolvedDistro, so mixed case is accepted here.
	if err := validateROS2Config("frameworks.ros2", &ROS2Config{Distro: "Humble"}, nil); err != nil {
		t.Errorf("mixed-case distro rejected: %v", err)
	}
}

func TestValidateROS2Config_RejectsMalformedDistros(t *testing.T) {
	bad := map[string]string{
		"humble; rm -rf /":   "shell metacharacters",
		"humble && curl x":   "command chaining",
		"../../etc":          "path traversal",
		"humble/extra":       "slash would break the image reference",
		"humble,domain_id=0": "a comma corrupts the annotation encoding",
		"1humble":            "must start with a letter",
		"humble ":            "trailing space",
		" humble":            "leading space",
		"hum_ble":            "underscore is not a ROS 2 distro character",
		"hum-ble":            "hyphen is not a ROS 2 distro character",
		"$(evil)":            "command substitution",
	}
	for distro, why := range bad {
		err := validateROS2Config("frameworks.ros2", &ROS2Config{Distro: distro}, nil)
		if err == nil {
			t.Errorf("distro %q accepted, want an error (%s)", distro, why)
			continue
		}
		if !strings.Contains(err.Error(), "frameworks.ros2.distro") {
			t.Errorf("distro %q error should name the field, got: %v", distro, err)
		}
	}
}

func TestValidateROS2Config_DistroErrorNamesTheService(t *testing.T) {
	// Multi-service apps must say *which* service is wrong.
	err := validateROS2Config(`services["talker"].frameworks.ros2`, &ROS2Config{Distro: "no spaces here"}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), `services["talker"]`) {
		t.Errorf("error should name the service, got: %v", err)
	}
}

func TestIsKnownROS2Distro(t *testing.T) {
	for _, known := range []string{"humble", "jazzy", "Humble", "ROLLING"} {
		if !IsKnownROS2Distro(known) {
			t.Errorf("IsKnownROS2Distro(%q) = false, want true", known)
		}
	}
	for _, unknown := range []string{"", "nonesuch", "humble2"} {
		if IsKnownROS2Distro(unknown) {
			t.Errorf("IsKnownROS2Distro(%q) = true, want false", unknown)
		}
	}
}

// A well-formed but unrecognized distro must still validate: pinning the list
// would block a new ROS 2 release until Wendy shipped support for it.
func TestValidateROS2Config_UnknownButWellFormedDistroIsAllowed(t *testing.T) {
	if IsKnownROS2Distro("mystery") {
		t.Skip("test distro is unexpectedly in the known set")
	}
	if err := validateROS2Config("frameworks.ros2", &ROS2Config{Distro: "mystery"}, nil); err != nil {
		t.Errorf("well-formed unknown distro rejected: %v", err)
	}
}
