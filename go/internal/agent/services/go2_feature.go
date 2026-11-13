package services

// This is a capability contract, not a release-version guess. It means the
// agent supports the managed VM runtime lifecycle. Unitree/ROSMaster runtimes
// also use kernel preparation and ROS inspection; SO-101 has no DDS backend.
// Kernel/module availability is still checked by the runtimes that need it.
func appendGo2AgentFeature(features []string, hostOS, deviceType string) []string {
	if hostOS == "linux" && deviceType == "vm-arm64" {
		return append(features, "go2-virtual-robot", "g1-virtual-robot", "so101-virtual-robot", "rosmaster-r2-virtual-robot")
	}
	return features
}
