package containerd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	cgroupv1 "github.com/containerd/cgroups/v3/cgroup1/stats"
	cgroupv2 "github.com/containerd/cgroups/v3/cgroup2/stats"
	tasks "github.com/containerd/containerd/api/services/tasks/v1"
	"github.com/containerd/containerd/api/types"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/internal/agent/cdi"
	"github.com/wendylabsinc/wendy/internal/agent/dbusproxy"
	localoci "github.com/wendylabsinc/wendy/internal/agent/oci"
	"github.com/wendylabsinc/wendy/internal/agent/services"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// Compile-time check that *Client satisfies services.ContainerdClient.
var _ services.ContainerdClient = (*Client)(nil)

// DefaultAddress is the default containerd socket path on Linux.
const DefaultAddress = "/run/containerd/containerd.sock"

// Client wraps the containerd SDK client and implements services.ContainerdClient.
type Client struct {
	client       *containerd.Client
	logger       *zap.Logger
	namespace    string
	mu           sync.Mutex
	proxyManager *dbusproxy.Manager // nil if xdg-dbus-proxy is not available
}

// NewClient creates a new containerd SDK client connected to the given Unix
// socket address. If address is empty, DefaultAddress is used.
// proxyMgr may be nil if xdg-dbus-proxy is not available.
func NewClient(logger *zap.Logger, address string, proxyMgr *dbusproxy.Manager) (*Client, error) {
	if address == "" {
		address = DefaultAddress
	}

	c, err := containerd.New(address)
	if err != nil {
		return nil, fmt.Errorf("connecting to containerd at %s: %w", address, err)
	}

	return &Client{
		client:       c,
		logger:       logger,
		namespace:    "default",
		proxyManager: proxyMgr,
	}, nil
}

// Close releases the underlying containerd client connection and stops all
// D-Bus proxy processes.
func (c *Client) Close() error {
	if c.proxyManager != nil {
		c.proxyManager.StopAll()
	}
	return c.client.Close()
}

// withNamespace returns a context annotated with the client's containerd namespace.
func (c *Client) withNamespace(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, c.namespace)
}

// ListLayers walks the content store and returns metadata for all layer blobs.
func (c *Client) ListLayers(ctx context.Context) ([]*agentpb.LayerHeader, error) {
	ctx = c.withNamespace(ctx)
	cs := c.client.ContentStore()

	var layers []*agentpb.LayerHeader
	err := cs.Walk(ctx, func(info content.Info) error {
		// Include blobs that are tagged as wendy layers or have a layer media type.
		if info.Labels[labelKeyWendyLayer] == "true" || isLayerDigest(info) {
			layers = append(layers, &agentpb.LayerHeader{
				Digest: info.Digest.String(),
				Size:   info.Size,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking content store: %w", err)
	}

	return layers, nil
}

// isLayerDigest checks if a content info entry looks like a layer by inspecting
// its labels for known layer media type indicators.
func isLayerDigest(info content.Info) bool {
	for k, v := range info.Labels {
		if strings.HasPrefix(k, "containerd.io/distribution.source") {
			_ = v
			continue
		}
		// Labels set by image handlers for layer children include media type info.
		if strings.Contains(v, "diff.tar") || strings.Contains(v, "layer") {
			return true
		}
	}
	return false
}

// WriteLayer writes a layer blob to the containerd content store. The digest
// parameter should be the expected content digest (e.g. "sha256:abc123...").
// Data is read from the provided io.Reader, which allows streaming without
// buffering the entire layer in memory. If size is 0, the descriptor size is
// left unset and determined by the content store from the reader.
func (c *Client) WriteLayer(ctx context.Context, dgst string, reader io.Reader, size int64) error {
	ctx = c.withNamespace(ctx)
	cs := c.client.ContentStore()

	expected, err := digest.Parse(dgst)
	if err != nil {
		return fmt.Errorf("parsing digest %q: %w", dgst, err)
	}

	labels := map[string]string{
		labelKeyGCRoot:     gcTimestamp(),
		labelKeyWendyLayer: "true",
	}

	err = content.WriteBlob(ctx, cs, dgst, reader, ocispec.Descriptor{
		Digest: expected,
		Size:   size,
	}, content.WithLabels(labels))
	if err != nil {
		// If the blob already exists, that is fine.
		if errdefs.IsAlreadyExists(err) {
			c.logger.Debug("Layer already exists in content store",
				zap.String("digest", dgst),
			)
			return nil
		}
		return fmt.Errorf("writing layer %s: %w", dgst, err)
	}

	c.logger.Info("Wrote layer to content store",
		zap.String("digest", dgst),
		zap.Int64("size", size),
	)
	return nil
}

// layerMediaType returns the OCI media type for a layer given its compression.
// The compression field takes precedence; when it is COMPRESSION_GZIP (the zero
// default), the legacy gzip bool determines the type for backward compatibility.
func layerMediaType(compression agentpb.RunContainerLayerHeader_CompressionType, gzip bool) string {
	switch compression {
	case agentpb.RunContainerLayerHeader_COMPRESSION_ZSTD:
		return ocispec.MediaTypeImageLayerZstd
	case agentpb.RunContainerLayerHeader_COMPRESSION_NONE:
		return ocispec.MediaTypeImageLayer
	default: // COMPRESSION_GZIP (0) or unrecognised
		if gzip {
			return ocispec.MediaTypeImageLayerGzip
		}
		return ocispec.MediaTypeImageLayer
	}
}

// AssembleImage creates a containerd image from layers already present in the
// content store. It builds an OCI manifest and config, writes them to the content
// store, and registers the image. If the image already exists it is updated.
func (c *Client) AssembleImage(ctx context.Context, imageName string, layers []*agentpb.RunContainerLayerHeader) error {
	ctx = c.withNamespace(ctx)
	cs := c.client.ContentStore()
	is := c.client.ImageService()

	// Build OCI layer descriptors and diff IDs.
	var layerDescs []ocispec.Descriptor
	var diffIDs []digest.Digest
	for _, l := range layers {
		mediaType := layerMediaType(l.GetCompression(), l.GetGzip())

		dgst, err := digest.Parse(l.GetDigest())
		if err != nil {
			return fmt.Errorf("parsing layer digest %q: %w", l.GetDigest(), err)
		}

		layerDescs = append(layerDescs, ocispec.Descriptor{
			MediaType: mediaType,
			Digest:    dgst,
			Size:      l.GetSize(),
		})

		diffID := l.GetDiffId()
		if diffID == "" {
			diffID = l.GetDigest()
		}
		did, err := digest.Parse(diffID)
		if err != nil {
			return fmt.Errorf("parsing diff ID %q: %w", diffID, err)
		}
		diffIDs = append(diffIDs, did)
	}

	// Build OCI image config.
	imgConfig := ocispec.Image{
		Platform: ocispec.Platform{
			Architecture: "arm64",
			OS:           "linux",
		},
		RootFS: ocispec.RootFS{
			Type:    "layers",
			DiffIDs: diffIDs,
		},
	}
	configData, err := json.Marshal(imgConfig)
	if err != nil {
		return fmt.Errorf("marshaling image config: %w", err)
	}
	configDigest := digest.FromBytes(configData)

	// Write config to content store.
	configDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageConfig,
		Digest:    configDigest,
		Size:      int64(len(configData)),
	}
	if err := content.WriteBlob(ctx, cs, configDigest.String(), bytes.NewReader(configData), configDesc); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return fmt.Errorf("writing config blob: %w", err)
		}
	}

	// Build OCI manifest.
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    configDesc,
		Layers:    layerDescs,
	}
	manifest.SchemaVersion = 2
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return fmt.Errorf("marshaling manifest: %w", err)
	}
	manifestDigest := digest.FromBytes(manifestData)

	// Write manifest to content store.
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    manifestDigest,
		Size:      int64(len(manifestData)),
	}
	if err := content.WriteBlob(ctx, cs, manifestDigest.String(), bytes.NewReader(manifestData), manifestDesc); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return fmt.Errorf("writing manifest blob: %w", err)
		}
	}

	// Create or update the image in the image store.
	_, err = is.Create(ctx, images.Image{
		Name:   imageName,
		Target: manifestDesc,
	})
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			_, err = is.Update(ctx, images.Image{
				Name:   imageName,
				Target: manifestDesc,
			})
			if err != nil {
				return fmt.Errorf("updating image %q: %w", imageName, err)
			}
		} else {
			return fmt.Errorf("creating image %q: %w", imageName, err)
		}
	}

	c.logger.Info("Assembled image",
		zap.String("name", imageName),
		zap.Int("layers", len(layers)),
		zap.String("manifest_digest", manifestDigest.String()),
	)
	return nil
}

// wrapWithDebugpy modifies the command args to run through debugpy for remote debugging.
// It injects "-m debugpy --listen 0.0.0.0:5678" after the Python binary.
func wrapWithDebugpy(args []string) []string {
	debugpyArgs := []string{"-m", "debugpy", "--listen", "0.0.0.0:5678"}

	if len(args) > 0 {
		base := args[0]
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		if base == "python" || base == "python3" || strings.HasPrefix(base, "python3.") {
			// python3 app.py -> python3 -m debugpy --listen 0.0.0.0:5678 app.py
			result := make([]string, 0, len(args)+len(debugpyArgs))
			result = append(result, args[0])
			result = append(result, debugpyArgs...)
			result = append(result, args[1:]...)
			return result
		}
	}

	// No python binary found; prepend python3 -m debugpy.
	result := make([]string, 0, len(args)+len(debugpyArgs)+1)
	result = append(result, "python3")
	result = append(result, debugpyArgs...)
	result = append(result, args...)
	return result
}

// CreateContainer creates (or replaces) a container in containerd for the given
// app. It builds an OCI runtime specification from the app config and request
// parameters, unpacks the image, and registers the container.
func (c *Client) CreateContainer(ctx context.Context, req *agentpb.CreateContainerRequest, appCfg *appconfig.AppConfig) error {
	return c.CreateContainerWithProgress(ctx, req, appCfg, nil)
}

func toCreateContainerProgress(progress UnpackProgress) *agentpb.CreateContainerProgress {
	switch progress.Phase {
	case "start":
		return &agentpb.CreateContainerProgress{
			Phase:       agentpb.CreateContainerProgress_UNPACKING,
			TotalLayers: int32(progress.TotalLayers),
		}
	case "layer":
		return &agentpb.CreateContainerProgress{
			Phase:          agentpb.CreateContainerProgress_APPLYING_LAYER,
			LayerIndex:     int32(progress.LayerIndex),
			TotalLayers:    int32(progress.TotalLayers),
			LayerSize:      progress.LayerSize,
			ReusedSnapshot: progress.Reused,
		}
	default:
		return nil
	}
}

func (c *Client) CreateContainerWithProgress(ctx context.Context, req *agentpb.CreateContainerRequest, appCfg *appconfig.AppConfig, onProgress services.ProgressFunc) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx = c.withNamespace(ctx)
	appName := req.GetAppName()
	// Canonicalise the image reference so older CLIs sending Docker short
	// names like "python:3.11-slim" still resolve correctly under containerd's
	// strict parser, which would otherwise read "3.11-slim" as a port.
	imageName := normalizeImageName(req.GetImageName())

	report := func(p *agentpb.CreateContainerProgress) {
		if onProgress != nil {
			onProgress(p)
		}
	}

	c.logger.Info("Creating container",
		zap.String("app_name", appName),
		zap.String("image", imageName),
	)

	// Determine version from the app config or default.
	version := appCfg.Version
	if version == "" {
		version = "latest"
	}

	// Delete any pre-existing container with the same name.
	if existing, err := c.client.LoadContainer(ctx, appName); err == nil {
		c.logger.Info("Removing existing container", zap.String("app_name", appName))
		// Try to stop/kill the task first.
		if task, taskErr := existing.Task(ctx, nil); taskErr == nil {
			_ = task.Kill(ctx, syscall.SIGKILL)
			_, _ = task.Delete(ctx, containerd.WithProcessKill)
		} else {
			// Task may be orphaned (shim crashed). Force-delete via the task
			// service directly so the runtime clears the old task ID.
			c.forceDeleteTask(ctx, appName)
		}
		_ = existing.Delete(ctx, containerd.WithSnapshotCleanup)
		// Stop old D-Bus proxy if any.
		if c.proxyManager != nil {
			_ = c.proxyManager.Stop(appName)
		}
	}

	// Start D-Bus proxy if bluetooth entitlement is present.
	var dbusProxyStarted bool
	if c.proxyManager != nil && hasBluetooth(appCfg) {
		if _, err := c.proxyManager.Start(ctx, appName); err != nil {
			return fmt.Errorf("starting D-Bus proxy for %q: %w", appName, err)
		}
		dbusProxyStarted = true
		defer func() {
			if dbusProxyStarted {
				_ = c.proxyManager.Stop(appName)
			}
		}()
	}

	// Try the local image store first. The device-local registry shares
	// containerd's content store, so anything just pushed to it is already
	// available via GetImage — pulling would just round-trip bytes over
	// loopback. Fall back to a pull only on miss; use PlainHTTP for the
	// local-registry case.
	var image containerd.Image
	var err error
	report(&agentpb.CreateContainerProgress{Phase: agentpb.CreateContainerProgress_UNPACKING})
	image, err = c.client.GetImage(ctx, imageName)
	if err != nil {
		c.logger.Info("Image not in local store, attempting pull from registry",
			zap.String("image", imageName),
		)
		pullOpts := []containerd.RemoteOpt{containerd.WithPullUnpack}
		if isLocalRegistryImage(imageName) {
			pullOpts = append(pullOpts,
				containerd.WithResolver(docker.NewResolver(docker.ResolverOptions{PlainHTTP: true})),
			)
		}
		image, err = c.client.Pull(ctx, imageName, pullOpts...)
		if err != nil {
			return fmt.Errorf("getting/pulling image %q: %w", imageName, err)
		}
	}

	// Unpack the image into the snapshotter if not already done.
	unpacked, err := image.IsUnpacked(ctx, "")
	if err != nil {
		c.logger.Warn("Failed to check if image is unpacked", zap.Error(err))
	}
	if !unpacked {
		c.logger.Info("Unpacking image", zap.String("image", imageName))
		if err := c.UnpackImage(ctx, image, func(progress UnpackProgress) {
			if mapped := toCreateContainerProgress(progress); mapped != nil {
				report(mapped)
			}
		}); err != nil {
			return fmt.Errorf("unpacking image %q: %w", imageName, err)
		}
	}

	// Read the image's OCI config (CMD, ENTRYPOINT, ENV, WorkingDir).
	imageSpec, specErr := image.Spec(ctx)
	if specErr != nil {
		c.logger.Warn("Failed to read image spec, using defaults", zap.Error(specErr))
	}

	// Build the container command: explicit request > image config > /bin/sh.
	var args []string
	cmd := req.GetCmd()
	if cmd != "" {
		args = strings.Fields(cmd)
	}
	if len(req.GetUserArgs()) > 0 {
		args = append(args, req.GetUserArgs()...)
	}
	if len(args) == 0 && specErr == nil {
		args = append(imageSpec.Config.Entrypoint, imageSpec.Config.Cmd...)
	}
	if len(args) == 0 {
		args = []string{"/bin/sh"}
	}

	// Wrap Python commands with debugpy for remote debugging (only in debug mode).
	if appCfg.Debug && appCfg.Language == "python" {
		args = wrapWithDebugpy(args)
	}

	// Build the working directory: explicit request > image config > /.
	workingDir := req.GetWorkingDir()
	if workingDir == "" && specErr == nil && imageSpec.Config.WorkingDir != "" {
		workingDir = imageSpec.Config.WorkingDir
	}
	if workingDir == "" {
		workingDir = "/"
	}

	// Build environment variables: image env first, then our overrides.
	env := buildContainerBaseEnv()
	if specErr == nil {
		env = append(imageSpec.Config.Env, env...)
	}

	// Build OCI spec using local oci package, then apply entitlements.
	spec := localoci.DefaultSpec("rootfs", args)
	spec.Process.Cwd = workingDir
	spec.Process.Env = env
	if spec.Linux == nil {
		spec.Linux = &localoci.Linux{}
	}
	spec.Linux.CgroupsPath = fmt.Sprintf("system.slice:wendy-agent:%s", appName)

	// Apply the NVIDIA CDI spec before entitlements so that entitlements can
	// override CDI-injected env vars (e.g. NVIDIA_VISIBLE_DEVICES=void → =all).
	if appCfg.HasEntitlement(appconfig.EntitlementGPU) {
		c.applyCDIGPU(spec)
	}

	opts := localoci.ApplyOptions{
		DBusProxyAvailable: c.proxyManager != nil,
	}
	if err := localoci.ApplyEntitlements(spec, appCfg, opts); err != nil {
		return fmt.Errorf("applying entitlements: %w", err)
	}

	report(&agentpb.CreateContainerProgress{Phase: agentpb.CreateContainerProgress_CREATING_CONTAINER})

	// Build labels for the container.
	var mcpPort uint32
	for _, e := range appCfg.Entitlements {
		if e.Type == appconfig.EntitlementMCP {
			mcpPort = uint32(e.Port)
			break
		}
	}
	labels := wendyLabels(appName, version, req.GetRestartPolicy(), mcpPort)

	// Serialize our custom OCI spec to JSON for WithSpecFromBytes.
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshaling OCI spec: %w", err)
	}

	// Create the container with a new snapshot from the image.
	snapshotKey := fmt.Sprintf("wendy-%s", appName)
	_, err = c.client.NewContainer(ctx, appName,
		containerd.WithImage(image),
		containerd.WithNewSnapshot(snapshotKey, image),
		containerd.WithContainerLabels(labels),
		containerd.WithNewSpec(
			oci.WithSpecFromBytes(specJSON),
		),
	)
	if err != nil {
		return fmt.Errorf("creating container %q: %w", appName, err)
	}

	// Container created successfully; keep the D-Bus proxy running.
	dbusProxyStarted = false

	report(&agentpb.CreateContainerProgress{Phase: agentpb.CreateContainerProgress_COMPLETE})

	c.logger.Info("Container created",
		zap.String("app_name", appName),
		zap.String("image", imageName),
		zap.String("version", version),
	)

	return nil
}

// applyCDIGPU loads the NVIDIA CDI spec (generated by nvidia-ctk at boot)
// and applies GPU devices, library mounts, and environment variables to the
// OCI spec. This handles platform-specific paths (Orin Nano vs Thor, etc.).
func (c *Client) applyCDIGPU(spec *localoci.Spec) {
	mgr := cdi.NewManager()
	cdiSpec, err := mgr.LoadNVIDIACDISpec()
	if err != nil {
		c.logger.Warn("No NVIDIA CDI spec found, GPU library mounts may be incomplete", zap.Error(err))
		return
	}

	// nvidia-ctk in CSV mode generates a device named "all".
	// Try that first, then fall back to the first device in the spec.
	if err := cdi.ApplyCDIDevice(spec, cdiSpec, "all"); err == nil {
		c.logger.Info("Applied NVIDIA CDI spec for GPU access")
		return
	}
	if len(cdiSpec.Devices) > 0 {
		if err := cdi.ApplyCDIDevice(spec, cdiSpec, cdiSpec.Devices[0].Name); err == nil {
			c.logger.Info("Applied NVIDIA CDI device", zap.String("device", cdiSpec.Devices[0].Name))
			return
		}
	}
	c.logger.Warn("CDI spec found but no devices could be applied")
}

// StartContainer starts the task for a named container and returns a channel
// that streams stdout/stderr output. When the container exits, a final
// ContainerOutput with Done=true is sent and the channel is closed.
func (c *Client) StartContainer(ctx context.Context, appName, postStartAgentCommand string, restartPolicy *agentpb.RestartPolicy) (<-chan services.ContainerOutput, error) {
	ctx = c.withNamespace(ctx)

	container, err := c.client.LoadContainer(ctx, appName)
	if err != nil {
		return nil, fmt.Errorf("loading container %q: %w", appName, err)
	}

	if restartPolicy != nil {
		if err := c.applyRestartPolicyLabel(ctx, container, restartPolicy); err != nil {
			return nil, fmt.Errorf("updating restart policy for %q: %w", appName, err)
		}
	}

	// Clean up any stale task from a previous run.
	c.deleteStaleTask(ctx, container, appName)

	// Create pipes for stdout/stderr capture.
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	// Create a new task with pipe-based stdio for programmatic capture.
	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStreams(nil, stdoutW, stderrW)))
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			// Orphaned task: exists in the containerd runtime but container.Task()
			// can't load it. Force-delete via the task service, then recreate the
			// container and retry.
			c.logger.Warn("Orphaned task detected, force-deleting and recreating container", zap.String("app_name", appName))
			c.forceDeleteTask(ctx, appName)
			if rerr := c.recreateContainer(ctx, container, appName); rerr != nil {
				c.logger.Error("Failed to recreate container", zap.Error(rerr))
			} else {
				container, err = c.client.LoadContainer(ctx, appName)
				if err == nil {
					task, err = container.NewTask(ctx, cio.NewCreator(cio.WithStreams(nil, stdoutW, stderrW)))
				}
			}
		}
		if err != nil {
			stdoutR.Close()
			stdoutW.Close()
			stderrR.Close()
			stderrW.Close()
			return nil, fmt.Errorf("creating task for %q: %w", appName, err)
		}
	}

	// Set up the wait channel before starting.
	exitStatusCh, err := task.Wait(ctx)
	if err != nil {
		_, _ = task.Delete(ctx)
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return nil, fmt.Errorf("waiting on task for %q: %w", appName, err)
	}

	// Start the task.
	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(ctx)
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return nil, fmt.Errorf("starting task for %q: %w", appName, err)
	}

	c.logger.Info("Container started", zap.String("app_name", appName))
	c.startPostStartAgentHook(postStartAgentCommand, appName)

	// Stream output from the pipes.
	outputCh := make(chan services.ContainerOutput, 64)
	go c.streamOutput(ctx, task, exitStatusCh, outputCh, appName, stdoutR, stderrR, stdoutW, stderrW)

	return outputCh, nil
}

// StartContainerWithStdin is like StartContainer but attaches the provided
// stdin reader to the container's standard input.
func (c *Client) StartContainerWithStdin(ctx context.Context, appName string, stdin io.Reader, postStartAgentCommand string, restartPolicy *agentpb.RestartPolicy) (<-chan services.ContainerOutput, error) {
	ctx = c.withNamespace(ctx)

	container, err := c.client.LoadContainer(ctx, appName)
	if err != nil {
		return nil, fmt.Errorf("loading container %q: %w", appName, err)
	}

	if restartPolicy != nil {
		if err := c.applyRestartPolicyLabel(ctx, container, restartPolicy); err != nil {
			return nil, fmt.Errorf("updating restart policy for %q: %w", appName, err)
		}
	}

	c.deleteStaleTask(ctx, container, appName)

	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	task, err := container.NewTask(ctx, cio.NewCreator(cio.WithStreams(stdin, stdoutW, stderrW)))
	if err != nil {
		if errdefs.IsAlreadyExists(err) {
			c.logger.Warn("Orphaned task detected, force-deleting and recreating container", zap.String("app_name", appName))
			c.forceDeleteTask(ctx, appName)
			if rerr := c.recreateContainer(ctx, container, appName); rerr != nil {
				c.logger.Error("Failed to recreate container", zap.Error(rerr))
			} else {
				container, err = c.client.LoadContainer(ctx, appName)
				if err == nil {
					task, err = container.NewTask(ctx, cio.NewCreator(cio.WithStreams(stdin, stdoutW, stderrW)))
				}
			}
		}
		if err != nil {
			stdoutR.Close()
			stdoutW.Close()
			stderrR.Close()
			stderrW.Close()
			return nil, fmt.Errorf("creating task for %q: %w", appName, err)
		}
	}

	exitStatusCh, err := task.Wait(ctx)
	if err != nil {
		_, _ = task.Delete(ctx)
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return nil, fmt.Errorf("waiting on task for %q: %w", appName, err)
	}

	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(ctx)
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return nil, fmt.Errorf("starting task for %q: %w", appName, err)
	}

	c.logger.Info("Container started with stdin", zap.String("app_name", appName))
	c.startPostStartAgentHook(postStartAgentCommand, appName)

	outputCh := make(chan services.ContainerOutput, 64)
	go c.streamOutput(ctx, task, exitStatusCh, outputCh, appName, stdoutR, stderrR, stdoutW, stderrW)

	return outputCh, nil
}

func shellCommand() (string, string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", "/C"
	}
	return "sh", "-c"
}

// deviceHostnameWithSuffix returns the device's mDNS hostname with the ".local"
// suffix (e.g. "wendyos-mighty-kayak.local"), or "" if the OS hostname is
// unavailable. Indirected through a var so tests can override it.
var deviceHostnameWithSuffix = func() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return ""
	}
	return h + ".local"
}

// buildContainerBaseEnv builds the wendy-injected env vars layered on top of
// the image's own env. WENDY_HOSTNAME is the device's mDNS hostname
// (omitted when unresolvable).
func buildContainerBaseEnv() []string {
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"TERM=xterm",
	}
	if h := deviceHostnameWithSuffix(); h != "" {
		env = append(env, "WENDY_HOSTNAME="+h)
	}
	return env
}

func expandAgentHook(command, appName string) string {
	return os.Expand(command, func(key string) string {
		switch key {
		case "WENDY_HOSTNAME":
			return "localhost"
		case "WENDY_APP_ID":
			return appName
		default:
			return os.Getenv(key)
		}
	})
}

var startPostStartHookCommand = func(shell, flag, command string) (func() error, error) {
	cmd := exec.Command(shell, flag, command)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Wait, nil
}

func (c *Client) startPostStartAgentHook(command, appName string) bool {
	if command == "" {
		return false
	}

	expanded := expandAgentHook(command, appName)
	shell, flag := shellCommand()
	wait, err := startPostStartHookCommand(shell, flag, expanded)
	if err != nil {
		c.logger.Warn("Failed to start postStart agent hook",
			zap.String("app_name", appName),
			zap.Error(err),
		)
		return false
	}
	go func() {
		if err := wait(); err != nil {
			c.logger.Warn("postStart agent hook exited with error",
				zap.String("app_name", appName),
				zap.Error(err),
			)
		}
	}()
	c.logger.Info("Started postStart agent hook",
		zap.String("app_name", appName),
	)
	return true
}

// deleteStaleTask attempts to load and force-delete any existing task for the
// container. It handles both the normal case (task loadable) and the edge case
// where the task exists in containerd but container.Task() can't load it.
func (c *Client) deleteStaleTask(ctx context.Context, container containerd.Container, appName string) {
	existingTask, taskErr := container.Task(ctx, nil)
	if taskErr != nil {
		return // No task to clean up.
	}
	_ = existingTask.Kill(ctx, syscall.SIGKILL)
	if waitCh, waitErr := existingTask.Wait(ctx); waitErr == nil {
		select {
		case <-waitCh:
		case <-time.After(5 * time.Second):
			c.logger.Warn("Timed out waiting for stale task to exit", zap.String("app_name", appName))
		}
	}
	_, _ = existingTask.Delete(ctx, containerd.WithProcessKill)
}

// forceDeleteTask uses the low-level containerd task service to delete a task
// by container ID. This handles orphaned tasks where container.Task() fails
// because the shim process is gone but task metadata remains in the runtime.
func (c *Client) forceDeleteTask(ctx context.Context, containerID string) {
	_, err := c.client.TaskService().Delete(ctx, &tasks.DeleteTaskRequest{
		ContainerID: containerID,
	})
	if err != nil {
		c.logger.Debug("Force task delete attempt",
			zap.String("container_id", containerID),
			zap.Error(err),
		)
	} else {
		c.logger.Info("Force-deleted orphaned task",
			zap.String("container_id", containerID),
		)
	}
}

// recreateContainer deletes a container (which cascades to any orphaned task)
// and recreates it with the same image, spec, and labels. This clears orphaned
// task metadata that blocks NewTask.
func (c *Client) recreateContainer(ctx context.Context, ctr containerd.Container, appName string) error {
	info, err := ctr.Info(ctx)
	if err != nil {
		return fmt.Errorf("getting container info: %w", err)
	}

	image, err := ctr.Image(ctx)
	if err != nil {
		return fmt.Errorf("getting container image: %w", err)
	}

	spec, err := ctr.Spec(ctx)
	if err != nil {
		return fmt.Errorf("getting container spec: %w", err)
	}

	specJSON, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshaling spec: %w", err)
	}

	// Delete the container (cascades to orphaned task).
	if err := ctr.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		return fmt.Errorf("deleting container: %w", err)
	}

	// Recreate with the same configuration.
	snapshotKey := fmt.Sprintf("wendy-%s", appName)
	_, err = c.client.NewContainer(ctx, appName,
		containerd.WithImage(image),
		containerd.WithNewSnapshot(snapshotKey, image),
		containerd.WithContainerLabels(info.Labels),
		containerd.WithNewSpec(
			oci.WithSpecFromBytes(specJSON),
		),
	)
	if err != nil {
		return fmt.Errorf("recreating container: %w", err)
	}

	c.logger.Info("Recreated container to clear orphaned task", zap.String("app_name", appName))
	return nil
}

// applyRestartPolicyLabel updates the restart policy label on an existing container.
func (c *Client) applyRestartPolicyLabel(ctx context.Context, container containerd.Container, restartPolicy *agentpb.RestartPolicy) error {
	return container.Update(ctx, func(ctx context.Context, client *containerd.Client, ctr *containers.Container) error {
		if ctr.Labels == nil {
			ctr.Labels = make(map[string]string)
		}
		policyStr := restartPolicyToLabel(restartPolicy)
		if policyStr != "" {
			ctr.Labels[labelKeyRestartPolicy] = policyStr
		} else {
			delete(ctr.Labels, labelKeyRestartPolicy)
		}
		return nil
	})
}

// streamOutput reads stdout/stderr from pipes and sends it to the output
// channel. It closes the channel when the task exits.
func (c *Client) streamOutput(
	ctx context.Context,
	task containerd.Task,
	exitStatusCh <-chan containerd.ExitStatus,
	outputCh chan<- services.ContainerOutput,
	appName string,
	stdoutR, stderrR *io.PipeReader,
	stdoutW, stderrW *io.PipeWriter,
) {
	defer close(outputCh)

	// Read stdout and stderr concurrently.
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		streamReader(stdoutR, outputCh, func(data []byte) services.ContainerOutput {
			return services.ContainerOutput{Stdout: data}
		})
	}()

	go func() {
		defer wg.Done()
		streamReader(stderrR, outputCh, func(data []byte) services.ContainerOutput {
			return services.ContainerOutput{Stderr: data}
		})
	}()

	// Wait for the task to exit.
	exitStatus := <-exitStatusCh
	code, _, err := exitStatus.Result()
	if err != nil {
		c.logger.Error("Task exited with error",
			zap.String("app_name", appName),
			zap.Error(err),
		)
	} else {
		c.logger.Info("Task exited",
			zap.String("app_name", appName),
			zap.Uint32("exit_code", code),
		)
	}

	// Close the write ends to unblock readers.
	stdoutW.Close()
	stderrW.Close()

	// Wait for readers to finish.
	wg.Wait()

	outputCh <- services.ContainerOutput{Done: true}
}

// StopContainer sends SIGTERM to the container's task, waits briefly, then
// sends SIGKILL if the task is still running, and finally deletes the task.
func (c *Client) StopContainer(ctx context.Context, appName string) error {
	ctx = c.withNamespace(ctx)

	container, err := c.client.LoadContainer(ctx, appName)
	if err != nil {
		return fmt.Errorf("loading container %q: %w", appName, err)
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil // No task running.
		}
		return fmt.Errorf("getting task for %q: %w", appName, err)
	}

	// Send SIGTERM first for graceful shutdown.
	if err := task.Kill(ctx, syscall.SIGTERM); err != nil {
		if !errdefs.IsNotFound(err) {
			c.logger.Warn("Failed to send SIGTERM",
				zap.String("app_name", appName),
				zap.Error(err),
			)
		}
	}

	// Wait up to 10 seconds for graceful exit.
	waitCh, err := task.Wait(ctx)
	if err != nil {
		c.logger.Warn("Failed to wait on task, sending SIGKILL",
			zap.String("app_name", appName),
			zap.Error(err),
		)
	} else {
		select {
		case <-waitCh:
			// Task exited gracefully.
			c.logger.Info("Container stopped gracefully", zap.String("app_name", appName))
		case <-time.After(10 * time.Second):
			// Force kill.
			c.logger.Warn("Container did not stop within 10s, sending SIGKILL",
				zap.String("app_name", appName),
			)
			if err := task.Kill(ctx, syscall.SIGKILL); err != nil && !errdefs.IsNotFound(err) {
				c.logger.Error("Failed to send SIGKILL",
					zap.String("app_name", appName),
					zap.Error(err),
				)
			}
			<-waitCh
		}
	}

	// Delete the task.
	_, err = task.Delete(ctx, containerd.WithProcessKill)
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("deleting task for %q: %w", appName, err)
	}

	// Stop D-Bus proxy if running.
	if c.proxyManager != nil {
		_ = c.proxyManager.Stop(appName)
	}

	c.logger.Info("Container stopped", zap.String("app_name", appName))
	return nil
}

// DeleteContainer stops the container task if running, deletes the container,
// cleans up the snapshot, and optionally deletes the image.
func (c *Client) DeleteContainer(ctx context.Context, appName string, deleteImage bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	ctx = c.withNamespace(ctx)

	container, err := c.client.LoadContainer(ctx, appName)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil // Already gone.
		}
		return fmt.Errorf("loading container %q: %w", appName, err)
	}

	// Stop the task if running.
	if task, taskErr := container.Task(ctx, nil); taskErr == nil {
		_ = task.Kill(ctx, syscall.SIGKILL)
		_, _ = task.Delete(ctx, containerd.WithProcessKill)
	}

	// Get the image name before deleting the container.
	var imgName string
	if deleteImage {
		if img, imgErr := container.Image(ctx); imgErr == nil {
			imgName = img.Name()
		}
	}

	// Delete the container and its snapshot.
	if err := container.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		return fmt.Errorf("deleting container %q: %w", appName, err)
	}

	// Stop D-Bus proxy if running.
	if c.proxyManager != nil {
		_ = c.proxyManager.Stop(appName)
	}

	c.logger.Info("Container deleted", zap.String("app_name", appName))

	// Optionally delete the image.
	if deleteImage && imgName != "" {
		imgService := c.client.ImageService()
		if err := imgService.Delete(ctx, imgName); err != nil && !errdefs.IsNotFound(err) {
			c.logger.Warn("Failed to delete image",
				zap.String("image", imgName),
				zap.Error(err),
			)
		} else {
			c.logger.Info("Image deleted", zap.String("image", imgName))
		}
	}

	return nil
}

// ListContainers lists all containers managed by Wendy (those with the
// sh.wendy/app.version label) and returns their status.
func (c *Client) ListContainers(ctx context.Context) ([]*agentpb.AppContainer, error) {
	ctx = c.withNamespace(ctx)

	containers, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppVersion))
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	var result []*agentpb.AppContainer
	for _, ctr := range containers {
		info, err := ctr.Info(ctx)
		if err != nil {
			c.logger.Warn("Failed to get container info",
				zap.String("id", ctr.ID()),
				zap.Error(err),
			)
			continue
		}

		appVersion := info.Labels[labelKeyAppVersion]
		runningState := agentpb.AppRunningState_STOPPED
		var failureCount uint32

		// Check if a task is running.
		task, err := ctr.Task(ctx, nil)
		if err == nil {
			status, statusErr := task.Status(ctx)
			if statusErr == nil && status.Status == containerd.Running {
				runningState = agentpb.AppRunningState_RUNNING
			}
		}

		// Parse failure count from restart policy label if present.
		if policyLabel, ok := info.Labels[labelKeyRestartPolicy]; ok {
			_, maxRetries := parseRestartPolicyLabel(policyLabel)
			_ = maxRetries
		}

		var mcpPort uint32
		if portStr, ok := info.Labels[labelKeyMCPPort]; ok && portStr != "" {
			if p, err := strconv.ParseUint(portStr, 10, 32); err == nil {
				mcpPort = uint32(p)
			}
		}

		result = append(result, &agentpb.AppContainer{
			AppName:      ctr.ID(),
			AppVersion:   appVersion,
			RunningState: runningState,
			FailureCount: failureCount,
			McpPort:      mcpPort,
		})
	}

	return result, nil
}

// GetContainerMCPPort returns the MCP server port for the named container,
// or 0 if the container has no mcp entitlement.
func (c *Client) GetContainerMCPPort(ctx context.Context, appName string) (uint32, error) {
	ctx = c.withNamespace(ctx)
	ctr, err := c.client.LoadContainer(ctx, appName)
	if err != nil {
		return 0, fmt.Errorf("loading container %q: %w", appName, err)
	}
	info, err := ctr.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("getting container info for %q: %w", appName, err)
	}
	portStr, ok := info.Labels[labelKeyMCPPort]
	if !ok || portStr == "" {
		return 0, nil
	}
	p, err := strconv.ParseUint(portStr, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parsing mcp port label for %q: %w", appName, err)
	}
	return uint32(p), nil
}

// GetContainerRestartPolicyLabel returns the raw restart policy label stored on
// the container (e.g. "unless-stopped", "on-failure:5", "no"). An empty string
// is returned when the container exists but has no restart policy label.
func (c *Client) GetContainerRestartPolicyLabel(ctx context.Context, appName string) (string, error) {
	ctx = c.withNamespace(ctx)
	ctr, err := c.client.LoadContainer(ctx, appName)
	if err != nil {
		return "", fmt.Errorf("loading container %q: %w", appName, err)
	}
	info, err := ctr.Info(ctx)
	if err != nil {
		return "", fmt.Errorf("getting container info for %q: %w", appName, err)
	}
	return info.Labels[labelKeyRestartPolicy], nil
}

// GetContainerStats collects memory and image-size stats for all Wendy-managed containers.
// Memory is read from cgroup metrics (only available for running tasks). Storage is the
// image size from the content store. Both values are 0 if unavailable.
func (c *Client) GetContainerStats(ctx context.Context) ([]*agentpb.ContainerStats, error) {
	ctx = c.withNamespace(ctx)

	containers, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppVersion))
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	var result []*agentpb.ContainerStats
	for _, ctr := range containers {
		appName := ctr.ID()
		stat := &agentpb.ContainerStats{AppName: appName}

		// Storage: image size from content store.
		if img, imgErr := ctr.Image(ctx); imgErr == nil {
			if sz, szErr := img.Size(ctx); szErr == nil {
				stat.StorageBytes = sz
			}
		}

		// Memory: cgroup metrics from running task.
		if task, taskErr := ctr.Task(ctx, nil); taskErr == nil {
			if metric, metErr := task.Metrics(ctx); metErr == nil {
				stat.MemoryBytes = extractMemoryBytes(metric)
			}
		}

		result = append(result, stat)
	}
	return result, nil
}

// GetContainerMetrics returns a point-in-time CPU and memory snapshot for a named container.
// Returns an error if the container or its task cannot be found.
func (c *Client) GetContainerMetrics(ctx context.Context, appName string) (services.ContainerMetrics, error) {
	ctx = c.withNamespace(ctx)
	container, err := c.client.LoadContainer(ctx, appName)
	if err != nil {
		return services.ContainerMetrics{}, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return services.ContainerMetrics{}, err
	}
	metric, err := task.Metrics(ctx)
	if err != nil {
		return services.ContainerMetrics{}, err
	}
	return extractContainerMetrics(metric), nil
}

// extractContainerMetrics decodes cgroup v1 or v2 task metrics into a ContainerMetrics snapshot.
func extractContainerMetrics(metric *types.Metric) services.ContainerMetrics {
	switch {
	case typeurl.Is(metric.Data, (*cgroupv1.Metrics)(nil)):
		m := &cgroupv1.Metrics{}
		if err := typeurl.UnmarshalTo(metric.Data, m); err != nil {
			return services.ContainerMetrics{}
		}
		var result services.ContainerMetrics
		if m.CPU != nil && m.CPU.Usage != nil {
			result.UserCPUNanos = int64(m.CPU.Usage.User)
			result.SysCPUNanos = int64(m.CPU.Usage.Kernel)
		}
		if m.Memory != nil && m.Memory.Usage != nil {
			result.MemBytes = int64(m.Memory.Usage.Usage)
		}
		return result
	case typeurl.Is(metric.Data, (*cgroupv2.Metrics)(nil)):
		m := &cgroupv2.Metrics{}
		if err := typeurl.UnmarshalTo(metric.Data, m); err != nil {
			return services.ContainerMetrics{}
		}
		var result services.ContainerMetrics
		if m.CPU != nil {
			result.UserCPUNanos = int64(m.CPU.UserUsec) * 1000
			result.SysCPUNanos = int64(m.CPU.SystemUsec) * 1000
		}
		if m.Memory != nil {
			result.MemBytes = int64(m.Memory.Usage)
		}
		return result
	}
	return services.ContainerMetrics{}
}

// extractMemoryBytes decodes cgroup v1 or v2 task metrics and returns memory usage in bytes.
func extractMemoryBytes(metric *types.Metric) int64 {
	return extractContainerMetrics(metric).MemBytes
}

// streamReader is a helper that continuously reads from a reader and sends
// chunks to the output channel with the specified builder function.
func streamReader(r io.Reader, ch chan<- services.ContainerOutput, buildOutput func([]byte) services.ContainerOutput) {
	buf := make([]byte, 32*1024)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := make([]byte, n)
			copy(data, buf[:n])
			ch <- buildOutput(data)
		}
		if err != nil {
			return
		}
	}
}

// hasBluetooth returns true if the app config includes a bluetooth entitlement.
func hasBluetooth(cfg *appconfig.AppConfig) bool {
	for _, ent := range cfg.Entitlements {
		if ent.Type == appconfig.EntitlementBluetooth {
			return true
		}
	}
	return false
}
