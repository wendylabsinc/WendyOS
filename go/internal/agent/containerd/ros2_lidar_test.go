package containerd

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
)

func validLidarExecOptions() services.ROS2ExecOptions {
	return services.ROS2ExecOptions{Lidar: &ros2inspection.LidarOptions{
		Topic: "/utlidar/cloud_deskewed", MessageType: "sensor_msgs/msg/PointCloud2", TargetFrame: "base_link",
		DurationSeconds: 5, Count: 1, MaxPoints: 10000, SamplePoints: 10,
		MinZ: -0.2, MaxZ: 1.2, MinRange: 0.1, MaxRange: 20,
	}}
}

func TestROS2LidarExecUsesTrustedCodeAndSeparateOptions(t *testing.T) {
	opts := validLidarExecOptions()
	args, err := ros2ExecArgs("humble", false, false, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 6 || args[0] != "/bin/sh" || args[1] != "-c" || args[4] != ros2inspection.LidarProbeScript {
		t.Fatal("LiDAR exec must pass the embedded probe as a single argument to the POSIX shell")
	}
	if !strings.HasSuffix(args[2], `exec python3 -u -c "$@"`) || !strings.Contains(args[2], ros2SetupScript("humble")) {
		t.Fatalf("unexpected probe shell script: %q", args[2])
	}
	for _, userValue := range []string{opts.Lidar.Topic, opts.Lidar.MessageType, opts.Lidar.TargetFrame} {
		if strings.Contains(args[2], userValue) {
			t.Fatalf("caller value %q was interpolated into shell source", userValue)
		}
	}
	var decoded ros2inspection.LidarOptions
	if err := json.Unmarshal([]byte(args[5]), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != *opts.Lidar {
		t.Fatalf("probe options changed: %+v", decoded)
	}

	// Exercise the actual shell expansion with a stand-in executable. Sourcing
	// ROS is the only omitted operation; the trusted code and JSON must arrive
	// byte-for-byte as arguments, without either being interpreted by the shell.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "python3"), []byte("#!/bin/sh\nprintf '%s\\0' \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prefix := ". " + ros2SetupScript("humble") + " >/dev/null 2>&1 && "
	args[2] = strings.TrimPrefix(args[2], prefix)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe shell: %v: %s", err, out)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	want := []string{"-u", "-c", ros2inspection.LidarProbeScript, args[5]}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("shell changed the Python probe or JSON options instead of forwarding argv")
	}
}

func TestROS2LidarExecRejectsUntrustedInputs(t *testing.T) {
	for _, mutate := range []func(*services.ROS2ExecOptions){
		func(o *services.ROS2ExecOptions) { o.Args = []string{"-c", "arbitrary_code()"} },
		func(o *services.ROS2ExecOptions) { o.Lidar.Topic = "/cloud;$(id)" },
		func(o *services.ROS2ExecOptions) { o.Lidar.MessageType = "__import__('os').system('id')" },
		func(o *services.ROS2ExecOptions) { o.Lidar.TargetFrame = "base_link;$(id)" },
		func(o *services.ROS2ExecOptions) { o.Lidar.DurationSeconds = -1 },
	} {
		opts := validLidarExecOptions()
		mutate(&opts)
		for _, host := range []bool{false, true} {
			if _, err := ros2ExecArgs("humble", false, host, opts); err == nil {
				t.Fatalf("accepted untrusted options on host=%t: %+v", host, opts)
			}
		}
	}
	if _, err := ros2ExecArgs("humble;id", false, false, validLidarExecOptions()); err == nil {
		t.Fatal("accepted shell metacharacters in the distro")
	}
}

func TestROS2LidarExecKeepsOverlayAndHostScope(t *testing.T) {
	opts := validLidarExecOptions()
	for _, host := range []bool{false, true} {
		args, err := ros2ExecArgs("humble", true, host, opts)
		if err != nil {
			t.Fatalf("host=%t: %v", host, err)
		}
		if strings.Contains(args[2], go2ROSSetup) == host {
			t.Fatalf("incorrect Go2 overlay for host=%t: %q", host, args[2])
		}
		if strings.Contains(args[2], ros2InspectorSetup) != host {
			t.Fatalf("incorrect native hardware overlay for host=%t: %q", host, args[2])
		}
	}
	for _, input := range [][]string{{"topic", "pub", "/cmd_vel"}, {"service", "call", "/move"}, {"run", "python3", "-c", "pass"}} {
		if _, err := ros2ExecArgs("humble", false, true, services.ROS2ExecOptions{Args: input}); err == nil {
			t.Fatalf("host CLI allowlist bypass: %v", input)
		}
	}
	input := []string{"topic", "list", "-t"}
	args, err := ros2ExecArgs("humble", false, true, services.ROS2ExecOptions{Args: input})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(args[4:], []string{"topic", "list", "-t", "--no-daemon", "--spin-time", "2"}) {
		t.Fatalf("host discovery safeguards changed: %v", args[4:])
	}
	args, err = ros2ExecArgs("humble", true, false, services.ROS2ExecOptions{Args: input})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(args[2], go2ROSSetup) || !strings.HasSuffix(args[2], `exec ros2 "$@"`) || !reflect.DeepEqual(args[4:], input) {
		t.Fatalf("ordinary app ROS 2 command changed: %v", args)
	}
}
