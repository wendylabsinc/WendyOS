package containerd

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

func TestGo2ROS2AnchorRequiresRunningMatchingProfile(t *testing.T) {
	base := services.ROS2Target{AppID: go2RuntimeAppID, Distro: "humble", DomainID: 0,
		RMW: "rmw_cyclonedds_cpp", Running: true}
	ordinary := base
	ordinary.AppID = "example.navigation"
	if !preferROS2Anchor(&base, &ordinary) || preferROS2Anchor(&ordinary, &base) {
		t.Fatal("typed runtime must win independently of container listing order")
	}
	for name, change := range map[string]func(*services.ROS2Target){
		"stopped": func(v *services.ROS2Target) { v.Running = false },
		"domain":  func(v *services.ROS2Target) { v.DomainID = 42 },
		"distro":  func(v *services.ROS2Target) { v.Distro = "jazzy" },
		"rmw":     func(v *services.ROS2Target) { v.RMW = "rmw_fastrtps_cpp" },
		"app":     func(v *services.ROS2Target) { v.AppID = ordinary.AppID },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			change(&candidate)
			if isGo2ROS2Target(&candidate) || preferROS2Anchor(&candidate, &ordinary) {
				t.Fatal("unrelated/stopped target selected as managed Go2")
			}
		})
	}
}

func TestG1ROS2AnchorAndOverlayCannotUseGo2Identity(t *testing.T) {
	g1 := services.ROS2Target{AppID: g1RuntimeAppID, Distro: "humble", DomainID: 0,
		RMW: "rmw_cyclonedds_cpp", Running: true}
	ordinary := g1
	ordinary.AppID = "example.navigation"
	if virtualRobotROS2Kind(&g1) != "g1" || isGo2ROS2Target(&g1) ||
		!preferROS2Anchor(&g1, &ordinary) || preferROS2Anchor(&ordinary, &g1) {
		t.Fatal("G1 anchor selection used the wrong robot identity")
	}
	for _, mutate := range []func(*services.ROS2Target){
		func(v *services.ROS2Target) { v.Running = false },
		func(v *services.ROS2Target) { v.DomainID = 42 },
		func(v *services.ROS2Target) { v.Distro = "jazzy" },
		func(v *services.ROS2Target) { v.RMW = "rmw_fastrtps_cpp" },
		func(v *services.ROS2Target) { v.AppID = "sh.wendy.simulator.g1.untrusted" },
	} {
		candidate := g1
		mutate(&candidate)
		if virtualRobotROS2Kind(&candidate) != "" {
			t.Fatal("unrelated target selected as G1")
		}
	}
	for _, labels := range []map[string]string{
		nil, {labelKeyGo2Overlay: go2OverlayVersion}, {labelKeyG1Overlay: "unknown"},
		{labelKeyGo2Overlay: go2OverlayVersion, labelKeyG1Overlay: g1OverlayVersion},
	} {
		if virtualRobotOverlayLabelsMatch(labels, &g1) {
			t.Fatalf("G1 reused an absent, different or mixed overlay: %v", labels)
		}
	}
	if !virtualRobotOverlayLabelsMatch(map[string]string{labelKeyG1Overlay: g1OverlayVersion}, &g1) {
		t.Fatal("G1 did not reuse its exact overlay")
	}
}

func TestG1ROS2ExecUsesFixedOverlayAndPreservesHostScope(t *testing.T) {
	input := []string{"topic", "echo", "/topic; printf INJECTED", "--once"}
	args, err := ros2ExecArgsForRobot("humble", "g1", false, services.ROS2ExecOptions{Args: input})
	if err != nil || !strings.Contains(args[2], g1ROSSetup) || strings.Contains(args[2], go2ROSSetup) ||
		!reflect.DeepEqual(args[4:], input) {
		t.Fatalf("G1 arguments or overlay changed: %v, %v", args, err)
	}
	script := strings.ReplaceAll(args[2], ". "+ros2SetupScript("humble")+" >/dev/null 2>&1", "true")
	script = strings.ReplaceAll(script, ". "+g1ROSSetup+" >/dev/null 2>&1", "true")
	script = strings.Replace(script, "exec ros2", "printf '%s\\n'", 1)
	got, err := exec.Command("sh", append([]string{"-c", script, "ros2"}, input...)...).CombinedOutput()
	if err != nil || string(got) != strings.Join(input, "\n")+"\n" {
		t.Fatalf("G1 shell reinterpreted argv: %q, %v", got, err)
	}
	for _, host := range []bool{false, true} {
		args, err := ros2ExecArgsForRobot("humble", "g1", host, validLidarExecOptions())
		if err != nil || strings.Contains(args[2], g1ROSSetup) == host || strings.Contains(args[2], go2ROSSetup) {
			t.Fatalf("incorrect G1 lidar/host overlay: %v, %v", args, err)
		}
	}
	if _, err := ros2ExecArgsForRobot("humble", "g1", true,
		services.ROS2ExecOptions{Args: []string{"topic", "pub", "/cmd_vel"}}); err == nil {
		t.Fatal("G1 overlay bypassed the host inspection allowlist")
	}
	if _, err := ros2ExecArgsForRobot("humble", "g1;id", false, services.ROS2ExecOptions{Args: input}); err == nil {
		t.Fatal("untrusted overlay identity was accepted")
	}
}

func TestGo2ROS2OverlayShellPreservesArgumentBoundary(t *testing.T) {
	if got := ros2SourceAndExecForOverlay("humble", false); got != ros2SourceAndExec("humble") {
		t.Fatal("ordinary and host inspection shell changed")
	}
	script := ros2SourceAndExecForOverlay("humble", true)
	if !strings.Contains(script, ". "+go2ROSSetup+" >/dev/null 2>&1 && exec ros2 \"$@\"") {
		t.Fatalf("missing fixed overlay followed by quoted argv: %s", script)
	}
	// Exercise the actual shell suffix with harmless stand-ins for the two
	// fixed setup scripts. Argument content must never be reinterpreted.
	script = strings.ReplaceAll(script, ". "+ros2SetupScript("humble")+" >/dev/null 2>&1", "true")
	script = strings.ReplaceAll(script, ". "+go2ROSSetup+" >/dev/null 2>&1", "true")
	script = strings.Replace(script, "exec ros2", "printf '%s\\n'", 1)
	value := "/topic; printf INJECTED"
	cmd := exec.Command("sh", "-c", script, "ros2", "topic", "echo", value)
	got, err := cmd.CombinedOutput()
	if err != nil || string(got) != "topic\necho\n"+value+"\n" {
		t.Fatalf("argv boundary changed: %q, %v", got, err)
	}
}
