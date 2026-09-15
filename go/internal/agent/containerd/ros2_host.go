package containerd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
)

const (
	ros2HostSidecarName = "wendy-ros2-host-inspector"
	// A separate label keeps host inspection out of app discovery, default
	// Exec selection, anchor verification, and app-sidecar reconciliation.
	labelKeyROS2HostSidecar = "sh.wendy/ros2.host-inspector"
	ros2HostProfilePath     = "/var/wendy/ros2-host-inspection/fastdds.xml"
	ros2HostProfileMount    = "/etc/wendy-fastdds.xml"
	ros2HostImage           = "docker.io/library/ros:humble-ros-base"
)

// Explicit UDP transports avoid FastRTPS assuming it can share sample buffers
// with host processes. The inspector keeps its own IPC namespace and /dev/shm.
const ros2HostFastDDSProfile = `<profiles xmlns="http://www.eprosima.com/XMLSchemas/fastRTPS_Profiles">
<transport_descriptors><transport_descriptor><transport_id>wendy_udp</transport_id><type>UDPv4</type></transport_descriptor></transport_descriptors>
<participant profile_name="wendy_host_inspection" is_default_profile="true"><rtps><userTransports><transport_id>wendy_udp</transport_id></userTransports><useBuiltinTransports>false</useBuiltinTransports></rtps></participant>
</profiles>`

// EnsureHostROS2Sidecar provisions one fixed, unprivileged inspector independently
// of running app containers. Only an explicit host-scoped inspection calls it.
// The image is cached after first use; no package installation or app deployment
// is performed. A domain is selected per Exec, never by changing an app config.
func (c *Client) EnsureHostROS2Sidecar(ctx context.Context, opts ros2inspection.HostOptions) (services.ROS2Sidecar, error) {
	if err := opts.Validate(); err != nil {
		return services.ROS2Sidecar{}, err
	}
	return c.ensureStandaloneROS2Sidecar(ctx, ros2HostSidecarName, labelKeyROS2HostSidecar, opts.DomainID, ros2HostSidecarSpec(ros2HostProfilePath))
}

// ensureStandaloneROS2Sidecar shares image preparation and lifecycle management
// between the restricted host inspector and the full system CLI. Their distinct
// labels keep command permissions and app-sidecar reconciliation separate.
func (c *Client) ensureStandaloneROS2Sidecar(ctx context.Context, name, label string, domain int, spec *localoci.Spec) (services.ROS2Sidecar, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx = c.withNamespace(ctx)
	sidecar := services.ROS2Sidecar{Name: name, Distro: ros2inspection.HostDistro, RMW: ros2inspection.FastRTPSRMW, DomainID: domain}
	if existing, err := c.client.LoadContainer(ctx, name); err == nil {
		labels, lerr := existing.Labels(ctx)
		if lerr != nil {
			return services.ROS2Sidecar{}, fmt.Errorf("reading host inspector labels: %w", lerr)
		}
		if labels[label] != ros2inspection.HostDistro {
			return services.ROS2Sidecar{}, fmt.Errorf("container name %q is already in use by a different workload", name)
		}
		if task, terr := existing.Task(ctx, nil); terr == nil {
			if st, serr := task.Status(ctx); serr == nil && st.Status == containerd.Running {
				return sidecar, nil
			}
		}
		if c.sidecarHasActiveExecsLocked(name) {
			return services.ROS2Sidecar{}, fmt.Errorf("host inspector is restarting with an inspection in flight; retry")
		}
		if err := c.deleteROS2Sidecar(ctx, existing); err != nil {
			return services.ROS2Sidecar{}, err
		}
	} else if !errdefs.IsNotFound(err) {
		return services.ROS2Sidecar{}, fmt.Errorf("loading host inspector: %w", err)
	}

	image, err := c.client.GetImage(ctx, ros2HostImage)
	if err != nil {
		image, err = c.client.Pull(ctx, ros2HostImage, containerd.WithPullUnpack)
		if err != nil {
			return services.ROS2Sidecar{}, fmt.Errorf("preparing standalone ROS 2 inspector image %q (first use requires downloading the image): %w", ros2HostImage, err)
		}
	}
	unpacked, err := image.IsUnpacked(ctx, "")
	if err != nil {
		return services.ROS2Sidecar{}, fmt.Errorf("checking host inspector image: %w", err)
	}
	if !unpacked {
		if err := c.UnpackImage(ctx, image, nil); err != nil {
			return services.ROS2Sidecar{}, fmt.Errorf("unpacking host inspector image: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(ros2HostProfilePath), 0o750); err != nil {
		return services.ROS2Sidecar{}, err
	}
	// The file contains only a fixed transport configuration, no sensor data.
	if err := os.WriteFile(ros2HostProfilePath, []byte(ros2HostFastDDSProfile), 0o444); err != nil {
		return services.ROS2Sidecar{}, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return services.ROS2Sidecar{}, err
	}
	ctr, err := c.client.NewContainer(ctx, name,
		containerd.WithImage(image), containerd.WithNewSnapshot(name, image),
		containerd.WithContainerLabels(map[string]string{label: ros2inspection.HostDistro, labelKeyROS2RMW: ros2inspection.FastRTPSRMW}),
		containerd.WithNewSpec(oci.WithSpecFromBytes(specJSON)),
	)
	if err != nil {
		return services.ROS2Sidecar{}, fmt.Errorf("creating host ROS 2 inspector: %w", err)
	}
	task, err := ctr.NewTask(ctx, cio.NullIO)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = ctr.Delete(cleanupCtx, containerd.WithSnapshotCleanup)
		return services.ROS2Sidecar{}, fmt.Errorf("creating host inspector task: %w", err)
	}
	if err := task.Start(ctx); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = task.Delete(cleanupCtx, containerd.WithProcessKill)
		_ = ctr.Delete(cleanupCtx, containerd.WithSnapshotCleanup)
		return services.ROS2Sidecar{}, fmt.Errorf("starting host inspector: %w", err)
	}
	return sidecar, nil
}

func ros2HostSidecarSpec(profilePath string) *localoci.Spec {
	spec := localoci.DefaultSpec("rootfs", []string{"sleep", "infinity"})
	spec.Process.User = localoci.User{UID: 65534, GID: 65534}
	spec.Process.Capabilities = &localoci.LinuxCapabilities{}
	spec.Root.Readonly = true
	var namespaces []localoci.LinuxNamespace
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type != "network" {
			namespaces = append(namespaces, ns)
		}
	}
	spec.Linux.Namespaces = namespaces
	spec.Process.Env = append(spec.Process.Env,
		"HOME=/tmp", "ROS_LOG_DIR=/tmp/ros-logs", "RMW_IMPLEMENTATION="+ros2inspection.FastRTPSRMW,
		"ROS_LOCALHOST_ONLY=0", "ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET",
		"FASTRTPS_DEFAULT_PROFILES_FILE="+ros2HostProfileMount,
		"FASTDDS_DEFAULT_PROFILES_FILE="+ros2HostProfileMount,
	)
	spec.Mounts = append(spec.Mounts,
		localoci.Mount{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777", "size=64m"}},
		localoci.Mount{Destination: ros2HostProfileMount, Type: "bind", Source: profilePath, Options: []string{"bind", "ro", "nosuid", "nodev", "noexec"}},
	)
	return spec
}

// Defense in depth: even a future caller of ExecROS2 cannot use the standalone
// host inspector to publish, call services/actions, or run arbitrary ros2 verbs.
func hostROS2InspectionArgs(args []string) ([]string, error) {
	if len(args) < 2 || args[0] != "topic" {
		return nil, fmt.Errorf("host inspector permits only topic list, info, echo and hz")
	}
	valid := false
	switch args[1] {
	case "list":
		valid = len(args) == 2 || (len(args) == 3 && args[2] == "-t")
	case "info":
		valid = (len(args) == 3 && strings.HasPrefix(args[2], "/")) || (len(args) == 4 && args[2] == "-v" && strings.HasPrefix(args[3], "/"))
	case "echo", "hz":
		valid = len(args) == 3 && strings.HasPrefix(args[2], "/")
	}
	if !valid {
		return nil, fmt.Errorf("unsupported command for read-only host ROS 2 inspection")
	}
	out := append([]string(nil), args...)
	if args[1] == "list" || args[1] == "info" {
		// Humble NodeStrategy accepts these flags. Direct discovery avoids a
		// persistent host DDS daemon or cached results from an earlier request.
		out = append(out, "--no-daemon", "--spin-time", "2")
	}
	return out, nil
}

func ros2ExecDiscoveryEnv(host bool) []string {
	if host {
		return []string{"ROS_LOCALHOST_ONLY=0", "ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET"}
	}
	return []string{"ROS_LOCALHOST_ONLY=1", "ROS_AUTOMATIC_DISCOVERY_RANGE=LOCALHOST"}
}
