package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/logfields"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
)

const (
	// ros2SidecarPrefix is the containerd ID prefix of the ROS 2 CLI sidecars.
	// A device runs one sidecar per distinct RMW (WDY-1594), each named
	// "<prefix>-<rmw-suffix>" by ros2SidecarName, so commands can reach the
	// right DDS graph. Listing by this prefix enumerates them all.
	ros2SidecarPrefix = "wendy-ros2-cli-sidecar"

	// ROS2BagDir is the host directory where rosbag2 recordings are stored.
	// It is bind-mounted into the sidecar at the same path so the agent can
	// list and stream bags without exec round-trips.
	ROS2BagDir = "/var/wendy/ros2-bags"

	// ROS2BagDirMode keeps recordings off world-readable. Bags carry raw sensor
	// data — camera frames, lidar, audio, positions — so 0o755 was more open than
	// it needed to be.
	ROS2BagDirMode = 0o750

	// labelKeyROS2Sidecar marks the sidecar container and stores its distro.
	// The sidecar intentionally has no sh.wendy/app.version label so it never
	// appears in ListContainers output.
	labelKeyROS2Sidecar = "sh.wendy/ros2.sidecar"

	// labelKeyROS2AnchorID and labelKeyROS2AnchorPID record which app
	// container's namespaces the sidecar joined. When the anchor changes
	// (app restarted), the sidecar is stale and must be recreated.
	labelKeyROS2AnchorID  = "sh.wendy/ros2.anchor.id"
	labelKeyROS2AnchorPID = "sh.wendy/ros2.anchor.pid"

	// labelKeyROS2RMW records the anchor app's RMW implementation so each
	// ExecROS2 can set RMW_IMPLEMENTATION to match — without it the sidecar's
	// ros2 CLI falls to the image default and can't see apps on another RMW
	// (WDY-1593). Empty means the app's RMW_IMPLEMENTATION was not set in its
	// OCI spec env; Wendy's config layer defaults that to CycloneDDS
	// (appconfig.ROS2DefaultRMW), so the sidecar layer treats empty as
	// CycloneDDS too (WDY-1703).
	labelKeyROS2RMW = "sh.wendy/ros2.rmw"

	// rosImageDefaultRMW is the only RMW the stock docker.io/library/ros:<distro>
	// image ships (verified: it contains librmw_fastrtps_cpp and the
	// rmw-fastrtps-cpp packages, no CycloneDDS). An anchor app on any other RMW
	// therefore needs a sidecar built from the app's own image (WDY-1593).
	rosImageDefaultRMW = "rmw_fastrtps_cpp"

	// ros2ExecStopGrace is how long ExecROS2 waits after SIGINT before
	// escalating to SIGKILL. `ros2 bag record` needs the grace period to
	// finalize the bag on disk.
	ros2ExecStopGrace = 10 * time.Second

	// ros2ExecKillWait bounds the wait for the exit status after SIGKILL. The
	// previous bare channel receive could hold the RPC — and one of the sidecar's
	// ros2MaxConcurrentExecs slots — forever if the shim never reported.
	ros2ExecKillWait = 15 * time.Second

	// ros2MaxConcurrentExecs is the maximum number of simultaneous ExecROS2
	// calls allowed against a single sidecar. Beyond this the exec is rejected
	// with a clear error so a slow or hung command stream cannot exhaust the
	// containerd exec table (L6, WDY-1706).
	ros2MaxConcurrentExecs = 16
)

// ros2DistroPattern validates ROS 2 distro names read from container labels
// before they are interpolated into an image reference and a shell command
// inside the sidecar (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
var ros2DistroPattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// ros2ExecCounter disambiguates concurrent exec IDs within the agent process.
var ros2ExecCounter atomic.Uint64

// FindROS2Containers returns all Wendy-managed containers carrying the
// sh.wendy/entitlement.ros2 label, with their parsed distro and DDS domain.
func (c *Client) FindROS2Containers(ctx context.Context) ([]services.ROS2Target, error) {
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", appconfig.ROS2AnnotationKey))
	if err != nil {
		return nil, fmt.Errorf("listing ROS2 containers: %w", err)
	}
	var targets []services.ROS2Target
	for _, ctr := range ctrs {
		labels, err := ctr.Labels(ctx)
		if err != nil {
			continue
		}
		distro, domainID, ok := appconfig.ParseROS2Annotation(labels[appconfig.ROS2AnnotationKey])
		if !ok {
			c.logger.Warn("Skipping container with malformed ros2 label",
				zap.String(logfields.ContainerID, ctr.ID()),
				zap.String("value", labels[appconfig.ROS2AnnotationKey]))
			continue
		}
		target := services.ROS2Target{
			ContainerID: ctr.ID(),
			AppID:       labels[labelKeyAppID],
			Distro:      distro,
			DomainID:    domainID,
		}
		// Resolve the RMW the app actually runs (Wendy injects RMW_IMPLEMENTATION
		// via buildROS2Env) so callers can group apps by RMW — a device runs one
		// sidecar per RMW (WDY-1593, WDY-1594). Drop an unrecognized value to ""
		// (the image default) so it can never reach a sidecar environment and so
		// naming/grouping stays consistent.
		if spec, serr := ctr.Spec(ctx); serr == nil {
			target.HostNetwork = ros2UsesHostNetwork(spec)
			if spec != nil && spec.Process != nil {
				if rmw := rmwFromEnv(spec.Process.Env); rmw == "" || appconfig.IsValidRMWImplementation(rmw) {
					target.RMW = rmw
				}
			}
		}
		if task, terr := ctr.Task(ctx, nil); terr == nil {
			if st, serr := task.Status(ctx); serr == nil && st.Status == containerd.Running {
				target.Running = true
				target.TaskPID = task.Pid()
			}
		}
		targets = append(targets, target)
	}
	return targets, nil
}

// ros2UsesHostNetwork recognizes the OCI representation of host networking.
// Missing specs and explicit namespace paths remain app-scoped: neither proves
// that discovery can safely reuse the host's camera participants.
func ros2UsesHostNetwork(spec *runtimespec.Spec) bool {
	if spec == nil || spec.Linux == nil {
		return false
	}
	for _, ns := range spec.Linux.Namespaces {
		if ns.Type == runtimespec.NetworkNamespace {
			return false
		}
	}
	return true
}

// rmwFromEnv returns the value of RMW_IMPLEMENTATION in a container's OCI spec
// env, or "" when absent. Wendy injects it into ROS 2 app containers
// (buildROS2Env); the sidecar reads it back to match the app's DDS impl.
func rmwFromEnv(env []string) string {
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "RMW_IMPLEMENTATION="); ok {
			return v
		}
	}
	return ""
}

// ros2SidecarSuffix maps an RMW to a short, containerd-ID-safe sidecar name
// suffix (e.g. "rmw_cyclonedds_cpp" -> "cyclonedds"). Empty RMW is treated as
// CycloneDDS — the Wendy config-layer default (appconfig.ROS2DefaultRMW) — so
// both layers agree on which sidecar serves an unset-RMW app (WDY-1703). One
// sidecar per suffix means one per distinct RMW.
func ros2SidecarSuffix(rmw string) string {
	switch rmw {
	case "rmw_cyclonedds_cpp", "":
		return "cyclonedds"
	case "rmw_fastrtps_cpp":
		return "fastrtps"
	case "rmw_connextdds":
		return "connext"
	case "rmw_gurumdds_cpp":
		return "gurum"
	default:
		return "default"
	}
}

// ros2SidecarName is the containerd ID of the sidecar for a given RMW.
func ros2SidecarName(rmw string) string {
	return ros2SidecarPrefix + "-" + ros2SidecarSuffix(rmw)
}

// listROS2Sidecars returns all sidecar containers (those carrying the sidecar
// label), across every RMW. ctx must already be namespaced.
func (c *Client) listROS2Sidecars(ctx context.Context) ([]containerd.Container, error) {
	return c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyROS2Sidecar))
}

// EnsureROS2Sidecars starts or reuses one CLI sidecar per distinct RMW used by
// the running ROS 2 apps, tears down sidecars whose RMW is gone, and returns one
// entry per live RMW graph (WDY-1594). Each sidecar runs an RMW-matched image
// and joins its anchor app's namespace topology (network always; for shared-ipc
// groups also the IPC namespace + shared /dev/shm — WDY-1555/1593), idling on
// `sleep infinity` while commands are exec'd into it.
func (c *Client) EnsureROS2Sidecars(ctx context.Context) ([]services.ROS2Sidecar, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx = c.withNamespace(ctx)

	targets, err := c.FindROS2Containers(ctx)
	if err != nil {
		return nil, err
	}
	// One anchor per distinct RMW. Prefer the managed virtual robot image for its typed
	// Unitree overlay; otherwise retain the first running target.
	var order []string // sidecar names, deduped, first-seen order
	anchorByName := map[string]*services.ROS2Target{}
	for i := range targets {
		t := &targets[i]
		if !t.Running {
			continue
		}
		name := ros2SidecarName(t.RMW)
		if current, dup := anchorByName[name]; dup {
			if a, b := virtualRobotROS2Kind(t), virtualRobotROS2Kind(current); a != "" && b != "" && a != b {
				return nil, fmt.Errorf("multiple virtual robot kinds share a ROS 2 graph; refusing to select a mismatched inspector")
			}
			if preferROS2Anchor(t, current) {
				anchorByName[name] = t
			}
			continue
		}
		anchorByName[name] = t
		order = append(order, name)
	}
	if len(order) == 0 {
		// No running ROS 2 apps: tear down every leftover sidecar and fail.
		c.teardownAllROS2SidecarsLocked(ctx)
		return nil, fmt.Errorf("%w; use explicit host inspection with a domain ID for robot/host sensors, or deploy an app with a frameworks.ros2 config for app-scoped inspection", services.ErrNoRunningROS2Containers)
	}

	// Tear down sidecars whose RMW is no longer running.
	if existing, lerr := c.listROS2Sidecars(ctx); lerr == nil {
		for _, ctr := range existing {
			if _, want := anchorByName[ctr.ID()]; !want {
				if c.sidecarHasActiveExecsLocked(ctr.ID()) {
					c.logger.Info("Deferring stale ROS 2 sidecar teardown: exec in flight",
						zap.String("sidecar", ctr.ID()))
					continue
				}
				_ = c.deleteROS2Sidecar(ctx, ctr)
			}
		}
	}

	// Build sidecars for every RMW. On a per-RMW failure we log and continue so
	// that a broken image for one RMW does not prevent commands against the
	// remaining RMWs (L3, WDY-1706 partial-success). Only fail the whole call
	// when every sidecar failed to ensure.
	sidecars := make([]services.ROS2Sidecar, 0, len(order))
	var errs []error
	for _, name := range order {
		sc, eerr := c.ensureOneROS2Sidecar(ctx, anchorByName[name], name)
		if eerr != nil {
			c.logger.Error("Failed to ensure ROS 2 sidecar; skipping RMW",
				zap.String("sidecar", name),
				zap.String("rmw", anchorByName[name].RMW),
				zap.Error(eerr))
			errs = append(errs, fmt.Errorf("sidecar %s: %w", name, eerr))
			continue
		}
		sidecars = append(sidecars, sc)
	}
	if len(sidecars) == 0 {
		return nil, fmt.Errorf("all ROS 2 sidecar builds failed: %w", errors.Join(errs...))
	}
	return sidecars, nil
}

// ensureOneROS2Sidecar starts or reuses the sidecar container named `name`,
// anchored to `anchor` and matching anchor.RMW. Caller holds c.mu and passes a
// namespaced ctx.
func (c *Client) ensureOneROS2Sidecar(ctx context.Context, anchor *services.ROS2Target, name string) (services.ROS2Sidecar, error) {
	if !ros2DistroPattern.MatchString(anchor.Distro) {
		return services.ROS2Sidecar{}, fmt.Errorf("invalid ROS 2 distro %q in container label", anchor.Distro)
	}
	// anchor.RMW is already validated to a known identifier or "" by
	// FindROS2Containers, so it's safe to inject and to use for naming.
	rmw := anchor.RMW

	sidecar := services.ROS2Sidecar{
		Name:     name,
		Distro:   anchor.Distro,
		DomainID: anchor.DomainID,
		RMW:      rmw,
	}

	// Reuse the existing sidecar when it is running and still anchored to this
	// RMW's live ROS 2 container.
	if existing, lerr := c.client.LoadContainer(ctx, name); lerr == nil {
		labels, _ := existing.Labels(ctx)
		anchorAlive := labels[labelKeyROS2AnchorID] == anchor.ContainerID &&
			labels[labelKeyROS2AnchorPID] == strconv.FormatUint(uint64(anchor.TaskPID), 10) &&
			labels[labelKeyROS2Sidecar] == anchor.Distro &&
			virtualRobotOverlayLabelsMatch(labels, anchor)
		if anchorAlive {
			if task, terr := existing.Task(ctx, nil); terr == nil {
				if st, serr := task.Status(ctx); serr == nil && st.Status == containerd.Running {
					return sidecar, nil
				}
			}
		}
		c.logger.Info("Recreating stale ROS 2 sidecar",
			zap.String("sidecar", name), zap.String("anchor", anchor.ContainerID), zap.Bool("anchor_alive", anchorAlive))
		if c.sidecarHasActiveExecsLocked(name) {
			c.logger.Info("Deferring stale ROS 2 sidecar recreation: exec in flight",
				zap.String("sidecar", name))
			return sidecar, nil
		}
		if derr := c.deleteROS2Sidecar(ctx, existing); derr != nil {
			return services.ROS2Sidecar{}, fmt.Errorf("removing stale ROS 2 sidecar: %w", derr)
		}
	}

	// Load the anchor container for the anchor-image reuse path below.
	anchorCtr, err := c.client.LoadContainer(ctx, anchor.ContainerID)
	if err != nil {
		return services.ROS2Sidecar{}, fmt.Errorf("loading ROS 2 anchor container %q: %w", anchor.ContainerID, err)
	}

	// Pick the sidecar image.
	//
	// The stock docker.io/library/ros:<distro> image ships only FastRTPS
	// (librmw_fastrtps_cpp; no CycloneDDS). It is used only for an explicit
	// rmw_fastrtps_cpp anchor — the verified path that does not depend on the
	// app image carrying the ros2 CLI (WDY-1593).
	//
	// For every other RMW — including empty (which Wendy's config layer
	// resolves to CycloneDDS, appconfig.ROS2DefaultRMW, WDY-1703) — reuse the
	// anchor app's own image: it already carries the matching RMW + ros2 CLI
	// and is already pulled and unpacked on the device.
	var image containerd.Image
	reusedAnchorImage := false
	if rmw == rosImageDefaultRMW {
		// stock docker.io/library/ros:<distro> ships only FastRTPS
		imageName := "docker.io/library/ros:" + anchor.Distro
		image, err = c.client.GetImage(ctx, imageName)
		if err != nil {
			c.logger.Info("Pulling ROS 2 sidecar image", zap.String("image", imageName))
			image, err = c.client.Pull(ctx, imageName, containerd.WithPullUnpack)
			if err != nil {
				return services.ROS2Sidecar{}, fmt.Errorf("pulling ROS 2 sidecar image %q: %w", imageName, err)
			}
		}
		if unpacked, uerr := image.IsUnpacked(ctx, ""); uerr == nil && !unpacked {
			if uerr := c.UnpackImage(ctx, image, nil); uerr != nil {
				return services.ROS2Sidecar{}, fmt.Errorf("unpacking ROS 2 sidecar image: %w", uerr)
			}
		}
	} else {
		// CycloneDDS (incl. the empty/config default) and other RMWs reuse the
		// anchor app's own image, which carries the matching RMW + ros2 CLI.
		// The anchor image is already pulled/unpacked (the anchor is running),
		// so skip the pull/unpack dance entirely.
		image, err = anchorCtr.Image(ctx)
		if err != nil {
			return services.ROS2Sidecar{}, fmt.Errorf("resolving anchor image for %s sidecar (anchor %s): %w", rmw, anchor.ContainerID, err)
		}
		reusedAnchorImage = true
		c.logger.Info("ROS 2 sidecar reusing anchor app image",
			zap.String("rmw", rmw), zap.String("image", image.Name()), zap.String("anchor", anchor.ContainerID))
	}

	// 0o750, not 0o755: rosbag2 recordings are raw sensor data (camera frames,
	// lidar, audio, positions) and there is no reason for every local account on
	// the device to be able to read them.
	if err := os.MkdirAll(ROS2BagDir, ROS2BagDirMode); err != nil {
		return services.ROS2Sidecar{}, fmt.Errorf("creating bag directory: %w", err)
	}

	spec := localoci.DefaultSpec("rootfs", []string{"sleep", "infinity"})
	localoci.DropToMinimalCapabilities(spec)
	spec.Process.Env = append(spec.Process.Env, "ROS_LOCALHOST_ONLY=1")
	spec.Mounts = append(spec.Mounts, localoci.Mount{
		Destination: ROS2BagDir,
		Type:        "bind",
		Source:      ROS2BagDir,
		Options:     []string{"rbind", "rw", "nosuid", "nodev"},
	})

	// Match the anchor app group's namespace topology so the sidecar can read
	// the data plane, not just DDS discovery (WDY-1555). A shared-ipc group
	// keeps FastRTPS sample data AND ros_discovery_info — the node records that
	// back `ros2 node list`/`echo`/`graph`/`param`/`call` — in POSIX shared
	// memory under /run/wendy/shm/<appID>, reachable only by sharing the group's
	// IPC namespace and bind-mounting that segment. Joining only the network
	// namespace gets UDP discovery (so `topics`/`services` list) but never the
	// shared-memory data plane. We detect shared-ipc by the presence of the
	// group's shm dir (created only for shared-ipc groups) and then mirror the
	// app containers' own setup. Plain shared-network apps put everything on the
	// shared localhost over UDP, so the network namespace alone is enough.
	isolation := "shared-network"
	var sidecarSHM string
	if anchor.AppID != "" {
		if shmPath, perr := sharedSHMPath(anchor.AppID); perr == nil {
			if fi, statErr := os.Stat(shmPath); statErr == nil && fi.IsDir() {
				isolation = "shared-ipc"
				sidecarSHM = shmPath
			}
		}
	}

	nsAnchors, err := localoci.JoinGroupNamespaces(spec, anchor.TaskPID, isolation)
	if err != nil {
		return services.ROS2Sidecar{}, fmt.Errorf("joining ROS 2 app %s namespaces: %w", isolation, err)
	}
	defer func() {
		for _, f := range nsAnchors {
			f.Close()
		}
	}()
	// For shared-ipc groups using FastRTPS, replace the sidecar's private tmpfs
	// /dev/shm with the group's shared segment so it attaches to the same
	// FastRTPS shm pool. CycloneDDS has SharedMemory disabled in
	// cycloneDDSInlineConfig so the bind would be a no-op for it; skip it to
	// avoid unnecessary mount complexity (M1, WDY-1706).
	if isolation == "shared-ipc" && anchor.RMW == "rmw_fastrtps_cpp" {
		localoci.RemoveDefaultSHM(spec)
		spec.Mounts = append(spec.Mounts, localoci.SharedSHMMount(sidecarSHM))
	}

	specJSON, err := json.Marshal(spec)
	if err != nil {
		return services.ROS2Sidecar{}, fmt.Errorf("marshaling sidecar OCI spec: %w", err)
	}

	labels := map[string]string{
		labelKeyROS2Sidecar:   anchor.Distro,
		labelKeyROS2AnchorID:  anchor.ContainerID,
		labelKeyROS2AnchorPID: strconv.FormatUint(uint64(anchor.TaskPID), 10),
		labelKeyROS2RMW:       rmw,
	}
	switch virtualRobotROS2Kind(anchor) {
	case "go2":
		labels[labelKeyGo2Overlay] = go2OverlayVersion
	case "g1":
		labels[labelKeyG1Overlay] = g1OverlayVersion
	}
	container, err := c.client.NewContainer(ctx, name,
		containerd.WithImage(image),
		containerd.WithNewSnapshot(name, image),
		containerd.WithContainerLabels(labels),
		containerd.WithNewSpec(oci.WithSpecFromBytes(specJSON)),
	)
	if err != nil {
		return services.ROS2Sidecar{}, fmt.Errorf("creating ROS 2 sidecar container: %w", err)
	}

	task, err := container.NewTask(ctx, cio.NullIO)
	if err != nil {
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
		return services.ROS2Sidecar{}, fmt.Errorf("creating ROS 2 sidecar task: %w", err)
	}
	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(ctx, containerd.WithProcessKill)
		_ = container.Delete(ctx, containerd.WithSnapshotCleanup)
		return services.ROS2Sidecar{}, fmt.Errorf("starting ROS 2 sidecar: %w", err)
	}

	// Close the residual TOCTOU window of the raw /proc/<pid>/ns paths: if
	// the anchor container exited (and its PID was possibly recycled) while
	// the sidecar was starting, the sidecar may have joined the wrong
	// namespace. Re-verify the anchor task is still the same live PID;
	// otherwise tear the sidecar down and fail (SOC2-CC6, NIST-SC-7).
	if err := c.verifyROS2Anchor(ctx, anchor); err != nil {
		_ = c.deleteROS2Sidecar(ctx, container)
		return services.ROS2Sidecar{}, fmt.Errorf("ROS 2 app container changed while sidecar was starting; retry: %w", err)
	}

	// A reused app image is guaranteed to carry the matching RMW (that is how
	// the app runs it) but not necessarily the ros2 CLI. If it lacks the CLI,
	// fail with an actionable message instead of letting every command return a
	// bare "ros2: not found" exit 127 (WDY-1593).
	if reusedAnchorImage {
		if hasCLI, perr := c.sidecarHasROS2CLI(ctx, container, task, anchor.Distro); perr != nil {
			c.logger.Warn("Could not probe reused app image for the ros2 CLI; proceeding",
				zap.String("image", image.Name()), zap.Error(perr))
		} else if !hasCLI {
			_ = c.deleteROS2Sidecar(ctx, container)
			return services.ROS2Sidecar{}, fmt.Errorf("ROS 2 app image %q runs %s but does not include the ros2 CLI, so `wendy device ros2` cannot inspect it; install the CLI in the app image (e.g. apt-get install ros-%s-ros2cli)", image.Name(), rmw, anchor.Distro)
		}
	}

	c.logger.Info("ROS 2 CLI sidecar started",
		zap.String("image", image.Name()),
		zap.String("rmw", rmw),
		zap.String("anchor", anchor.ContainerID),
		zap.Int("domain_id", anchor.DomainID))
	return sidecar, nil
}

// ros2ShellArgs builds the argv for running `script` inside the sidecar, with
// `extra` appended as positional arguments ("$@" starts at $1).
//
// Uses `/bin/sh` rather than `/bin/bash`. Every non-FastRTPS RMW — which includes
// the CycloneDDS default, i.e. the common case — reuses the *app's own image* as
// the sidecar image, and a slim ROS image (the natural target for a C++ or Swift
// node) frequently ships no bash. That made the CLI probe fail with an opaque
// setup error and then every `wendy device ros2` command fail with a bare exec
// error. `source` is a bashism, so it becomes `.`, which is POSIX and works in
// bash too.
//
// Split out as a pure function so the shell contract is unit-testable without
// containerd.
func ros2ShellArgs(distro, script string, extra []string) []string {
	args := []string{"/bin/sh", "-c", script}
	if extra != nil {
		// argv[0] for the script's positional parameters; "$@" then starts at the
		// first element of extra.
		args = append(args, "ros2")
		args = append(args, extra...)
	}
	return args
}

// ros2SetupScript is the ROS environment script to dot-source for a distro.
//
// It must be setup.sh, not setup.bash. The sidecar runs commands under /bin/sh
// (a slim app image may ship no bash), and on Ubuntu-based ROS images /bin/sh
// is dash. ament's setup.bash resolves its own location via ${BASH_SOURCE[0]}
// and declares AMENT_SHELL=bash, so dash aborts on it — and because the sourcing
// is silenced, the only visible symptom was `command -v ros2` failing, which the
// probe then reported as "the image does not include the ros2 CLI" for images
// that plainly do. ament generates setup.sh alongside it for exactly this case:
// it avoids BASH_SOURCE and bakes the install prefix in at generation time.
func ros2SetupScript(distro string) string {
	return fmt.Sprintf("/opt/ros/%s/setup.sh", distro)
}

// ros2SourceAndExec is the shell script the sidecar runs for a `ros2` invocation:
// source the ROS environment, then exec the CLI with the caller's arguments kept
// out of shell interpretation via "$@" (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
func ros2SourceAndExec(distro string) string {
	return fmt.Sprintf(". %s >/dev/null 2>&1 && exec ros2 \"$@\"", ros2SetupScript(distro))
}

// sidecarHasROS2CLI reports whether the running sidecar task can find the ros2
// CLI on PATH after sourcing the ROS environment. Used to fail loudly when a
// reused app image shipped the RMW but not ros2cli (WDY-1593). A probe-setup
// error (not a clean exit code) is returned so the caller can proceed rather
// than block on a transient containerd hiccup.
func (c *Client) sidecarHasROS2CLI(ctx context.Context, container containerd.Container, task containerd.Task, distro string) (bool, error) {
	spec, err := container.Spec(ctx)
	if err != nil {
		return false, err
	}
	pspec := spec.Process
	pspec.Terminal = false
	pspec.Args = ros2ShellArgs(distro,
		fmt.Sprintf(". %s >/dev/null 2>&1; command -v ros2 >/dev/null 2>&1", ros2SetupScript(distro)),
		nil)
	execID := fmt.Sprintf("ros2-probe-%d", ros2ExecCounter.Add(1))
	proc, err := task.Exec(ctx, execID, pspec, cio.NullIO)
	if err != nil {
		return false, err
	}
	defer func() { _, _ = proc.Delete(ctx, containerd.WithProcessKill) }()
	statusC, err := proc.Wait(ctx)
	if err != nil {
		return false, err
	}
	if err := proc.Start(ctx); err != nil {
		return false, err
	}
	st := <-statusC
	return st.ExitCode() == 0, st.Error()
}

// verifyROS2Anchor checks that the anchor container's task is still running
// with the same PID recorded before the sidecar joined its namespaces.
func (c *Client) verifyROS2Anchor(ctx context.Context, anchor *services.ROS2Target) error {
	ctr, err := c.client.LoadContainer(ctx, anchor.ContainerID)
	if err != nil {
		return fmt.Errorf("anchor container gone: %w", err)
	}
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		return fmt.Errorf("anchor task gone: %w", err)
	}
	if st, err := task.Status(ctx); err != nil || st.Status != containerd.Running {
		return fmt.Errorf("anchor task no longer running")
	}
	if task.Pid() != anchor.TaskPID {
		return fmt.Errorf("anchor task PID changed (%d → %d)", anchor.TaskPID, task.Pid())
	}
	return nil
}

// VerifyROS2Sidecar reports whether every running sidecar is still anchored to a
// live ROS 2 app container. A stopped or replaced anchor (app redeploy) tears
// down the namespace the sidecar joined, invalidating any in-flight command —
// most visibly a bag recording session.
func (c *Client) VerifyROS2Sidecar(ctx context.Context) error {
	ctx = c.withNamespace(ctx)
	sidecars, err := c.listROS2Sidecars(ctx)
	if err != nil {
		return fmt.Errorf("listing ROS 2 sidecars: %w", err)
	}
	if len(sidecars) == 0 {
		return fmt.Errorf("sidecar not found")
	}
	for _, sc := range sidecars {
		labels, lerr := sc.Labels(ctx)
		if lerr != nil {
			return fmt.Errorf("reading sidecar labels: %w", lerr)
		}
		if verr := c.verifyAppROS2SidecarAnchor(ctx, sc.ID(), labels); verr != nil {
			return verr
		}
	}
	return nil
}

// VerifyROS2SidecarNamed checks the sidecar used by one recording. A standalone
// system CLI has no app anchor; unrelated app sidecars must not affect its
// diagnosis, and an idle system CLI must not mask a lost app anchor.
func (c *Client) VerifyROS2SidecarNamed(ctx context.Context, name string) error {
	ctx = c.withNamespace(ctx)
	sc, err := c.client.LoadContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("loading ROS 2 sidecar %q: %w", name, err)
	}
	labels, err := sc.Labels(ctx)
	if err != nil {
		return fmt.Errorf("reading sidecar labels: %w", err)
	}
	if name == ros2SystemSidecarName || labels[labelKeyROS2SystemSidecar] != "" {
		if name != ros2SystemSidecarName || labels[labelKeyROS2SystemSidecar] != "humble" || labels[labelKeyROS2HostSidecar] != "" || labels[labelKeyROS2Sidecar] != "" {
			return fmt.Errorf("invalid system ROS 2 CLI identity")
		}
		task, err := sc.Task(ctx, nil)
		if err != nil {
			return fmt.Errorf("system ROS 2 CLI task unavailable: %w", err)
		}
		st, err := task.Status(ctx)
		if err != nil {
			return fmt.Errorf("checking system ROS 2 CLI task: %w", err)
		}
		if st.Status != containerd.Running {
			return fmt.Errorf("system ROS 2 CLI task no longer running")
		}
		return nil
	}
	if !ros2DistroPattern.MatchString(labels[labelKeyROS2Sidecar]) || labels[labelKeyROS2HostSidecar] != "" {
		return fmt.Errorf("invalid app ROS 2 sidecar identity")
	}
	return c.verifyAppROS2SidecarAnchor(ctx, name, labels)
}

func (c *Client) verifyAppROS2SidecarAnchor(ctx context.Context, name string, labels map[string]string) error {
	anchorPID, err := strconv.ParseUint(labels[labelKeyROS2AnchorPID], 10, 32)
	if err != nil {
		return fmt.Errorf("sidecar %s has no valid anchor PID label", name)
	}
	return c.verifyROS2Anchor(ctx, &services.ROS2Target{
		ContainerID: labels[labelKeyROS2AnchorID],
		TaskPID:     uint32(anchorPID),
	})
}

// StopROS2Sidecar stops and removes all ROS 2 CLI sidecars if present.
func (c *Client) StopROS2Sidecar(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.teardownAllROS2SidecarsLocked(c.withNamespace(ctx))
	for name, label := range map[string]string{ros2HostSidecarName: labelKeyROS2HostSidecar, ros2SystemSidecarName: labelKeyROS2SystemSidecar} {
		if c.sidecarHasActiveExecsLocked(name) {
			continue
		}
		if inspector, err := c.client.LoadContainer(c.withNamespace(ctx), name); err == nil {
			labels, lerr := inspector.Labels(c.withNamespace(ctx))
			if lerr == nil && labels[label] != "" {
				if err := c.deleteROS2Sidecar(c.withNamespace(ctx), inspector); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// teardownAllROS2SidecarsLocked removes every sidecar container that has no
// active ExecROS2 calls. Sidecars with in-flight execs are skipped (logged).
// Caller must hold c.mu and pass a namespaced ctx.
func (c *Client) teardownAllROS2SidecarsLocked(ctx context.Context) {
	sidecars, err := c.listROS2Sidecars(ctx)
	if err != nil {
		return
	}
	for _, ctr := range sidecars {
		if c.sidecarHasActiveExecsLocked(ctr.ID()) {
			c.logger.Info("Deferring ROS 2 sidecar teardown: exec in flight",
				zap.String("sidecar", ctr.ID()))
			continue
		}
		_ = c.deleteROS2Sidecar(ctx, ctr)
	}
}

// isOrphanedSidecar reports whether a sidecar is orphaned. A sidecar is
// orphaned when its anchor PID is not present in the live-task set, or when
// the live task at that PID belongs to a different container (PID recycled).
// liveTasks maps running task PID → container ID.
// This is a pure function so it can be unit-tested without containerd.
func isOrphanedSidecar(anchorID string, anchorPID uint32, liveTasks map[uint32]string) bool {
	liveID, ok := liveTasks[anchorPID]
	return !ok || liveID != anchorID
}

// shouldReapSidecar decides whether a sidecar should be reaped. It is orphaned
// only when its anchor is confirmably gone; if the anchor's liveness could not
// be verified (anchorID in unresolvable), the sidecar is KEPT — a boot reaper
// must never delete a sidecar whose anchor might still be alive (WDY-1702 H4).
func shouldReapSidecar(anchorID string, anchorPID uint32, liveTasks map[uint32]string, unresolvable map[string]bool) bool {
	if unresolvable[anchorID] {
		return false
	}
	return isOrphanedSidecar(anchorID, anchorPID, liveTasks)
}

// ReapOrphanedROS2Sidecars deletes any ROS 2 CLI sidecar containers whose
// anchor app task is no longer running. It is called once at agent boot after
// the containerd client is ready, so sidecars orphaned by a prior agent crash
// or ungraceful shutdown are cleaned up before new workloads start.
//
// A sidecar is only deleted when its anchor container ID + PID no longer match
// a live running task. Sidecars with active ExecROS2 calls in flight are
// skipped (consistent with teardownAllROS2SidecarsLocked).
func (c *Client) ReapOrphanedROS2Sidecars(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx = c.withNamespace(ctx)

	// Build the live-task map: PID → container ID, for all running Wendy app
	// containers. Sidecars carry labelKeyROS2Sidecar (not labelKeyAppVersion),
	// so they are naturally excluded from this set.
	liveTasks := make(map[uint32]string)
	unresolvable := make(map[string]bool)
	appCtrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppVersion))
	if err != nil {
		return fmt.Errorf("listing app containers for sidecar reap: %w", err)
	}
	for _, ctr := range appCtrs {
		// WDY-1702 H4: distinguish definitive "anchor gone" from ambiguous errors.
		// NotFound from Task() means the container has no task — it cleanly stopped
		// or never started. That is precisely the orphan case: do NOT mark it
		// unresolvable; omitting it from liveTasks lets its sidecar be reaped.
		// Any other error (timeout, transient RPC) means liveness is genuinely
		// unknown — mark unresolvable so its sidecar is kept (safety, WDY-1702 H4).
		task, terr := ctr.Task(ctx, nil)
		if terr != nil {
			if errdefs.IsNotFound(terr) {
				// Anchor definitively has no task (cleanly stopped/gone): reapable.
				continue
			}
			// Liveness unknown: keep sidecar to avoid false-positive reap.
			c.logger.Warn("ROS 2 sidecar reap: could not get task for app container, treating as unresolvable",
				zap.String(logfields.ContainerID, ctr.ID()), zap.Error(terr))
			unresolvable[ctr.ID()] = true
			continue
		}
		st, serr := task.Status(ctx)
		if serr != nil {
			if errdefs.IsNotFound(serr) {
				// Task record gone between Task() and Status(): definitively stopped.
				continue
			}
			// Status unknown: keep sidecar to avoid false-positive reap.
			c.logger.Warn("ROS 2 sidecar reap: could not get task status for app container, treating as unresolvable",
				zap.String(logfields.ContainerID, ctr.ID()), zap.Error(serr))
			unresolvable[ctr.ID()] = true
			continue
		}
		if st.Status != containerd.Running {
			continue
		}
		liveTasks[task.Pid()] = ctr.ID()
	}

	sidecars, err := c.listROS2Sidecars(ctx)
	if err != nil {
		return fmt.Errorf("listing ROS 2 sidecars for reap: %w", err)
	}

	for _, sc := range sidecars {
		labels, lerr := sc.Labels(ctx)
		if lerr != nil {
			c.logger.Warn("ROS 2 sidecar reap: could not read labels, skipping",
				zap.String("sidecar", sc.ID()), zap.Error(lerr))
			continue
		}
		anchorID := labels[labelKeyROS2AnchorID]
		anchorPIDStr := labels[labelKeyROS2AnchorPID]
		anchorPID64, perr := strconv.ParseUint(anchorPIDStr, 10, 32)
		if perr != nil {
			c.logger.Warn("ROS 2 sidecar reap: invalid anchor PID label, treating as orphaned",
				zap.String("sidecar", sc.ID()), zap.String("pid", anchorPIDStr))
			// Fall through: isOrphanedSidecar with PID 0 will return true.
		}
		anchorPID := uint32(anchorPID64)

		if !shouldReapSidecar(anchorID, anchorPID, liveTasks, unresolvable) {
			continue
		}
		if c.sidecarHasActiveExecsLocked(sc.ID()) {
			c.logger.Info("ROS 2 sidecar reap: exec in flight, skipping",
				zap.String("sidecar", sc.ID()))
			continue
		}
		c.logger.Info("ROS 2 sidecar reap: deleting orphaned sidecar",
			zap.String("sidecar", sc.ID()),
			zap.String("anchor_id", anchorID),
			zap.Uint32("anchor_pid", anchorPID))
		if derr := c.deleteROS2Sidecar(ctx, sc); derr != nil {
			c.logger.Warn("ROS 2 sidecar reap: delete failed",
				zap.String("sidecar", sc.ID()), zap.Error(derr))
		}
	}
	return nil
}

func (c *Client) deleteROS2Sidecar(ctx context.Context, container containerd.Container) error {
	if task, err := container.Task(ctx, nil); err == nil {
		if termErr := c.terminateTask(ctx, task, container.ID(), syscall.SIGKILL, killWaitTimeout, killWaitTimeout); termErr != nil {
			c.logger.Warn("Failed to delete ROS 2 sidecar task",
				zap.String("sidecar", container.ID()),
				zap.Error(termErr))
		}
	}
	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return err
	}
	return nil
}

// Locking order for ros2ExecRefs (WDY-1702 H5):
//
//   - acquireSidecarExec and releaseSidecarExec each take c.mu briefly only
//     for the map mutation, then immediately release it.
//   - ExecROS2 calls acquire/release around task.Exec+proc.Wait but does NOT
//     hold c.mu for the full duration of the exec. This is intentional:
//     EnsureROS2Sidecars and StopROS2Sidecar hold c.mu for their entire body,
//     so if ExecROS2 also held c.mu across a long exec the two would serialize
//     and risk deadlock.
//   - Teardown callers (EnsureROS2Sidecars, teardownAllROS2SidecarsLocked)
//     already hold c.mu when they need to test the refcount, so they use the
//     sidecarHasActiveExecsLocked variant which skips the redundant lock.

// acquireSidecarExec increments the active-exec refcount for the named sidecar.
// It takes c.mu briefly; callers must NOT hold c.mu.
func (c *Client) acquireSidecarExec(name string) {
	c.mu.Lock()
	if c.ros2ExecRefs == nil {
		c.ros2ExecRefs = make(map[string]int)
	}
	c.ros2ExecRefs[name]++
	c.mu.Unlock()
}

// acquireSidecarExecCapped is like acquireSidecarExec but returns an error when
// the per-sidecar exec count would exceed ros2MaxConcurrentExecs (L6, WDY-1706).
// Callers must NOT hold c.mu.
func (c *Client) acquireSidecarExecCapped(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ros2ExecRefs == nil {
		c.ros2ExecRefs = make(map[string]int)
	}
	if c.ros2ExecRefs[name] >= ros2MaxConcurrentExecs {
		return fmt.Errorf("too many concurrent ROS 2 execs on sidecar %q (limit %d); retry later", name, ros2MaxConcurrentExecs)
	}
	c.ros2ExecRefs[name]++
	return nil
}

// releaseSidecarExec decrements the active-exec refcount for the named sidecar.
// It takes c.mu briefly; callers must NOT hold c.mu.
func (c *Client) releaseSidecarExec(name string) {
	c.mu.Lock()
	if c.ros2ExecRefs[name] > 0 {
		c.ros2ExecRefs[name]--
	}
	if c.ros2ExecRefs[name] == 0 {
		delete(c.ros2ExecRefs, name)
	}
	c.mu.Unlock()
}

// sidecarHasActiveExecsLocked reports whether name has any active ExecROS2
// calls in flight. Caller must hold c.mu.
func (c *Client) sidecarHasActiveExecsLocked(name string) bool {
	return c.ros2ExecRefs[name] > 0
}

// ros2ExecArgs builds an invocation from either CLI arguments or validated
// options for the fixed LiDAR probe. All caller-controlled values remain argv
// entries, never shell or Python source. Host inspection retains its CLI
// allowlist and additionally permits only this trusted read-only probe.
func ros2ExecArgs(distro string, go2Overlay, hostInspector bool, opts services.ROS2ExecOptions) ([]string, error) {
	kind := ""
	if go2Overlay {
		kind = "go2"
	}
	return ros2ExecArgsForRobot(distro, kind, hostInspector, opts)
}

func ros2ExecArgsForRobot(distro, robotKind string, hostInspector bool, opts services.ROS2ExecOptions) ([]string, error) {
	if !ros2DistroPattern.MatchString(distro) {
		return nil, fmt.Errorf("invalid distro %q on ROS 2 sidecar", distro)
	}
	if robotKind != "" && robotKind != "go2" && robotKind != "g1" && robotKind != "unitree" {
		return nil, fmt.Errorf("invalid virtual robot ROS overlay kind")
	}
	if hostInspector {
		robotKind = "unitree"
	}
	script := ros2SourceAndExecForRobot(distro, robotKind)
	if opts.Lidar != nil {
		if len(opts.Args) != 0 {
			return nil, fmt.Errorf("LiDAR inspection cannot include ROS 2 CLI arguments")
		}
		if err := opts.Lidar.Validate(); err != nil {
			return nil, fmt.Errorf("invalid LiDAR inspection options: %w", err)
		}
		options, err := json.Marshal(opts.Lidar)
		if err != nil {
			return nil, fmt.Errorf("encoding LiDAR inspection options: %w", err)
		}
		// Keep exactly the same trusted ROS setup and optional Unitree overlay
		// as CLI commands; replace only the fixed executable at the end.
		script = strings.Replace(script, `exec ros2 "$@"`, `exec python3 -u -c "$@"`, 1)
		return ros2ShellArgs(distro, script, []string{ros2inspection.LidarProbeScript, string(options)}), nil
	}
	args := opts.Args
	if hostInspector {
		var err error
		args, err = hostROS2InspectionArgs(args)
		if err != nil {
			return nil, err
		}
	}
	return ros2ShellArgs(distro, script, args), nil
}

// ExecROS2 runs `ros2 <args...>` or the trusted LiDAR probe inside the CLI sidecar, streaming stdout and
// stderr to the given writers, and returns the command's exit code. When ctx
// is cancelled the process receives SIGINT and, after a grace period, SIGKILL
// — the SIGINT-first order lets `ros2 bag record` finalize its output.
// EnsureROS2Sidecar must have succeeded before calling ExecROS2.
func (c *Client) ExecROS2(ctx context.Context, opts services.ROS2ExecOptions, stdout, stderr io.Writer) (int, error) {
	nctx := c.withNamespace(context.WithoutCancel(ctx))

	name := opts.SidecarName
	if name == "" {
		// No specific sidecar requested: use any running one (single-RMW case).
		if scs, lerr := c.listROS2Sidecars(nctx); lerr == nil && len(scs) > 0 {
			name = scs[0].ID()
		} else {
			name = ros2SidecarName("")
		}
	}
	container, err := c.client.LoadContainer(nctx, name)
	if err != nil {
		return -1, fmt.Errorf("ROS 2 sidecar not available: %w", err)
	}
	labels, err := container.Labels(nctx)
	if err != nil {
		return -1, fmt.Errorf("reading ROS 2 sidecar labels: %w", err)
	}
	distro := labels[labelKeyROS2Sidecar]
	hostInspector := labels[labelKeyROS2HostSidecar] != ""
	systemCLI := labels[labelKeyROS2SystemSidecar] != ""
	if systemCLI {
		if name != ros2SystemSidecarName || labels[labelKeyROS2SystemSidecar] != "humble" || hostInspector {
			return -1, fmt.Errorf("invalid system ROS 2 CLI identity")
		}
		distro = labels[labelKeyROS2SystemSidecar]
	}
	if hostInspector {
		if name != ros2HostSidecarName || labels[labelKeyROS2HostSidecar] != "humble" {
			return -1, fmt.Errorf("invalid host inspector identity")
		}
		distro = labels[labelKeyROS2HostSidecar]
	}
	if opts.DomainID < appconfig.ROS2DomainIDMin || opts.DomainID > appconfig.ROS2DomainIDMax {
		return -1, fmt.Errorf("domain ID %d out of range [%d,%d]", opts.DomainID, appconfig.ROS2DomainIDMin, appconfig.ROS2DomainIDMax)
	}
	robotKind := ""
	if hostInspector || systemCLI {
		if labels[labelKeyROS2InspectorVersion] != ros2InspectorVersion {
			return -1, fmt.Errorf("standalone ROS 2 inspector needs an image upgrade; retry inspection")
		}
		robotKind = "unitree"
	}
	if !hostInspector && !systemCLI {
		robotKind, err = virtualRobotOverlayKind(labels)
		if err != nil {
			return -1, err
		}
	}
	args, err := ros2ExecArgsForRobot(distro, robotKind, hostInspector, opts)
	if err != nil {
		return -1, err
	}

	task, err := container.Task(nctx, nil)
	if err != nil {
		return -1, fmt.Errorf("ROS 2 sidecar task not running: %w", err)
	}

	spec, err := container.Spec(nctx)
	if err != nil {
		return -1, fmt.Errorf("reading ROS 2 sidecar spec: %w", err)
	}
	pspec := spec.Process
	// The ROS environment lives in /opt/ros/<distro>/setup.sh; the "$@"
	// indirection keeps user-supplied args out of shell interpretation
	// (SOC2-CC6, ISO27001-A.8, NIST-SI-10). See ros2ShellArgs for why this is
	// /bin/sh rather than /bin/bash.
	pspec.Terminal = false
	pspec.Args = args
	// Copy pspec.Env before appending to avoid mutating the slice header returned
	// by container.Spec (future callers might cache the spec or share the backing
	// array across execs — defensive copy prevents env bleed-over, L5, WDY-1706).
	//
	// ROS_AUTOMATIC_DISCOVERY_RANGE accompanies ROS_LOCALHOST_ONLY for the same
	// reason as in buildROS2Env: the latter has been deprecated since Iron, and the
	// sidecar has to see the same graph as the app it is anchored to.
	pspec.Env = append(append([]string(nil), pspec.Env...),
		"ROS_DOMAIN_ID="+strconv.Itoa(opts.DomainID),
	)
	discoveryEnv, err := c.ros2SidecarDiscoveryEnv(nctx, labels, hostInspector || systemCLI)
	if err != nil {
		return -1, err
	}
	pspec.Env = ros2WithDiscoveryEnv(pspec.Env, discoveryEnv)
	// Match the anchor app's RMW so the CLI speaks the same DDS implementation;
	// otherwise it falls to the image default and sees nothing on another RMW
	// (WDY-1593). The label is written validated, but re-check before injecting
	// into the environment as defense-in-depth (SOC2-CC6, NIST-SI-10).
	if rmw := labels[labelKeyROS2RMW]; appconfig.IsValidRMWImplementation(rmw) {
		pspec.Env = append(pspec.Env, "RMW_IMPLEMENTATION="+rmw)
		if rmw == appconfig.ROS2DefaultRMW {
			pspec.Env = append(pspec.Env, "CYCLONEDDS_URI="+cycloneDDSInlineConfig)
		}
	}

	// Increment the per-sidecar exec refcount so that teardown (EnsureROS2Sidecars /
	// StopROS2Sidecar) defers deletion while this exec is in flight. The acquire and
	// release each take c.mu only briefly; c.mu is NOT held across task.Exec or
	// proc.Wait (see locking-order comment above deleteROS2Sidecar).
	// acquireSidecarExecCapped rejects the call when the per-sidecar cap is reached
	// (ros2MaxConcurrentExecs) to prevent exhausting the containerd exec table (L6, WDY-1706).
	if err := c.acquireSidecarExecCapped(name); err != nil {
		return -1, err
	}
	defer c.releaseSidecarExec(name)

	execID := fmt.Sprintf("ros2-exec-%d-%d", time.Now().UnixNano(), ros2ExecCounter.Add(1))
	proc, err := task.Exec(nctx, execID, pspec, cio.NewCreator(cio.WithStreams(nil, stdout, stderr)))
	if err != nil {
		return -1, fmt.Errorf("exec into ROS 2 sidecar: %w", err)
	}
	defer func() {
		_, _ = proc.Delete(nctx, containerd.WithProcessKill)
	}()

	statusC, err := proc.Wait(nctx)
	if err != nil {
		return -1, fmt.Errorf("waiting on ROS 2 exec: %w", err)
	}
	if err := proc.Start(nctx); err != nil {
		return -1, fmt.Errorf("starting ROS 2 exec: %w", err)
	}

	// drainIO waits for containerd's cio copy goroutines to finish moving the
	// FIFOs into stdout/stderr.
	//
	// The exit status arriving does NOT mean the output has been copied. The
	// deferred proc.Delete then calls cio.Cancel(), which *closes the read ends of
	// the FIFOs* and aborts any copy still in flight (containerd v2
	// pkg/cio/io_unix.go copyIO + client/process.go Delete). So the tail of a
	// command's output could be silently dropped — and every caller here hands
	// stdout straight to a parser, which then reported a partial topic/node list
	// with exit code 0. Waiting here first lets the copies finish naturally: the
	// writer side is already closed by the exited process, so each copy sees a
	// clean EOF.
	drainIO := func() {
		if pio := proc.IO(); pio != nil {
			pio.Wait()
		}
	}

	select {
	case st := <-statusC:
		drainIO()
		return int(st.ExitCode()), st.Error()
	case <-ctx.Done():
		_ = proc.Kill(nctx, syscall.SIGINT)
		select {
		case st := <-statusC:
			drainIO()
			return int(st.ExitCode()), ctx.Err()
		case <-time.After(ros2ExecStopGrace):
			_ = proc.Kill(nctx, syscall.SIGKILL)
			// Bounded: a wedged shim would otherwise hold this RPC (and one of the
			// sidecar's ros2MaxConcurrentExecs slots) forever.
			select {
			case st := <-statusC:
				drainIO()
				return int(st.ExitCode()), ctx.Err()
			case <-time.After(ros2ExecKillWait):
				return -1, fmt.Errorf("ROS 2 exec did not exit within %s of SIGKILL: %w",
					ros2ExecKillWait, ctx.Err())
			}
		}
	}
}
