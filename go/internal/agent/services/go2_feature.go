package services

// This is a capability contract, not a release-version guess. It means the
// agent has the managed VM kernel preparation and typed Unitree ROS inspector.
// Kernel/module availability is still checked when the runtime starts.
func appendGo2AgentFeature(features []string, hostOS, deviceType string) []string {
	if hostOS == "linux" && deviceType == "vm-arm64" {
		return append(features, "go2-virtual-robot", "g1-virtual-robot")
	}
	return features
}
