package containerd

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

const (
	go2RuntimeAppID    = "sh.wendy.simulator.go2"
	labelKeyGo2Overlay = "sh.wendy/ros2.go2.overlay"
	go2OverlayVersion  = "1"
	go2ROSSetup        = "/opt/wendy-go2/ros_ws/install/setup.sh"
	g1RuntimeAppID     = "sh.wendy.simulator.g1"
	labelKeyG1Overlay  = "sh.wendy/ros2.g1.overlay"
	g1OverlayVersion   = "1"
	g1ROSSetup         = "/opt/wendy-g1/ros_ws/install/setup.sh"
)

// The managed runtime carries the Unitree types. A random user app may carry
// only standard ROS messages, so prefer this anchor whenever it is running.
// Namespace ownership and task lifetime are still verified by the normal path.
func isGo2ROS2Target(target *services.ROS2Target) bool {
	return virtualRobotROS2Kind(target) == "go2"
}

func virtualRobotROS2Kind(target *services.ROS2Target) string {
	if target == nil || !target.Running || target.Distro != "humble" || target.DomainID != 0 ||
		(target.RMW != "rmw_cyclonedds_cpp" && target.RMW != "") {
		return ""
	}
	switch target.AppID {
	case go2RuntimeAppID:
		return "go2"
	case g1RuntimeAppID:
		return "g1"
	default:
		return ""
	}
}

func preferROS2Anchor(candidate, current *services.ROS2Target) bool {
	return current == nil || (virtualRobotROS2Kind(candidate) != "" && virtualRobotROS2Kind(current) == "")
}

func virtualRobotOverlayLabelsMatch(labels map[string]string, target *services.ROS2Target) bool {
	kind, err := virtualRobotOverlayKind(labels)
	return err == nil && kind == virtualRobotROS2Kind(target)
}

func virtualRobotOverlayKind(labels map[string]string) (string, error) {
	go2, g1 := labels[labelKeyGo2Overlay], labels[labelKeyG1Overlay]
	if (go2 != "" && go2 != go2OverlayVersion) || (g1 != "" && g1 != g1OverlayVersion) || (go2 != "" && g1 != "") {
		return "", fmt.Errorf("invalid or mixed virtual robot ROS overlay identity")
	}
	if go2 != "" {
		return "go2", nil
	}
	if g1 != "" {
		return "g1", nil
	}
	return "", nil
}

// Overlay paths are fixed build-time values. No app-supplied path or shell text
// is sourced. The standalone inspector uses its own native hardware overlay.
func ros2SourceAndExecForOverlay(distro string, go2 bool) string {
	kind := ""
	if go2 {
		kind = "go2"
	}
	return ros2SourceAndExecForRobot(distro, kind)
}

func ros2SourceAndExecForRobot(distro, kind string) string {
	var setup string
	switch kind {
	case "go2":
		setup = go2ROSSetup
	case "g1":
		setup = g1ROSSetup
	case "unitree":
		setup = ros2InspectorSetup
	default:
		return ros2SourceAndExec(distro)
	}
	return ". " + ros2SetupScript(distro) + " >/dev/null 2>&1 && . " +
		setup + " >/dev/null 2>&1 && exec ros2 \"$@\""
}
