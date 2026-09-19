package ros2inspection

import (
	"os/exec"
	"testing"
)

// The probe runs in the ROS image, but its binary decoder and geometry tests
// need only the Python standard library. Include them in ordinary Go test runs
// on development/CI hosts that have Python; ROS integration is opt-in.
func TestLidarProbe(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable; run lidar_probe_test.py in the ROS image")
	}
	cmd := exec.Command(python, "-B", "-m", "unittest", "discover", "-s", ".", "-p", "lidar_probe_test.py", "-v")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("LiDAR probe tests: %v\n%s", err, output)
	}
}
