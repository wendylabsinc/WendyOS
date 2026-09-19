package containerd

import (
	"context"
	"fmt"
	"os"

	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
)

const (
	ros2SystemSidecarName = "wendy-ros2-system-cli"
	// System CLI containers have no app anchor. A separate label keeps the app
	// reaper from removing them and preserves the restricted host inspector.
	labelKeyROS2SystemSidecar = "sh.wendy/ros2.system-cli"
)

// EnsureSystemROS2Sidecar provides the full CLI for the device's DDS graph when
// no ROS 2 app is running. Robots such as Unitree can expose DDS without a ros2
// executable on the host, so use a cached ROS image rather than host binaries.
func (c *Client) EnsureSystemROS2Sidecar(ctx context.Context) (services.ROS2Sidecar, error) {
	if err := os.MkdirAll(ROS2BagDir, ROS2BagDirMode); err != nil {
		return services.ROS2Sidecar{}, fmt.Errorf("creating ROS 2 bag directory: %w", err)
	}
	return c.ensureStandaloneROS2Sidecar(ctx, ros2SystemSidecarName, labelKeyROS2SystemSidecar, 0, ros2SystemSidecarSpec(ros2HostProfilePath))
}

func ros2SystemSidecarSpec(profilePath string) *localoci.Spec {
	spec := ros2HostSidecarSpec(profilePath)
	// Recordings use the same root-owned 0750 directory as app sidecars. Keep
	// the remaining filesystem read-only, private IPC and no capabilities.
	spec.Process.User = localoci.User{}
	spec.Mounts = append(spec.Mounts, localoci.Mount{
		Destination: ROS2BagDir, Type: "bind", Source: ROS2BagDir,
		Options: []string{"bind", "rw", "nosuid", "nodev", "noexec"},
	})
	return spec
}
