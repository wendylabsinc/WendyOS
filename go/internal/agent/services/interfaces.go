// Package services implements the gRPC service handlers for the wendy-agent.
package services

import (
	"context"
	"io"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// NetworkManager abstracts WiFi management operations (typically backed by nmcli).
type NetworkManager interface {
	ListWiFiNetworks(ctx context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error)
	ConnectToWiFi(ctx context.Context, req *agentpb.ConnectToWiFiRequest) error
	GetWiFiStatus(ctx context.Context) (connected bool, ssid string, err error)
	DisconnectWiFi(ctx context.Context) error
	ListKnownWiFiNetworks(ctx context.Context) ([]*agentpb.ListKnownWiFiNetworksResponse_KnownWiFiNetwork, error)
	SetWiFiNetworkPriority(ctx context.Context, ssid string, priority int32) error
	ReorderKnownWiFiNetworks(ctx context.Context, orderedSSIDs []string) error
	ForgetWiFiNetwork(ctx context.Context, ssid string) error
}

// HardwareDiscoverer discovers hardware capabilities by probing sysfs, /dev, /proc, etc.
type HardwareDiscoverer interface {
	Discover(ctx context.Context, categoryFilter string) ([]*agentpb.ListHardwareCapabilitiesResponse_HardwareCapability, error)
}

// BluetoothManager abstracts Bluetooth peripheral management.
type BluetoothManager interface {
	Scan(ctx context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error)
	// Connect reports whether the device is paired after the connection is
	// established (pairing can fail while the connection still succeeds).
	Connect(ctx context.Context, address string, pair, trust bool) (paired bool, err error)
	Disconnect(ctx context.Context, address string) error
	Forget(ctx context.Context, address string) error
}

// ProgressFunc is called by CreateContainer to report progress during
// image pull, unpack, and container creation. The caller may be nil.
type ProgressFunc func(progress *agentpb.CreateContainerProgress)

// ContainerdClient abstracts interactions with the containerd runtime.
type ContainerdClient interface {
	ListLayers(ctx context.Context) ([]*agentpb.LayerHeader, error)
	WriteLayer(ctx context.Context, digest string, reader io.Reader, size int64) error
	AssembleImage(ctx context.Context, imageName string, layers []*agentpb.RunContainerLayerHeader, imageConfig []byte) error
	MissingChunks(ctx context.Context, hashes [][32]byte) ([][32]byte, error)
	// PresentLayers reports which uncompressed layer diff IDs the device already
	// has, mapping each to its blob size. Used by QueryLayers so the CLI can skip
	// chunking layers the device can reuse as-is.
	PresentLayers(ctx context.Context, diffIDs []string) (map[string]int64, error)
	StageChunk(ctx context.Context, h [32]byte, data []byte) error
	AssembleLayerFromChunks(ctx context.Context, diffID string, hashes [][32]byte) error
	CreateContainer(ctx context.Context, req *agentpb.CreateContainerRequest, appCfg *appconfig.AppConfig) error
	CreateContainerWithProgress(ctx context.Context, req *agentpb.CreateContainerRequest, appCfg *appconfig.AppConfig, onProgress ProgressFunc) error
	StartContainer(ctx context.Context, appName, postStartAgentCommand string, restartPolicy *agentpb.RestartPolicy) (<-chan ContainerOutput, error)
	StartContainerWithStdin(ctx context.Context, appName string, stdin io.Reader, postStartAgentCommand string, restartPolicy *agentpb.RestartPolicy) (<-chan ContainerOutput, error)
	StopContainer(ctx context.Context, appName string) error
	DeleteContainer(ctx context.Context, appName string, deleteImage bool) error
	// ContainerIDsForApp returns the containerd container IDs for all services
	// belonging to appID (one for single-container, one per service for
	// multi-service apps). Used by the service layer to mark every container in
	// the monitor before issuing a stop or delete.
	ContainerIDsForApp(ctx context.Context, appID string) ([]string, error)
	// ResolveAppContainerIDs returns the container IDs addressed by name: a bare
	// appID yields every service in the app, "{appID}_{serviceName}" yields that
	// one service, and a name matching neither returns errdefs.ErrNotFound.
	ResolveAppContainerIDs(ctx context.Context, name string) ([]string, error)
	ListContainers(ctx context.Context) ([]*agentpb.AppContainer, error)
	// AppDeclaredVolumes maps every deployed app (bare appID) to the persistent
	// volume names its containers declare via persist entitlement labels. This
	// is the source of truth for volume ownership (volumes are shared across
	// apps by name, so one name may appear under several apps). Apps deployed
	// before entitlement labels existed are absent — callers must treat that as
	// "ownership unknown" and fail safe rather than guess from name prefixes
	// (WDY-1807).
	AppDeclaredVolumes(ctx context.Context) (map[string][]string, error)
	// ListBootContainers returns the containers that should be (re)started at
	// agent boot: restart policy keeps them running (not "no") and they were not
	// explicitly stopped by the user. Used by the boot reconcile.
	ListBootContainers(ctx context.Context) ([]BootContainer, error)
	// SetStoppedByUser persists (or clears) the "user explicitly stopped this"
	// mark on a container so a deliberate stop survives a reboot.
	SetStoppedByUser(ctx context.Context, containerID string, stopped bool) error
	// MigrateStoppedByUserOnce back-fills the stopped-by-user mark for apps that
	// predate it (one-time, persistent-marker-guarded), so upgrading to
	// boot-reconcile doesn't resurrect apps the user had already stopped.
	MigrateStoppedByUserOnce(ctx context.Context) error
	GetContainerStats(ctx context.Context) ([]*agentpb.ContainerStats, error)
	GetResourceStats(ctx context.Context) ([]*agentpb.ResourceContainerStats, error)
	GetListeningPorts(ctx context.Context, appName string) ([]*agentpb.PortEntry, error)
	GetContainerMetrics(ctx context.Context, appName string) (ContainerMetrics, error)
	GetContainerMCPPort(ctx context.Context, appName string) (uint32, error)
	// GetContainerRestartPolicyLabel returns the raw restart policy label stored on
	// the container (e.g. "unless-stopped", "on-failure:5", "no"). An empty string
	// is returned when the container exists but has no restart policy label.
	GetContainerRestartPolicyLabel(ctx context.Context, appName string) (string, error)
}

// ImagePreparer is the optional fast-deploy capability that assembles
// chunk-backed image layers and unpacks their snapshots before RunContainer.
// It stays separate from ContainerdClient so older test doubles and alternate
// runtimes can decline the optimization without affecting normal deploys.
type ImagePreparer interface {
	PrepareImage(ctx context.Context, imageName string, layers []*agentpb.RunContainerLayerHeader, imageConfig []byte) error
}

// CachePruneResult describes Wendy-managed container cache pins released by a
// ContainerdCachePruner. The byte counts are the aggregate sizes of the
// affected objects, not a promise that all of those bytes are unreachable:
// containerd keeps anything still referenced by an image or active container.
type CachePruneResult struct {
	ContentBlobs      uint64
	ContentBytes      uint64
	Snapshots         uint64
	SnapshotBytes     uint64
	MinimumAgeSeconds uint64
}

// ContainerdCachePruner is an optional capability implemented by the real
// containerd client. It remains separate from ContainerdClient so existing
// service fakes do not need to implement maintenance operations.
type ContainerdCachePruner interface {
	PruneCache(ctx context.Context, dryRun bool) (CachePruneResult, error)
}

// ContainerExecer is the optional capability to run a process inside a running
// container with an interactive PTY (docker `exec -it`). The ExecContainer RPC
// type-asserts for it; a client that does not implement it yields Unimplemented.
// Kept separate (like GroupRestarter) so the many ContainerdClient mocks stay
// untouched.
type ContainerExecer interface {
	// ExecInContainer runs command in the named app's running container. When tty
	// is true a PTY is allocated (stderr merged into stdout) and resize events
	// ([rows, cols]) are applied to the process. Returns the process exit code.
	ExecInContainer(ctx context.Context, appName string, command []string, tty bool, stdin io.Reader, stdout, stderr io.Writer, resize <-chan [2]uint32) (int, error)
}

// BootContainer describes a container the boot reconcile should bring back up,
// along with the restart policy to register it under. RestartPolicy is the bare
// policy string ("unless-stopped", "on-failure", "always"; empty means default
// keep-running); MaxRetries applies to on-failure.
type BootContainer struct {
	Name          string
	RestartPolicy string
	MaxRetries    int
}

// GroupRestarter is the optional capability a ContainerdClient may provide to
// restart a shared-namespace app group as a unit. The container monitor
// type-asserts for it and falls back to single-container restarts when the
// client does not implement it (e.g. in tests). It is a separate interface so
// the large ContainerdClient interface and its many mocks stay untouched.
type GroupRestarter interface {
	// GroupRestartAppID reports whether appName belongs to a shared-namespace app
	// group (shared-ipc/shared-network, more than one service) and returns the
	// bare appID when it does. Such members must restart together (see
	// RestartGroup): a secondary's namespace join is resolved against the
	// primary's live task, so an independent restart strands it in a dead
	// namespace.
	GroupRestartAppID(ctx context.Context, appName string) (appID string, grouped bool)
	// RestartGroup restarts every service of the shared-namespace group as a unit
	// — stopping all members, starting the primary, then re-resolving each
	// secondary's namespace join against the primary's new task before starting
	// it — and returns the per-service output channels keyed by full container
	// name.
	RestartGroup(ctx context.Context, appID string) (map[string]<-chan ContainerOutput, error)
}

// AppStateRebuilder is the optional capability a ContainerdClient may provide to
// rebuild its in-memory per-app caches (isolation mode + service graph) from
// persisted container labels. ReconcileBootContainers type-asserts for it and
// calls it before listing boot containers, so the caches are warm before any
// StartContainer runs (an empty appIsolation after a reboot would otherwise make
// StartContainer skip CNI networking + mesh egress for isolated apps). Kept
// separate from ContainerdClient so the large interface and its mocks stay
// untouched, mirroring GroupRestarter.
type AppStateRebuilder interface {
	RebuildAppStateCaches(ctx context.Context)
}

// GPUDeviceReporter is the optional capability a ContainerdClient may provide to
// report whether a container was created with a gpu entitlement. The container
// monitor type-asserts for it and treats every container as non-GPU when the
// client does not implement it. Kept separate from ContainerdClient so the
// large interface and its mocks stay untouched (mirrors GroupRestarter).
type GPUDeviceReporter interface {
	// HasGPUEntitlement reads the persisted entitlement. Lookup errors must not
	// be cached as a negative answer; the monitor retries them on a later tick.
	HasGPUEntitlement(ctx context.Context, appName string) (bool, error)
}

// PortExposureProber is the optional capability to scan running host-network
// apps for publicly-bound listening ports and log a warning for each new
// exposure. The container monitor calls it once per health tick; the
// implementation dedups so a given exposure is logged once. Kept separate from
// ContainerdClient so the large interface and its mocks stay untouched
// (mirrors GroupRestarter / AppStateRebuilder).
type PortExposureProber interface {
	WarnPubliclyExposedPorts(ctx context.Context)
}

// Restart policy constants mirror container.RestartPolicy values and are used
// as the policy argument to ContainerMonitorRegistrar.Register.
const (
	RestartPolicyNo            = 0 // never restart
	RestartPolicyUnlessStopped = 1 // restart unless explicitly stopped
	RestartPolicyOnFailure     = 2 // restart only on non-zero exit
	RestartPolicyAlways        = 3 // always restart
)

// ContainerMonitorRegistrar is the subset of container.ContainerMonitor used by
// ContainerService. It is declared here (rather than importing the container
// package) to avoid a circular dependency: container imports services.
type ContainerMonitorRegistrar interface {
	// Register adds appName to the monitor with the given restart policy.
	// policy values mirror container.RestartPolicy: use the RestartPolicy*
	// constants defined in this package.
	Register(appName string, policy int, maxRetries int)
	// Unregister removes appName from the monitor.
	Unregister(appName string)
	// MarkExplicitStop marks appName as intentionally stopped so it won't be
	// automatically restarted, regardless of restart policy.
	MarkExplicitStop(appName string)
	// ClearExplicitStop reverts a prior MarkExplicitStop call, re-enabling
	// automatic restarts for appName. Used to undo a pre-emptive mark when the
	// stop operation itself fails.
	ClearExplicitStop(appName string)
}

// RestartStatus is the container monitor's live bookkeeping for one monitored
// container, keyed the same way the monitor registers state (bare appID, or
// "{appID}_{serviceName}" for services-map apps).
type RestartStatus struct {
	// FailureCount is how many automatic restarts the monitor has performed.
	FailureCount int
	// WillRestart reports that the restart policy is still active for this
	// container (not explicitly stopped, retry budget not exhausted): if the
	// container is stopped, the monitor will start it again. Combined with
	// FailureCount > 0 and a stopped container this is a crash loop.
	WillRestart bool
}

// RestartStatusProvider is the optional capability a ContainerMonitorRegistrar
// may provide to expose its restart bookkeeping. ListContainers type-asserts
// for it so a crash-looping app can be reported as CRASH_LOOPING with its
// failure count instead of a plain STOPPED (WDY-1826). A separate interface so
// existing registrar fakes stay untouched (same pattern as GroupRestarter).
type RestartStatusProvider interface {
	// RestartStatuses returns the status of every monitored container, keyed by
	// the monitored container name.
	RestartStatuses() map[string]RestartStatus
}

type ContainerOutput struct {
	Stdout []byte
	Stderr []byte
	Done   bool
}

type ContainerMetrics struct {
	UserCPUNanos int64 // cumulative user-mode CPU time in nanoseconds
	SysCPUNanos  int64 // cumulative kernel-mode CPU time in nanoseconds
	MemBytes     int64 // current memory usage in bytes
}

// ROS2Target describes a Wendy-managed container carrying the
// sh.wendy/entitlement.ros2 label (WDY-884, WDY-1332).
type ROS2Target struct {
	ContainerID string
	AppID       string
	Distro      string // e.g. "humble"
	DomainID    int    // resolved ROS_DOMAIN_ID
	RMW         string // resolved RMW_IMPLEMENTATION (e.g. "rmw_cyclonedds_cpp"); "" if unset
	Running     bool
	TaskPID     uint32 // pid of the container's init process; 0 when not running
}

// ROS2Sidecar describes one running ROS 2 CLI sidecar container. A device with
// apps on multiple RMWs runs one sidecar per RMW (WDY-1594); each inspects the
// graph of its own RMW.
type ROS2Sidecar struct {
	Name     string // sidecar container ID (per-RMW)
	Distro   string
	DomainID int    // default DDS domain, taken from the anchor app container
	RMW      string // the RMW this sidecar speaks (e.g. "rmw_cyclonedds_cpp"); "" = image default
}

// ROS2ExecOptions configures a single `ros2` invocation inside the sidecar.
type ROS2ExecOptions struct {
	DomainID    int      // ROS_DOMAIN_ID for this invocation
	Args        []string // arguments after `ros2`, passed without shell interpretation
	SidecarName string   // which per-RMW sidecar to exec in; empty = the default/first
}

// ROS2Runtime abstracts the containerd-side ROS 2 sidecar plumbing used by
// the ROS2Service gRPC handlers (WDY-1332).
type ROS2Runtime interface {
	// FindROS2Containers returns all containers labelled with a ros2 config.
	FindROS2Containers(ctx context.Context) ([]ROS2Target, error)
	// EnsureROS2Sidecars starts or reuses one CLI sidecar per distinct RMW in
	// use by the running ROS 2 apps, and tears down sidecars whose RMW is no
	// longer present. Returns one entry per live RMW graph (WDY-1594). Returns
	// an error when no ROS 2 app is running.
	EnsureROS2Sidecars(ctx context.Context) ([]ROS2Sidecar, error)
	// StopROS2Sidecar stops and removes the sidecar if present.
	StopROS2Sidecar(ctx context.Context) error
	// VerifyROS2Sidecar reports whether the sidecar is still anchored to a
	// live ROS 2 app container. It returns an error describing the problem
	// when the anchor container stopped or was replaced (e.g. the app was
	// redeployed), which invalidates the sidecar's network namespace.
	VerifyROS2Sidecar(ctx context.Context) error
	// ExecROS2 runs `ros2 <args>` in the sidecar, streaming output to the
	// writers, and returns the exit code. Cancelling ctx sends SIGINT first
	// so commands like `ros2 bag record` can finalize.
	ExecROS2(ctx context.Context, opts ROS2ExecOptions, stdout, stderr io.Writer) (int, error)
}
