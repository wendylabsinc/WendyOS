package containerd

import (
	"context"
	"fmt"
	"strings"
)

// The sidecar joins the app's network namespace, so its discovery settings
// must also match the app. Forcing localhost here hides a host-configured
// app's physical sensors, even when DDS topic discovery finds their names.
// Read the anchor on every exec so existing sidecars need no restart or new
// labels to pick up this behavior. Standalone inspectors have no app anchor.
func (c *Client) ros2SidecarDiscoveryEnv(ctx context.Context, labels map[string]string, standalone bool) ([]string, error) {
	if standalone {
		return ros2ExecDiscoveryEnv(true), nil
	}
	anchorID := labels[labelKeyROS2AnchorID]
	if anchorID == "" {
		return nil, fmt.Errorf("ROS 2 sidecar is missing its anchor container")
	}
	anchor, err := c.client.LoadContainer(ctx, anchorID)
	if err != nil {
		return nil, fmt.Errorf("loading ROS 2 discovery anchor %q: %w", anchorID, err)
	}
	spec, err := anchor.Spec(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading ROS 2 discovery anchor %q: %w", anchorID, err)
	}
	if spec.Process == nil {
		return nil, fmt.Errorf("ROS 2 discovery anchor %q has no process specification", anchorID)
	}
	return ros2AppDiscoveryEnv(spec.Process.Env), nil
}

func ros2AppDiscoveryEnv(env []string) []string {
	var localhost, discoveryRange string
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		switch key {
		case "ROS_LOCALHOST_ONLY":
			localhost = value
		case "ROS_AUTOMATIC_DISCOVERY_RANGE":
			discoveryRange = value
		}
	}
	// Only an explicit subnet configuration broadens discovery. Missing or
	// inconsistent settings retain the existing app-local default. The empty
	// range supports older ROS configurations using only ROS_LOCALHOST_ONLY.
	host := localhost == "0" && (discoveryRange == "SUBNET" || discoveryRange == "")
	return ros2ExecDiscoveryEnv(host)
}

func ros2WithDiscoveryEnv(env, discovery []string) []string {
	out := make([]string, 0, len(env)+len(discovery))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if key != "ROS_LOCALHOST_ONLY" && key != "ROS_AUTOMATIC_DISCOVERY_RANGE" {
			out = append(out, entry)
		}
	}
	// Replace, rather than append duplicate keys: cached sidecar specs contain
	// ROS_LOCALHOST_ONLY=1, which some process launchers may otherwise retain.
	return append(out, discovery...)
}
