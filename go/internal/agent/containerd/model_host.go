package containerd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
	"go.uber.org/zap"
)

// Model host containers carry these labels and no sh.wendy/app.* label, so
// app listing, stats, restarts and camera sync never see them (as with the
// ROS 2 inspector). The supervisor owns their lifecycle.
const (
	labelKeyModelInstance = "sh.wendy/model.instance"
	labelKeyModelID       = "sh.wendy/model.id"
	labelKeyModelVariant  = "sh.wendy/model.variant"
	labelKeyModelFile     = "sh.wendy/model.file.sha256"
	modelHostPrefix       = "wendy-model-"
	// modelHostVideoGID opens the camera node, as the camera entitlement's
	// videoGroupGID does.
	modelHostVideoGID = 44
)

var _ models.Runtime = (*Client)(nil)

// resolveModelCamera finds a camera node's device numbers; tests replace it.
var resolveModelCamera = localoci.ResolveDeviceNode

func modelHostName(instanceID string) string { return modelHostPrefix + instanceID }

// HasImage reports whether a model host image is already present.
func (c *Client) HasImage(ctx context.Context, ref string) bool {
	_, err := c.client.GetImage(c.withNamespace(ctx), ref)
	return err == nil
}

// EnsureImage pulls and unpacks a model host image unless it is present.
func (c *Client) EnsureImage(ctx context.Context, ref string) error {
	ctx = c.withNamespace(ctx)
	image, err := c.client.GetImage(ctx, ref)
	if err != nil {
		if image, err = c.client.Pull(ctx, ref, containerd.WithPullUnpack); err != nil {
			return fmt.Errorf("pulling %s: %w", ref, err)
		}
	}
	unpacked, err := image.IsUnpacked(ctx, "")
	if err != nil {
		return fmt.Errorf("checking %s: %w", ref, err)
	}
	if !unpacked {
		if err := c.UnpackImage(ctx, image, nil); err != nil {
			return fmt.Errorf("unpacking %s: %w", ref, err)
		}
	}
	return nil
}

// StartHost creates and starts a model host. The channel receives once,
// when the host's process ends.
func (c *Client) StartHost(ctx context.Context, h models.HostSpec) (<-chan models.HostExit, error) {
	ctx = c.withNamespace(ctx)
	if c.dataSocketProvider == nil {
		return nil, errors.New("app data sockets are unavailable on this agent")
	}
	image, err := c.client.GetImage(ctx, h.Image)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", h.Image, err)
	}
	img, err := imageRunConfig(ctx, image)
	if err != nil {
		return nil, err
	}
	socketDir, err := c.dataSocketProvider.Ensure(h.AppID, "")
	if err != nil {
		return nil, fmt.Errorf("opening the data socket: %w", err)
	}
	release := func() { c.dataSocketProvider.ReleaseApp(h.AppID) }
	spec, err := c.modelHostSpec(h, img, socketDir)
	if err != nil {
		release()
		return nil, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		release()
		return nil, err
	}
	name := modelHostName(h.InstanceID)
	ctr, err := c.client.NewContainer(ctx, name,
		containerd.WithImage(image), containerd.WithNewSnapshot(name, image),
		containerd.WithContainerLabels(map[string]string{
			labelKeyModelInstance: h.InstanceID, labelKeyModelID: h.ModelID,
			labelKeyModelVariant: h.VariantID, labelKeyModelFile: h.FileSHA256,
		}),
		containerd.WithNewSpec(oci.WithSpecFromBytes(specJSON)),
	)
	if err != nil {
		release()
		return nil, fmt.Errorf("creating the model host: %w", err)
	}
	task, err := ctr.NewTask(ctx, cio.LogFile(h.LogPath))
	if err != nil {
		c.abandonModelHost(ctx, h.InstanceID, ctr, nil, release)
		return nil, fmt.Errorf("creating the model host task: %w", err)
	}
	// Wait before Start so an immediate exit is not missed; it outlives ctx.
	statusC, err := task.Wait(context.WithoutCancel(ctx))
	if err != nil {
		c.abandonModelHost(ctx, h.InstanceID, ctr, task, release)
		return nil, fmt.Errorf("waiting on the model host: %w", err)
	}
	if err := task.Start(ctx); err != nil {
		c.abandonModelHost(ctx, h.InstanceID, ctr, task, release)
		return nil, fmt.Errorf("starting the model host: %w", err)
	}
	exits := make(chan models.HostExit, 1)
	go func() {
		st := <-statusC
		code, _, err := st.Result()
		exits <- models.HostExit{Code: code, Err: err}
	}()
	return exits, nil
}

// modelHostCleanupTimeout bounds the whole cleanup after StartHost fails.
// Deletes can wedge on real hardware (task_teardown.go), and StartHost runs
// on the instance's run loop, which holds its slot and camera pin.
var modelHostCleanupTimeout = 10 * time.Second

// failedHostTask and failedHostContainer are the parts of a containerd task
// and container that the cleanup after a failed StartHost uses.
type failedHostTask interface {
	Delete(ctx context.Context, opts ...containerd.ProcessDeleteOpts) (*containerd.ExitStatus, error)
}

type failedHostContainer interface {
	Delete(ctx context.Context, opts ...containerd.DeleteOpts) error
}

// abandonModelHost removes a host whose start failed: its task, if it has
// one, then its container, all within modelHostCleanupTimeout, and releases
// its data socket. What it cannot remove is logged; the supervisor removes
// the host again before any restart.
func (c *Client) abandonModelHost(ctx context.Context, instanceID string, ctr failedHostContainer, task failedHostTask, release func()) {
	defer release()
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelHostCleanupTimeout)
	defer cancel()
	if task != nil {
		if _, err := task.Delete(cleanupCtx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
			c.logger.Warn("cleaning up a model host that failed to start: deleting its task failed", zap.String("instance", instanceID), zap.Error(err))
		}
	}
	if err := ctr.Delete(cleanupCtx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		c.logger.Warn("cleaning up a model host that failed to start: deleting its container failed", zap.String("instance", instanceID), zap.Error(err))
	}
}

// RemoveHost stops and deletes a model host and releases its data socket.
func (c *Client) RemoveHost(ctx context.Context, instanceID string) error {
	ctx = c.withNamespace(ctx)
	defer func() {
		if c.dataSocketProvider != nil {
			c.dataSocketProvider.ReleaseApp(models.AppIDPrefix + instanceID)
		}
	}()
	ctr, err := c.client.LoadContainer(ctx, modelHostName(instanceID))
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if task, err := ctr.Task(ctx, nil); err == nil {
		if err := c.terminateTask(ctx, task, ctr.ID(), syscall.SIGTERM, stopGracePeriod, killWaitTimeout); err != nil {
			c.logger.Warn("stopping a model host failed", zap.String("instance", instanceID), zap.Error(err))
		}
	}
	if err := ctr.Delete(ctx, containerd.WithSnapshotCleanup); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("deleting the model host: %w", err)
	}
	return nil
}

// ListHosts returns the instance ids of every model host container.
func (c *Client) ListHosts(ctx context.Context) ([]string, error) {
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyModelInstance))
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, ctr := range ctrs {
		// Containers already returned this container's metadata; re-read it
		// from that cached record instead of an extra per-container RPC. A
		// transient error here must fail the whole list, not silently drop a
		// host: CleanupOrphans trusts ListHosts to name every model host
		// container so it can delete them all at boot, and a skipped host
		// would keep running, invisible to app listings by design.
		info, err := ctr.Info(ctx, containerd.WithoutRefreshedMetadata)
		if err != nil {
			return nil, fmt.Errorf("reading labels for model host %s: %w", ctr.ID(), err)
		}
		if id := info.Labels[labelKeyModelInstance]; id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// modelHostImage is the part of an image's config a model host inherits.
type modelHostImage struct {
	Args []string
	Env  []string
	Cwd  string
}

func imageRunConfig(ctx context.Context, image containerd.Image) (modelHostImage, error) {
	spec, err := image.Spec(ctx)
	if err != nil {
		return modelHostImage{}, fmt.Errorf("reading the model host image config: %w", err)
	}
	args := append(append([]string{}, spec.Config.Entrypoint...), spec.Config.Cmd...)
	if len(args) == 0 {
		return modelHostImage{}, errors.New("the model host image has no entrypoint or command")
	}
	cwd := spec.Config.WorkingDir
	if cwd == "" {
		cwd = "/"
	}
	return modelHostImage{Args: args, Env: spec.Config.Env, Cwd: cwd}, nil
}

// modelHostSpec is the full spec: the locked-down base, the engine's
// accelerator runtime, then the grants and cgroup scope.
func (c *Client) modelHostSpec(h models.HostSpec, img modelHostImage, socketDir string) (*localoci.Spec, error) {
	spec, err := modelHostBaseSpec(h, img)
	if err != nil {
		return nil, err
	}
	switch h.Engine {
	case models.EngineTensorRT:
		if err := c.applyNvidiaCDI(spec); err != nil {
			return nil, fmt.Errorf("adding the NVIDIA runtime: %w", err)
		}
	case models.EngineQNN:
		c.applyQualcommNPURuntime(spec)
	}
	if err := finishModelHostSpec(spec, h, socketDir); err != nil {
		return nil, err
	}
	return spec, nil
}

// modelHostBaseSpec runs a model host as an unprivileged user with a
// read-only root, no capabilities, and a network namespace of its own that no
// CNI configures, so it has no network: the agent fetches everything the host
// needs. The host sees exactly one camera node, not the whole /dev that the
// camera entitlement grants.
func modelHostBaseSpec(h models.HostSpec, img modelHostImage) (*localoci.Spec, error) {
	spec := localoci.DefaultSpec("rootfs", img.Args)
	spec.Process.Cwd = img.Cwd
	spec.Process.User = localoci.User{UID: models.HostUID, GID: models.HostGID, AdditionalGids: []uint32{modelHostVideoGID}}
	spec.Process.Capabilities = &localoci.LinuxCapabilities{}
	spec.Root.Readonly = true
	spec.Process.Env = mergeEnv(spec.Process.Env, img.Env, h.Env())
	ro := []string{"bind", "ro", "nosuid", "nodev", "noexec"}
	spec.Mounts = append(spec.Mounts,
		localoci.Mount{Destination: "/tmp", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=1777", "size=256m"}},
		localoci.Mount{Destination: models.HostModelFile, Type: "bind", Source: h.ModelFile, Options: ro},
		localoci.Mount{Destination: models.HostLabelsFile, Type: "bind", Source: h.LabelsFile, Options: ro},
	)
	if h.EngineCache != "" {
		spec.Mounts = append(spec.Mounts, localoci.Mount{Destination: models.HostEngineCacheDir, Type: "bind",
			Source: h.EngineCache, Options: []string{"bind", "rw", "nosuid", "nodev", "noexec"}})
	}
	major, minor, err := resolveModelCamera(h.CameraNode, "c", false)
	if err != nil {
		return nil, fmt.Errorf("camera node %s: %w", h.CameraNode, err)
	}
	spec.Mounts = append(spec.Mounts, localoci.Mount{Destination: h.CameraNode, Source: h.CameraNode, Type: "bind",
		Options: []string{"bind", "rw", "nosuid", "noexec"}})
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices,
		localoci.LinuxDeviceCgroup{Allow: true, Type: "c", Major: &major, Minor: &minor, Access: "rw"})
	localoci.RecordPinnedDevice(spec, h.CameraNode, "c", major, minor)
	return spec, nil
}

// mergeEnv combines environments. A later entry replaces an earlier one with
// the same key: a process reads the first match, so a plain append would let
// the default PATH shadow the image's, and the image could shadow the host
// contract.
func mergeEnv(lists ...[]string) []string {
	index := map[string]int{}
	var out []string
	for _, list := range lists {
		for _, kv := range list {
			key, _, _ := strings.Cut(kv, "=")
			if i, ok := index[key]; ok {
				out[i] = kv
				continue
			}
			index[key] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

// finishModelHostSpec grants the data socket and the engine's accelerator
// through the same entitlement code apps use, then sets the cgroup scope that
// the data socket's peer check attributes to h.AppID.
func finishModelHostSpec(spec *localoci.Spec, h models.HostSpec, socketDir string) error {
	ents := []appconfig.Entitlement{{Type: appconfig.EntitlementEpisodeWrite}}
	switch h.Engine {
	case models.EngineTensorRT:
		ents = append(ents, appconfig.Entitlement{Type: appconfig.EntitlementGPU})
	case models.EngineQNN:
		ents = append(ents, appconfig.Entitlement{Type: appconfig.EntitlementNPU})
	}
	cfg := &appconfig.AppConfig{AppID: h.AppID, Entitlements: ents}
	if err := localoci.ApplyEntitlements(spec, cfg, localoci.ApplyOptions{DataSocketDir: socketDir}); err != nil {
		return fmt.Errorf("granting model host access: %w", err)
	}
	if spec.Linux.CgroupsPath != "" {
		return fmt.Errorf("security: CgroupsPath was set before assignment (%q)", spec.Linux.CgroupsPath)
	}
	spec.Linux.CgroupsPath = fmt.Sprintf("system.slice:%s:%s", sharedenv.SystemdServiceName(), h.AppID)
	localoci.DedupeDevices(spec)
	return nil
}
