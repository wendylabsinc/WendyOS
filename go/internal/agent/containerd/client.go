package containerd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	digest "github.com/opencontainers/go-digest"
	"github.com/opencontainers/image-spec/identity"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/board"
	"github.com/wendylabsinc/wendy/go/internal/agent/cdi"
	"github.com/wendylabsinc/wendy/go/internal/agent/dbusproxy"
	"github.com/wendylabsinc/wendy/go/internal/agent/logfields"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

var errAppStopping = errors.New("app is currently being stopped")

// Compile-time check that *Client satisfies services.ContainerdClient.
var _ services.ContainerdClient = (*Client)(nil)

// DefaultAddress is the default containerd socket path on Linux.
const DefaultAddress = "/run/containerd/containerd.sock"

type AppSystemAPISocketProvider interface {
	Ensure(appID, serviceName string, capabilities []string) (string, error)
	Release(appID, serviceName string)
	ReleaseApp(appID string)
}

// restartSuppressor is the narrow capability the client needs from the
// container-restart monitor: pause automatic restarts for a container name
// while a replace or stop operation holds the handle, so the monitor's
// periodic tick cannot resurrect the task the client is mid-way through
// killing and deleting (see suppressRestarts). Declared here — rather than
// importing internal/agent/container, which would cycle back through
// internal/agent/services — so *container.ContainerMonitor satisfies it
// structurally; wired in from main.go via SetRestartSuppressor.
type restartSuppressor interface {
	// Suppress pauses restarts for containerName until the returned resume
	// func runs.
	Suppress(containerName string) func()
}

// dbusProxyManager is the narrow lifecycle surface containerd needs. Keeping
// it as an interface lets the reboot/start path be tested without launching a
// real xdg-dbus-proxy process.
type dbusProxyManager interface {
	Start(context.Context, string) (string, error)
	Stop(string) error
	StopAll()
}

type Client struct {
	client                  *containerd.Client
	logger                  *zap.Logger
	namespace               string
	mu                      sync.Mutex
	proxyManager            dbusProxyManager // nil if xdg-dbus-proxy is not available
	systemAPISocketProvider AppSystemAPISocketProvider

	// cameraLoopbackProvider is the VideoService camera-loopback API (Task
	// C6), injected via SetCameraLoopbackProvider (camera_wiring.go). Nil is
	// a valid, common state — a build without the ipcam module, or before
	// main.go wires it in — and every camera-loopback call site tolerates it
	// as "camera sync unavailable, skip silently", mirroring
	// systemAPISocketProvider's nil-tolerant treatment above.
	cameraLoopbackProvider CameraLoopbackProvider

	// appServices caches the services map for multi-service apps, keyed by appID.
	// Populated on CreateContainerWithProgress; used by resolveStopOrder.
	appServices map[string]map[string]*appconfig.ServiceConfig

	// primaryPIDs tracks the PID of the primary (namespace-owner) container
	// for each shared-namespace app group. Protected by mu.
	primaryPIDs map[string]uint32

	// appIsolation caches the isolation mode for each appID.
	// Populated on CreateContainerWithProgress; read by StartContainer.
	appIsolation map[string]string

	// warnedExposures dedups public-port exposure warnings, keyed by
	// exposureKey. Protected by mu. Rebuilt each probe so a vanished exposure
	// is pruned (and re-warned if it returns).
	warnedExposures map[string]struct{}

	// serviceIPs maps appID → serviceName → IP for isolated-mode apps.
	// Updated after each successful CNI ADD. Protected by mu.
	serviceIPs map[string]map[string]string

	// preparedSnapshots holds a fresh, never-executed writable rootfs prepared
	// by PrepareImage while chunks are still arriving. CreateContainer consumes
	// it exactly once. It is guarded separately from mu because image preparation
	// intentionally overlaps deployment work.
	preparedSnapshotsMu sync.Mutex
	preparedSnapshots   map[string]*preparedSnapshot

	// networkSandboxes owns CNI-configured namespace bind mounts retained
	// across task restart and compatible container replacement. It has a
	// separate mutex because CNI work deliberately runs after c.mu is released.
	networkSandboxesMu sync.Mutex
	networkSandboxes   map[string]*networkSandbox
	networkOpsMu       sync.Mutex
	networkOps         map[string]*networkOperation

	// appStopping tracks appIDs that are currently being stopped or deleted.
	// Set before releasing c.mu in StopContainer/DeleteContainer; cleared in the
	// cleanup phase. Checked by create/start paths to reject concurrent lifecycle races
	// (SOC2-CC6, NIST-AC-3, ISO27001-A.8).
	appStopping map[string]bool

	// ros2ExecRefs counts active ExecROS2 calls per sidecar name. Protected by mu.
	// Teardown paths check this before SIGKILLing a sidecar (WDY-1702 H5).
	ros2ExecRefs map[string]int

	// chunkIndex maps CDC chunk hashes to byte ranges in uncompressed layer
	// blobs (Model B). staging holds chunks received this session until the
	// following AssembleLayerFromChunks consumes them.
	chunkIndex *ChunkIndex
	staging    *staging

	// snapshotter is the containerd snapshotter to use for new snapshots.
	// Defaults to "overlayfs" when supported; falls back to "native" on kernels
	// that do not support overlay mounts (e.g. nested container environments).
	snapshotter string

	// meshDNS is the shared mesh DNS service, injected once at agent startup
	// via SetMeshDNS. nil when mesh DNS is unavailable (e.g. build without
	// the mesh feature, or startup failed to bind); mesh_wiring.go treats a
	// nil meshDNS as "DNS best-effort unavailable" and skips it without
	// failing container start — VIP literals still work without it.
	// Interface-typed (meshDNSService, mesh_wiring.go) so tests can inject a
	// recording fake and assert the Ensure/Release pairing invariant.
	meshDNS meshDNSService

	// meshDNSHeld tracks, per containerd container ID, whether that container
	// successfully acquired a DNS-listener reference (ensureMeshDNS). It is
	// the guard that keeps Ensure/Release balanced when sibling services
	// share one gateway listener and when a container is torn down twice
	// (stopOne then deleteOne). Guarded by meshMu, NOT c.mu: deleteOne runs
	// with c.mu already held by DeleteContainer, so touching this map under
	// c.mu would deadlock there, while stopOne runs without c.mu held.
	meshMu      sync.Mutex
	meshDNSHeld map[string]bool

	// restartMonitor lets replace/stop pause the restart monitor's tick for
	// the container they are tearing down (see suppressRestarts). nil when
	// containerd came up without a monitor being wired in (e.g. many unit
	// tests construct a bare *Client) — suppressRestarts no-ops in that case,
	// same nil-tolerant treatment as meshDNS above.
	restartMonitor restartSuppressor
}

// SetRestartSuppressor injects the container-restart monitor's suppression
// handle. Called once from main.go at agent startup, after the monitor is
// constructed (the monitor itself is constructed from the client, so this
// necessarily wires in after SetMeshDNS/SetAppSystemAPISocketProvider). Not
// protected by c.mu: set once during single-threaded startup before the
// Client is exposed to concurrent callers, mirroring SetMeshDNS.
func (c *Client) SetRestartSuppressor(s restartSuppressor) {
	c.restartMonitor = s
}

// suppressRestarts pauses the restart monitor for containerName for the
// duration of the caller's replace/stop operation. Returns a no-op resume
// func when no monitor is wired (containerd came up without one, or a unit
// test constructed a bare *Client), so callers can unconditionally
// `defer resume()` without a nil check.
func (c *Client) suppressRestarts(containerName string) func() {
	if c.restartMonitor == nil {
		return func() {}
	}
	return c.restartMonitor.Suppress(containerName)
}

// SetMeshDNS injects the shared mesh DNS server used by applyMeshEgress and
// teardownMeshEgress to manage per-gateway listeners. Called once from
// main.go at agent startup, before any containers start. Not protected by
// c.mu: it is set once during single-threaded startup before the Client is
// exposed to concurrent callers. A nil server is ignored so the field stays
// a nil interface (a non-nil interface wrapping a nil *mesh.DNSServer would
// defeat the `c.meshDNS == nil` availability checks in mesh_wiring.go).
func (c *Client) SetMeshDNS(d *mesh.DNSServer) {
	if d == nil {
		return
	}
	c.meshDNS = d
}

// SetAppSystemAPISocketProvider injects the manager for private app System API sockets.
func (c *Client) SetAppSystemAPISocketProvider(provider AppSystemAPISocketProvider) {
	c.systemAPISocketProvider = provider
}

type appSystemAPIOwner struct {
	appID       string
	serviceName string
}

func appSystemAPIOwnersFromLabels(labelSets []map[string]string) []appSystemAPIOwner {
	owners := make([]appSystemAPIOwner, 0, len(labelSets))
	for _, labels := range labelSets {
		appID := labels[labelKeyAppID]
		serviceName := labels[labelKeyServiceName]
		if appconfig.ValidateAppID(appID) != nil || (serviceName != "" && appconfig.ValidateServiceName(serviceName) != nil) {
			continue
		}
		if entitlementsContain(parseEntitlementsFromAnnotations(labels), appconfig.EntitlementNotifications) {
			owners = append(owners, appSystemAPIOwner{appID: appID, serviceName: serviceName})
		}
	}
	return owners
}

// RestoreAppSystemAPISockets reconstructs listeners and ownership from trusted,
// persisted container labels after an Agent restart. Stopped containers count
// too because they retain the socket directory mount and may be started later.
func (c *Client) RestoreAppSystemAPISockets(ctx context.Context) {
	if c.systemAPISocketProvider == nil {
		return
	}
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppID))
	if err != nil {
		c.logger.Warn("restore app System API sockets: listing containers failed", zap.Error(err))
		return
	}
	labelSets := make([]map[string]string, 0, len(ctrs))
	for _, ctr := range ctrs {
		info, err := ctr.Info(ctx)
		if err != nil {
			c.logger.Warn("restore app System API socket: reading container failed", zap.String("id", ctr.ID()), zap.Error(err))
			continue
		}
		labelSets = append(labelSets, info.Labels)
	}
	for _, owner := range appSystemAPIOwnersFromLabels(labelSets) {
		if _, err := c.systemAPISocketProvider.Ensure(owner.appID, owner.serviceName, []string{services.SystemAPICapabilityNotifications}); err != nil {
			c.logger.Warn("restore app System API socket failed", zap.String(logfields.AppID, owner.appID), zap.Error(err))
		}
	}
}

func NewClient(logger *zap.Logger, address string, proxyMgr *dbusproxy.Manager) (*Client, error) {
	if address == "" {
		address = DefaultAddress
	}

	c, err := containerd.New(address)
	if err != nil {
		return nil, fmt.Errorf("connecting to containerd at %s: %w", address, err)
	}

	chunkIndexPath := "/var/lib/wendy/chunk-index.json"
	idx, err := NewChunkIndex(chunkIndexPath)
	if err != nil {
		return nil, fmt.Errorf("loading chunk index: %w", err)
	}

	snapshotter := probeSnapshotter(logger)

	return &Client{
		client:            c,
		logger:            logger,
		namespace:         "default",
		proxyManager:      proxyMgr,
		appServices:       make(map[string]map[string]*appconfig.ServiceConfig),
		primaryPIDs:       make(map[string]uint32),
		appIsolation:      make(map[string]string),
		warnedExposures:   make(map[string]struct{}),
		serviceIPs:        make(map[string]map[string]string),
		preparedSnapshots: make(map[string]*preparedSnapshot),
		networkSandboxes:  make(map[string]*networkSandbox),
		networkOps:        make(map[string]*networkOperation),
		appStopping:       make(map[string]bool),
		ros2ExecRefs:      make(map[string]int),
		chunkIndex:        idx,
		staging:           newStaging(defaultChunkStagingDir),
		snapshotter:       snapshotter,
	}, nil
}

// probeSnapshotter returns "overlayfs" if the kernel supports overlay mounts,
// otherwise "native". Implemented in client_linux.go (Linux) and
// client_other.go (always "native" on non-Linux platforms).

// Close releases the underlying containerd client connection and stops all
// D-Bus proxy processes.
func (c *Client) Close() error {
	c.discardAllPreparedSnapshots()
	// Do not tear down retained network sandboxes here. Closing an agent client
	// does not stop its containerd tasks; their nsfs bind mounts and CNI state
	// must survive an agent restart so the next client can validate and adopt
	// them. Explicit stop/delete paths remain the lifecycle boundary.
	if c.proxyManager != nil {
		c.proxyManager.StopAll()
	}
	return c.client.Close()
}

func (c *Client) withNamespace(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, c.namespace)
}

// setPrimaryPID records the PID of the primary container for appID.
// Caller must hold c.mu.
func (c *Client) setPrimaryPID(appID string, pid uint32) {
	if c.primaryPIDs == nil {
		c.primaryPIDs = make(map[string]uint32)
	}
	c.primaryPIDs[appID] = pid
}

// getPrimaryPID returns the PID of the primary container, if known.
// Caller must hold c.mu.
func (c *Client) getPrimaryPID(appID string) (uint32, bool) {
	pid, ok := c.primaryPIDs[appID]
	return pid, ok
}

// primaryTaskAlive reports whether pid belongs to a currently running task of
// one of appID's containers. Used to detect stale primaryPIDs entries left
// behind when a primary exits or is redeployed without a group stop. ctx must
// already carry the containerd namespace; caller must hold c.mu.
func (c *Client) primaryTaskAlive(ctx context.Context, appID string, pid uint32) bool {
	ctrs, err := c.containersForApp(ctx, appID)
	if err != nil {
		return false
	}
	for _, ctr := range ctrs {
		task, terr := ctr.Task(ctx, nil)
		if terr != nil {
			continue
		}
		if st, serr := task.Status(ctx); serr != nil || st.Status != containerd.Running {
			continue
		}
		if task.Pid() == pid {
			return true
		}
	}
	return false
}

// clearPrimaryPID removes the primary PID entry when the app group stops.
// Caller must hold c.mu.
func (c *Client) clearPrimaryPID(appID string) {
	delete(c.primaryPIDs, appID)
}

// getIsolation returns the cached isolation mode for appID. Caller must hold c.mu.
func (c *Client) getIsolation(appID string) string {
	return c.appIsolation[appID]
}

// hydrateIsolation warms c.appIsolation[appID] from a persisted container
// label (labelKeyIsolation) when the in-memory cache has no value for appID
// yet. This repopulates the cache after an agent restart or device reboot,
// when c.appIsolation starts out empty even though isolated containers were
// created (and labelled) in a previous process lifetime. It is idempotent and
// safe to call repeatedly: once a value is set — whether by
// CreateContainerWithProgress or a prior hydrate — it is never overwritten,
// so a live create-time value always wins over a (necessarily identical,
// since the label was written at create time) rehydrated one.
//
// Acquires c.mu; callers that already hold c.mu must use
// hydrateIsolationLocked instead.
func (c *Client) hydrateIsolation(appID string, labels map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hydrateIsolationLocked(appID, labels)
}

// hydrateIsolationLocked is the lock-free core of hydrateIsolation. Caller
// must hold c.mu.
func (c *Client) hydrateIsolationLocked(appID string, labels map[string]string) {
	if appID == "" {
		return
	}
	if c.appIsolation == nil {
		c.appIsolation = make(map[string]string)
	}
	if c.appIsolation[appID] != "" {
		return // already set (live create or earlier hydrate) — never override
	}
	if v := labels[labelKeyIsolation]; v != "" {
		c.appIsolation[appID] = v
	}
}

// recordServiceIP stores the CNI-assigned IP for a service. Caller must hold c.mu.
func (c *Client) recordServiceIP(appID, serviceName, ip string) {
	if c.serviceIPs == nil {
		c.serviceIPs = make(map[string]map[string]string)
	}
	if c.serviceIPs[appID] == nil {
		c.serviceIPs[appID] = make(map[string]string)
	}
	c.serviceIPs[appID][serviceName] = ip
}

// rebuildCachesFromLabels reconstructs the appIsolation and appServices caches
// from a list of per-container label maps. Pure (no containerd calls, no lock)
// so it is unit-testable without a live containerd, mirroring the
// containerd-free split used for mesh resolv.conf recreation. A container with
// no appID label is skipped; a blank isolation label yields no isolation entry
// (non-isolated, the default). Only DependsOn is reconstructed for services —
// it is the sole ServiceConfig field read after create (len + ServiceTopoOrder).
func rebuildCachesFromLabels(containerLabels []map[string]string) (
	isolation map[string]string,
	servicesByApp map[string]map[string]*appconfig.ServiceConfig,
) {
	isolation = make(map[string]string)
	servicesByApp = make(map[string]map[string]*appconfig.ServiceConfig)
	for _, labels := range containerLabels {
		appID := labels[labelKeyAppID]
		if appID == "" {
			continue
		}
		if iso := labels[labelKeyIsolation]; iso != "" {
			isolation[appID] = iso
		}
		if svc := labels[labelKeyServiceName]; svc != "" {
			if servicesByApp[appID] == nil {
				servicesByApp[appID] = make(map[string]*appconfig.ServiceConfig)
			}
			servicesByApp[appID][svc] = &appconfig.ServiceConfig{
				DependsOn: parseDependsOn(labels[labelKeyDependsOn]),
			}
		}
	}
	return isolation, servicesByApp
}

// RebuildAppStateCaches repopulates the appIsolation and appServices caches
// from persisted container labels. The maps are otherwise written only at
// container-create time, so after an agent restart (reboot) they start empty
// and StartContainer skips isolated-container wiring (CNI, /etc/hosts, mesh
// egress). Best-effort: any failure logs and returns without blocking boot
// recovery. Idempotent — fills only entries not already present; a
// concurrently-created live entry always wins.
func (c *Client) RebuildAppStateCaches(ctx context.Context) {
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppID))
	if err != nil {
		c.logger.Warn("rebuild app-state caches: listing containers failed", zap.Error(err))
		return
	}

	// Read labels outside the lock — Info() is containerd I/O.
	labelSets := make([]map[string]string, 0, len(ctrs))
	for _, ctr := range ctrs {
		info, infoErr := ctr.Info(ctx)
		if infoErr != nil {
			c.logger.Warn("rebuild app-state caches: reading container info failed",
				zap.String("id", ctr.ID()), zap.Error(infoErr))
			continue
		}
		labelSets = append(labelSets, info.Labels)
	}

	isolation, servicesByApp := rebuildCachesFromLabels(labelSets)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.appIsolation == nil {
		c.appIsolation = make(map[string]string)
	}
	for appID, iso := range isolation {
		if _, ok := c.appIsolation[appID]; !ok {
			c.appIsolation[appID] = iso
		}
	}
	if c.appServices == nil {
		c.appServices = make(map[string]map[string]*appconfig.ServiceConfig)
	}
	for appID, svcs := range servicesByApp {
		if _, ok := c.appServices[appID]; !ok {
			c.appServices[appID] = svcs
		}
	}
	c.logger.Info("Rebuilt app-state caches from labels",
		zap.Int("apps_isolation", len(isolation)), zap.Int("apps_services", len(servicesByApp)))
}

// WarnPubliclyExposedPorts scans running host-network apps and logs a WARN for
// each newly-observed publicly-bound listening port, so operators notice a
// service exposed on the device's real interfaces. Best-effort: any failure
// logs and returns without affecting the caller. Deduped per
// (appID, protocol, port, address); a vanished exposure is pruned so it warns
// again if it reappears.
func (c *Client) WarnPubliclyExposedPorts(ctx context.Context) {
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppID))
	if err != nil {
		c.logger.Warn("port-exposure probe: listing containers failed", zap.Error(err))
		return
	}

	// Gather unique host-network appIDs among running containers (outside the lock).
	hostNetApps := make(map[string]struct{})
	for _, ctr := range ctrs {
		if !c.containerIsRunning(ctx, ctr) {
			continue
		}
		info, infoErr := ctr.Info(ctx)
		if infoErr != nil {
			c.logger.Warn("port-exposure probe: reading container info failed",
				zap.String("id", ctr.ID()), zap.Error(infoErr))
			continue
		}
		appID := info.Labels[labelKeyAppID]
		if appID == "" {
			continue
		}
		if entitlementsUseHostNetwork(parseEntitlementsFromAnnotations(info.Labels)) {
			hostNetApps[appID] = struct{}{}
		}
	}

	// Read each host-network app's listening ports (outside the lock).
	portsByApp := make(map[string][]*agentpb.PortEntry, len(hostNetApps))
	for appID := range hostNetApps {
		ports, portErr := c.GetListeningPorts(ctx, appID)
		if portErr != nil {
			c.logger.Warn("port-exposure probe: reading listening ports failed",
				zap.String(logfields.AppID, appID), zap.Error(portErr))
			continue
		}
		portsByApp[appID] = ports
	}

	current := collectExposures(portsByApp)

	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]struct{}, len(current))
	for key, e := range current {
		next[key] = struct{}{}
		if _, warned := c.warnedExposures[key]; warned {
			continue
		}
		c.logger.Warn("app is listening on a publicly reachable address; the port is exposed on the device's network (network mode: host). For private cross-device access, use a \"mesh\" network entitlement.",
			zap.String(logfields.AppID, e.appID),
			zap.String("protocol", e.protocol),
			zap.Uint32("port", e.port),
			zap.String("bind_address", e.address))
	}
	c.warnedExposures = next
}

// isPubliclyBoundAddress reports whether a listening socket's bind address is
// reachable from outside the host — a wildcard (0.0.0.0 / ::) or a specific
// non-loopback interface address. Loopback (127.0.0.0/8, ::1) is private; an
// empty or unparseable address is treated as not-public (we only warn on a
// definite exposure).
func isPubliclyBoundAddress(addr string) bool {
	a, err := netip.ParseAddr(addr)
	if err != nil {
		return false
	}
	return !a.IsLoopback()
}

// exposedPort identifies one publicly-bound listening socket of an app, used as
// the dedup unit for exposure warnings.
type exposedPort struct {
	appID    string
	protocol string
	port     uint32
	address  string
}

// exposureKey is the stable dedup key for an exposedPort.
func exposureKey(e exposedPort) string {
	return fmt.Sprintf("%s|%s|%d|%s", e.appID, e.protocol, e.port, e.address)
}

// collectExposures returns the publicly-bound listening sockets across the given
// host-network apps, keyed by exposureKey. Pure (no containerd, no lock) so it
// is unit-testable; the caller supplies each host-network app's listening ports.
func collectExposures(portsByApp map[string][]*agentpb.PortEntry) map[string]exposedPort {
	out := make(map[string]exposedPort)
	for appID, ports := range portsByApp {
		for _, p := range ports {
			if !isPubliclyBoundAddress(p.Address) {
				continue
			}
			e := exposedPort{appID: appID, protocol: p.Protocol, port: p.Port, address: p.Address}
			out[exposureKey(e)] = e
		}
	}
	return out
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

// maxImageConfigBytes bounds the OCI image config blob accepted over the wire.
// A real config (Cmd/Entrypoint/Env/WorkingDir/User + metadata) is a few KiB;
// 1 MiB is generous headroom while still rejecting an abusive payload.
const maxImageConfigBytes = 1 << 20

// assembleLeaseExpiration bounds how long the assemble lease keeps the config
// and manifest blobs alive before the image record roots them. Only a backstop
// for an agent that dies mid-assemble; the happy path releases immediately.
const assembleLeaseExpiration = 30 * time.Minute

func (c *Client) AssembleImage(ctx context.Context, imageName string, layers []*agentpb.RunContainerLayerHeader, imageConfig []byte) error {
	ctx = c.withNamespace(ctx)
	cleanupCtx := c.withNamespace(context.Background())
	cs := c.client.ContentStore()
	is := c.client.ImageService()

	// Config and manifest are unreferenced between their writes and is.Create,
	// and containerd's GC scheduler collects on any lease release. Hold a lease
	// across the whole assemble so nothing can be reaped inside that window.
	ctx, doneLease, err := c.client.WithLease(ctx, leases.WithExpiration(assembleLeaseExpiration))
	if err != nil {
		return fmt.Errorf("creating assemble lease: %w", err)
	}
	defer func() {
		if err := doneLease(cleanupCtx); err != nil {
			c.logger.Warn("Failed to release assemble lease; relying on expiration backstop",
				zap.Duration("expiration", assembleLeaseExpiration),
				zap.Error(err),
			)
		}
	}()

	// Store the image under the SAME normalized name that
	// CreateContainerWithProgress uses for its GetImage lookup. Without this,
	// a short ref like "app:latest" is stored verbatim here but looked up as
	// "docker.io/library/app:latest" at create time, missing the local store
	// and falling through to a (failing) registry pull.
	imageName = normalizeImageName(imageName)

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

	// Build the OCI image config. When the caller supplies the original config
	// blob (chunk-diff path), preserve it so the runtime config — Cmd,
	// Entrypoint, Env, WorkingDir, User — survives reassembly; otherwise a
	// container created from this image would have no command and exit
	// immediately. We override RootFS.DiffIDs with the diff IDs we just computed
	// so the config always matches the layers in this manifest. When no config
	// is supplied we fall back to a minimal synthesized config (legacy callers).
	imgConfig := ocispec.Image{
		Platform: ocispec.Platform{
			Architecture: "arm64",
			OS:           "linux",
		},
	}
	if len(imageConfig) > 0 {
		// A real OCI image config is small (a few KiB). Reject an oversized blob
		// before parsing so a misbehaving client cannot force a large allocation.
		if len(imageConfig) > maxImageConfigBytes {
			return fmt.Errorf("image config too large: %d > %d bytes", len(imageConfig), maxImageConfigBytes)
		}
		// Decode into the typed OCI struct: unknown/extra JSON fields are dropped
		// on the re-marshal below, so only well-formed config survives.
		if err := json.Unmarshal(imageConfig, &imgConfig); err != nil {
			return fmt.Errorf("parsing supplied image config: %w", err)
		}
	}
	// Always re-derive the security-critical layer binding from the diff IDs we
	// computed locally — never trust RootFS supplied over the wire.
	imgConfig.RootFS = ocispec.RootFS{
		Type:    "layers",
		DiffIDs: diffIDs,
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

	// Write manifest to content store. The gc.ref.content.* labels are what make
	// the config and layers reachable from the manifest — without them the image
	// record roots only the manifest itself and the config blob is collected on
	// the next GC sweep, leaving a container with no Cmd/Env/WorkingDir.
	manifestDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    manifestDigest,
		Size:      int64(len(manifestData)),
	}
	manifestLabels := map[string]string{
		"containerd.io/gc.ref.content.config": configDigest.String(),
	}
	for i, l := range layerDescs {
		manifestLabels["containerd.io/gc.ref.content.l."+strconv.Itoa(i)] = l.Digest.String()
	}
	if err := content.WriteBlob(ctx, cs, manifestDigest.String(), bytes.NewReader(manifestData), manifestDesc,
		content.WithLabels(manifestLabels)); err != nil {
		if !errdefs.IsAlreadyExists(err) {
			return fmt.Errorf("writing manifest blob: %w", err)
		}
		// An identical manifest written by an older agent carries no gc.ref
		// labels, so its config stays unreachable. Apply them to the existing
		// blob rather than leaving the device permanently poisoned.
		fieldpaths := make([]string, 0, len(manifestLabels))
		for k := range manifestLabels {
			fieldpaths = append(fieldpaths, "labels."+k)
		}
		if _, uerr := cs.Update(ctx, content.Info{
			Digest: manifestDigest,
			Labels: manifestLabels,
		}, fieldpaths...); uerr != nil {
			return fmt.Errorf("labelling existing manifest blob: %w", uerr)
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
// It injects "-m debugpy --listen 127.0.0.1:5678" after the Python binary.
//
// SECURITY (WDY-1010): the listener binds loopback only, never 0.0.0.0. debugpy
// exposes an unauthenticated DAP endpoint with full Python RCE; binding all
// interfaces made that reachable by anyone on the device's network during a
// debug session. Remote attach reaches the listener through a device-side
// tunnel (e.g. SSH/`wendy` port-forward) terminating on the device's loopback.
func wrapWithDebugpy(args []string) []string {
	debugpyArgs := []string{"-m", "debugpy", "--listen", "127.0.0.1:5678"}

	if len(args) > 0 {
		base := args[0]
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		if base == "python" || base == "python3" || strings.HasPrefix(base, "python3.") {
			// python3 app.py -> python3 -m debugpy --listen 127.0.0.1:5678 app.py
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
	case "layer-start":
		return &agentpb.CreateContainerProgress{
			Phase:       agentpb.CreateContainerProgress_UNPACKING,
			LayerIndex:  int32(progress.LayerIndex),
			TotalLayers: int32(progress.TotalLayers),
			LayerSize:   progress.LayerSize,
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
	createStarted := time.Now()
	var replaceDuration, imageResolveDuration, unpackDuration, specDuration, snapshotDuration time.Duration
	var replacedExisting bool

	ctx = c.withNamespace(ctx)

	// Derive the app identity. appCfg.AppID is the authoritative source; fall
	// back to req.GetAppName() for raw RPC calls that arrive without a parsed
	// AppConfig. We use a local variable (not a struct mutation) so the caller's
	// AppConfig is unchanged and concurrent/retry uses see a stable value.
	//
	// Validate before assigning to the named variables so that no unvalidated
	// RPC-controlled value ever reaches downstream helpers, even if future
	// refactors reorder code below this block (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
	rawAppID := appCfg.AppID
	if rawAppID == "" {
		rawAppID = req.GetAppName()
	}
	rawServiceName := appCfg.ServiceName

	if err := appconfig.ValidateAppID(rawAppID); err != nil {
		c.logger.Warn("CreateContainer rejected: invalid app ID",
			zap.String(logfields.AppID, sanitizeForLog(rawAppID, 253)), zap.Error(err))
		return fmt.Errorf("invalid app ID: %w", err)
	}
	if rawServiceName != "" {
		if err := appconfig.ValidateServiceName(rawServiceName); err != nil {
			c.logger.Warn("CreateContainer rejected: invalid service name",
				zap.String(logfields.AppID, sanitizeForLog(rawAppID, 253)),
				zap.String("service_name", sanitizeForLog(rawServiceName, 57)),
				zap.Error(err))
			return fmt.Errorf("invalid service name: %w", err)
		}
	}

	// Both values are now validated; promote to short names for readability.
	appID, serviceName := rawAppID, rawServiceName
	containerName := ContainerName(appID, serviceName)
	unlockNetwork := c.lockNetworkOperation(containerName)
	defer unlockNetwork()
	lockStarted := time.Now()
	c.mu.Lock()
	lockWait := time.Since(lockStarted)
	defer c.mu.Unlock()

	// Reject creation while a concurrent StopContainer is tearing down this app.
	// Without this check a new container could be created after resolveStopOrder
	// snapshots the container list, leaving it running after StopContainer returns
	// (TOCTOU; SOC2-CC6, NIST-AC-3, ISO27001-A.8).
	if c.appStopping[appID] {
		return fmt.Errorf("app %q is currently being stopped; retry after stop completes", appID)
	}

	desiredNetworkIdentity := ""
	if networkSandboxEligible(serviceName, appCfg.Entitlements) {
		desiredNetworkIdentity = networkIdentity(appCfg.Isolation, appCfg.Entitlements)
	}
	var reusedNetworkSandbox *networkSandbox
	reusedNetworkSandboxCommitted := false
	defer func() {
		// Validation/spec/snapshot/container failures after the old task was
		// removed must not leave a CNI namespace detached from any container.
		if reusedNetworkSandbox != nil && !reusedNetworkSandboxCommitted {
			c.destroyNetworkSandbox(context.WithoutCancel(ctx), containerName)
		}
	}()

	// Canonicalise the image reference so older CLIs sending Docker short
	// names like "python:3.11-slim" still resolve correctly under containerd's
	// strict parser, which would otherwise read "3.11-slim" as a port.
	imageName := normalizeImageName(req.GetImageName())

	report := func(p *agentpb.CreateContainerProgress) {
		if onProgress != nil {
			onProgress(p)
		}
	}

	logFields := []zap.Field{
		zap.String("container_name", containerName),
		zap.String(logfields.AppID, appID),
		zap.String("image", imageName),
	}
	if serviceName != "" {
		logFields = append(logFields, zap.String("service_name", serviceName))
	}
	c.logger.Info("Creating container", logFields...)

	// Determine version from the app config or default.
	version := appCfg.Version
	if version == "" {
		version = "latest"
	}

	// Delete any pre-existing container with the same name.
	phaseStarted := time.Now()
	if existing, err := c.client.LoadContainer(ctx, containerName); err == nil {
		replacedExisting = true
		// Pause the restart monitor for this name for the rest of this
		// function: without it, a crash-looping app's next tick can call
		// StartContainer on this same container between our kill and delete
		// below, resurrecting the task and racing us (observed live:
		// "cannot delete running task: failed precondition"). Held through
		// the new container's creation further down too, so the monitor
		// cannot also race the fresh task being started.
		resumeRestarts := c.suppressRestarts(containerName)
		defer resumeRestarts()

		oldHadSystemAPI := false
		if oldLabels, labelErr := existing.Labels(ctx); labelErr == nil {
			oldHadSystemAPI = entitlementsContain(parseEntitlementsFromAnnotations(oldLabels), appconfig.EntitlementNotifications)
			oldSpec, _ := existing.Spec(ctx)
			if oldLabels[labelKeyNetworkIdentity] == desiredNetworkIdentity {
				reusedNetworkSandbox, _ = c.reusableNetworkSandbox(ctx, containerName, desiredNetworkIdentity)
				if reusedNetworkSandbox == nil && oldSpec != nil {
					reusedNetworkSandbox, _ = c.recoverNetworkSandbox(ctx, existing, desiredNetworkIdentity, oldLabels, oldSpec)
				}
			}
			if reusedNetworkSandbox == nil {
				c.purgePersistedNetworkSandbox(ctx, existing, oldLabels, oldSpec)
			}
		}
		if reusedNetworkSandbox == nil {
			// Identity mismatch, unhealthy namespace, legacy container, or a
			// non-CNI configuration: release every external network side effect
			// before the old task disappears.
			c.destroyNetworkSandbox(ctx, containerName)
		} else {
			c.logger.Debug("Retaining compatible network sandbox across container replacement",
				zap.String("container_name", containerName), zap.String("netns", reusedNetworkSandbox.path))
		}
		c.logger.Info("Removing existing container", zap.String("container_name", containerName))
		// Kill the old task's whole process group — not just init — and wait
		// for it to exit. A surviving process keeps devices/ports the new
		// container needs (WDY-1818: /dev/video0 held for hours after replace).
		if task, taskErr := existing.Task(ctx, nil); taskErr == nil {
			if termErr := c.terminateTask(ctx, task, containerName, syscall.SIGKILL, killWaitTimeout, killWaitTimeout); termErr != nil {
				c.logger.Error("Failed to delete old task during replace; forcing runtime delete",
					zap.String("container_name", containerName),
					zap.Error(termErr))
				c.forceDeleteTask(ctx, containerName)
			}
		} else {
			// Task may be orphaned (shim crashed). Force-delete via the task
			// service directly so the runtime clears the old task ID.
			c.forceDeleteTask(ctx, containerName)
		}
		delErr := existing.Delete(ctx, containerd.WithSnapshotCleanup)
		if delErr != nil && errdefs.IsFailedPrecondition(delErr) {
			// A task exists again despite the kill above — most likely the
			// restart monitor's tick winning a race against suppressRestarts
			// (a tick already past its check when Suppress was called), or an
			// unrelated in-flight start. Kill it again and retry the delete
			// once rather than failing the whole replace outright.
			c.logger.Warn("Existing container still has a running task after kill; retrying",
				zap.String("container_name", containerName), zap.Error(delErr))
			if task, taskErr := existing.Task(ctx, nil); taskErr == nil {
				if termErr := c.terminateTask(ctx, task, containerName, syscall.SIGKILL, killWaitTimeout, killWaitTimeout); termErr != nil {
					// Mirror the first attempt's fallback above: if the
					// re-kill itself fails, fall back to force-deleting the
					// task via the runtime directly before retrying, rather
					// than retrying the container delete against a task we
					// know we failed to kill.
					c.logger.Error("Failed to kill racing task during replace retry; forcing runtime delete",
						zap.String("container_name", containerName), zap.Error(termErr))
					c.forceDeleteTask(ctx, containerName)
				}
			}
			delErr = existing.Delete(ctx, containerd.WithSnapshotCleanup)
		}
		if delErr != nil && !errdefs.IsNotFound(delErr) {
			return fmt.Errorf("deleting existing container %q during replace: %w", containerName, delErr)
		}
		if oldHadSystemAPI && c.systemAPISocketProvider != nil {
			c.systemAPISocketProvider.Release(appID, serviceName)
		}
		// Stop old D-Bus proxy if any.
		if c.proxyManager != nil {
			_ = c.proxyManager.Stop(containerName)
		}
		replaceDuration = time.Since(phaseStarted)
	}

	// Try the local image store first. The device-local registry shares
	// containerd's content store, so anything just pushed to it is already
	// available via GetImage — pulling would just round-trip bytes over
	// loopback. Fall back to a pull only on miss; use PlainHTTP for the
	// local-registry case.
	var image containerd.Image
	var err error
	phaseStarted = time.Now()
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
	imageResolveDuration = time.Since(phaseStarted)

	// Start D-Bus proxy if bluetooth entitlement is present. The returned
	// socket directory is keyed by containerName (which includes the service
	// name for multi-service apps), so it must be threaded through to the
	// bluetooth entitlement verbatim — reconstructing it from appID alone would
	// drop the service suffix and runc would fail with a missing bind-mount
	// source.
	// SECURITY (WDY-1093): refuse to start a bluetooth container when the D-Bus
	// proxy is unavailable, rather than silently starting it without the filter.
	if err := c.requireDBusProxy(appCfg, containerName); err != nil {
		return err
	}

	var dbusProxyStarted bool
	var dbusProxySocketDir string
	if c.proxyManager != nil && hasBluetooth(appCfg) {
		dir, err := c.proxyManager.Start(ctx, containerName)
		if err != nil {
			return fmt.Errorf("starting D-Bus proxy for %q: %w", containerName, err)
		}
		dbusProxySocketDir = dir
		dbusProxyStarted = true
		defer func() {
			if dbusProxyStarted {
				_ = c.proxyManager.Stop(containerName)
			}
		}()
	}

	// Unpack the image into the snapshotter if not already done.
	phaseStarted = time.Now()
	unpacked, err := image.IsUnpacked(ctx, c.snapshotter)
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
	unpackDuration = time.Since(phaseStarted)

	// Read the image's OCI config (CMD, ENTRYPOINT, ENV, WorkingDir). An image
	// whose config cannot be read is unusable: falling back to defaults would
	// discard Cmd/Entrypoint/Env/WorkingDir and silently run /bin/sh in place of
	// the application, which exits 0 immediately and presents as a crash loop
	// with no explanation (WDY-2009).
	phaseStarted = time.Now()
	imageSpec, err := image.Spec(ctx)
	if err != nil {
		return fmt.Errorf("reading image config for %q (image is incomplete or corrupt): %w", imageName, err)
	}

	// Build the container command: explicit request Cmd > image config >
	// /bin/sh, with UserArgs appended to whichever base won.
	args := containerArgs(req.GetCmd(), req.GetUserArgs(), imageSpec.Config)

	// Wrap Python commands with debugpy for remote debugging (only in debug mode).
	if appCfg.Debug && appCfg.Language == "python" {
		args = wrapWithDebugpy(args)
	}

	// Build the working directory: explicit request > image config > /.
	workingDir := req.GetWorkingDir()
	if workingDir == "" && imageSpec.Config.WorkingDir != "" {
		workingDir = imageSpec.Config.WorkingDir
	}
	if workingDir == "" {
		workingDir = "/"
	}

	// Build environment variables.
	// Order: image built-in env → user-provided env (from request) → PATH/TERM
	// defaults (only when absent) → Wendy system env → OTEL injection.
	// WENDY_* vars appear last so they always win in OCI semantics (last KEY
	// wins); PATH and TERM are generic fallbacks that must not clobber values
	// the image or the caller already set (WDY-1825).
	wendyEnv, err := buildContainerBaseEnv(appID, serviceName)
	if err != nil {
		return fmt.Errorf("building container env: %w", err)
	}
	if err := validateUserEnv(req.GetEnv()); err != nil {
		return fmt.Errorf("invalid env var in request (SOC2-CC6, NIST-SI-10): %w", err)
	}
	var env []string
	env = append(env, imageSpec.Config.Env...)
	env = append(env, req.GetEnv()...)
	env = appendFallbackEnv(env)
	env = append(env, wendyEnv...)
	ros2Env, err := buildROS2Env(appCfg, appID, serviceName)
	if err != nil {
		// Fail the create rather than start a ROS 2 container with no
		// ROS_DOMAIN_ID, which silently lands it on the global default domain 0.
		return fmt.Errorf("resolving ROS 2 environment for %q: %w", appID, err)
	}
	env = append(env, ros2Env...)
	env = injectOTELEnvIfNeeded(env, appCfg, appID)

	// Build OCI spec using local oci package, then apply entitlements.
	spec := localoci.DefaultSpec("rootfs", args)
	spec.Process.Cwd = workingDir
	spec.Process.Env = env
	if spec.Linux == nil {
		spec.Linux = &localoci.Linux{}
	}

	// Apply the NVIDIA CDI spec before entitlements so that entitlements can
	// override CDI-injected env vars (e.g. NVIDIA_VISIBLE_DEVICES=void → =all).
	if needsNvidiaCDI(appCfg) {
		c.applyNvidiaCDI(spec)
	}

	var systemAPISocketDir string
	systemAPIRefOwned := false
	if appCfg.HasEntitlement(appconfig.EntitlementNotifications) {
		if c.systemAPISocketProvider == nil {
			return fmt.Errorf("notifications entitlement unavailable: app System API socket manager is not configured")
		}
		systemAPISocketDir, err = c.systemAPISocketProvider.Ensure(
			appID,
			serviceName,
			[]string{services.SystemAPICapabilityNotifications},
		)
		if err != nil {
			return fmt.Errorf("preparing app System API socket: %w", err)
		}
		systemAPIRefOwned = true
		defer func() {
			if systemAPIRefOwned {
				c.systemAPISocketProvider.Release(appID, serviceName)
			}
		}()
	}

	var hostResolvConfPath string
	if entitlementsUseHostNetwork(appCfg.Entitlements) {
		if systemdResolvedStubAvailable() {
			path, writeErr := writeHostResolvConf()
			if writeErr != nil {
				c.logger.Warn("host DNS: could not prepare systemd-resolved stub configuration; falling back to a host resolver file that will not follow later replacements",
					zap.String(logfields.AppID, appID), zap.Error(writeErr))
			} else {
				hostResolvConfPath = path
			}
		} else {
			c.logger.Warn("host DNS: systemd-resolved stub is unavailable; falling back to a host resolver file that will not follow later replacements",
				zap.String(logfields.AppID, appID), zap.String("stub", systemdResolvedStubAddress))
		}
	}

	opts := localoci.ApplyOptions{
		DBusProxySocketDir: dbusProxySocketDir,
		SystemAPISocketDir: systemAPISocketDir,
		HostResolvConfPath: hostResolvConfPath,
	}
	// Pass a shallow copy of appCfg with AppID and ServiceName set to the
	// derived (validated) values. This ensures ApplyEntitlements always receives
	// a non-empty AppID even when the caller used the raw-RPC fallback path
	// where appCfg.AppID was empty and appID was derived from req.GetAppName().
	entCfg := *appCfg
	entCfg.AppID = appID
	entCfg.ServiceName = serviceName

	// Pre-create v4l2loopback camera nodes before ApplyEntitlements runs, for
	// apps with the camera entitlement or its deprecated alias, video
	// (shouldEnsureCameraNodes). This works even though the node is created
	// before applyCamera's mounts/cgroup rules are added to spec below:
	// applyCamera (entitlements.go:773-851, cited at client.go:195-197) both
	// bind-mounts the host's live /dev tree into the container AND allows the
	// whole V4L2 major (81) with the minor left unrestricted, so any node
	// that exists on the host by the time the container starts is visible
	// in-container with no further spec change required. Best-effort and
	// nil-provider-safe (SyncCameraLoopbacks/EnsureCameraNodes swallow their
	// own errors) — module absence is the production default until Branch F
	// ships the v4l2loopback kernel module, and a camera-node failure must
	// never block creation of an otherwise-valid container.
	if c.cameraLoopbackProvider != nil && shouldEnsureCameraNodes(&entCfg) {
		if err := c.cameraLoopbackProvider.EnsureCameraNodes(ctx); err != nil {
			c.logger.Warn("ensuring camera loopback nodes failed", zap.String(logfields.AppID, appID), zap.Error(err))
		}
	}

	if err := localoci.ApplyEntitlements(spec, &entCfg, opts); err != nil {
		return fmt.Errorf("applying entitlements: %w", err)
	}

	// DEPRECATION (specs/2026-07-05-network-bridge-default-design.md): warn,
	// but do not change behavior, when a network entitlement omits "mode" — it
	// currently maps to host networking (every port the app binds becomes
	// reachable on the device's real interfaces), which is being deprecated in
	// favor of an eventual isolated "bridge" default. This never affects
	// container creation; it only informs.
	if hasImplicitHostNetworkMode(entCfg.Entitlements) {
		c.logger.Warn(
			`network entitlement without an explicit "mode" currently uses host networking (ports are publicly reachable). This default will change to isolated "bridge" networking in a future release. Set "mode": "host" to keep host networking, or "mode": "bridge" for isolated networking with outbound internet.`,
			zap.String(logfields.AppID, appID))
	}

	// Set the cgroup path here — client.go is the sole authority so there is
	// no risk of divergence with entitlements.go. SetDeviceCapabilities only
	// adds the cgroup namespace and mount; it no longer sets CgroupsPath.
	// "@" is used as separator because it cannot appear in a valid appID
	// ([a-zA-Z0-9._-]) or serviceName ([a-z][a-z0-9-]*), eliminating the
	// collision risk that a hyphen separator would have introduced.
	//   - Single-container: "system.slice:{systemdSvc}:{appID}"
	//   - Multi-service:    "system.slice:{systemdSvc}:{appID}@{serviceName}"
	//
	// INVARIANT: ApplyEntitlements and CDI helpers must not set CgroupsPath.
	// The assertion below detects any future violation at runtime (SOC2-CC6).
	if spec.Linux.CgroupsPath != "" {
		return fmt.Errorf("security: CgroupsPath was unexpectedly set before assignment (%q); ApplyEntitlements or CDI must not set it", spec.Linux.CgroupsPath)
	}
	cgroupSuffix := appID
	if serviceName != "" {
		cgroupSuffix = appID + "@" + serviceName
	}
	spec.Linux.CgroupsPath = fmt.Sprintf("system.slice:%s:%s", sharedenv.SystemdServiceName(), cgroupSuffix)

	// Apply CPU/memory/PID ceilings from wendy.json (per-service overrides
	// the app-level default). Malformed values are rejected here rather than
	// silently running the container unbounded. CLI-side validation should
	// catch these first, but the agent must not trust the request blindly.
	if err := localoci.ApplyResourceLimits(spec, appCfg.ResolveResourcesForService(serviceName)); err != nil {
		return fmt.Errorf("applying resource limits: %w", err)
	}

	report(&agentpb.CreateContainerProgress{Phase: agentpb.CreateContainerProgress_CREATING_CONTAINER})

	// Persist isolation + this service's dependsOn so appIsolation/appServices
	// can be rebuilt after an agent restart (RebuildAppStateCaches). Single-
	// service apps have no Services entry, so dependsOn stays nil.
	var dependsOn []string
	if serviceName != "" && appCfg.Services != nil {
		if sc := appCfg.Services[serviceName]; sc != nil {
			dependsOn = sc.DependsOn
		}
	}
	labels := wendyLabels(appID, serviceName, version, req.GetRestartPolicy(), appCfg.Entitlements, appCfg.Isolation, dependsOn)
	if identities := captureSerialIdentities(appCfg.Entitlements); len(identities) > 0 {
		encoded, err := json.Marshal(identities)
		if err != nil {
			return fmt.Errorf("encoding serial device identities: %w", err)
		}
		labels[labelKeySerialIdentities] = string(encoded)
	}
	if desiredNetworkIdentity != "" {
		labels[labelKeyNetworkIdentity] = desiredNetworkIdentity
		if reusedNetworkSandbox != nil {
			labels[labelKeyNetworkSandboxIP] = reusedNetworkSandbox.ip
		}
	}

	// Publish the resolved ROS 2 configuration as a container label so the
	// agent can discover ROS 2 containers at runtime and configure the CLI
	// sidecar with the right distro and DDS domain (WDY-884, WDY-1332).
	if ros2 := appCfg.ResolveROS2ConfigForService(serviceName); ros2 != nil {
		if v := appconfig.ROS2AnnotationValue(ros2, appID); v != "" {
			labels[appconfig.ROS2AnnotationKey] = v
		}
	}

	// Inject /etc/hosts bind-mount for isolated multi-service apps so service
	// names resolve via CNI-assigned IPs.
	if appCfg.Isolation == "isolated" && len(appCfg.Services) > 1 {
		// safeJoin rejects separators and dot-only segments, then verifies the
		// result is directly under the base dir (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
		hostsPath, err := safeJoin("/run/wendy/hosts", appID)
		if err != nil {
			return fmt.Errorf("security: appID %q produces unsafe hosts path: %w", appID, err)
		}
		// Always create the directory (os.MkdirAll is idempotent) and seed the
		// hosts file with IPs already known from previously-started sibling services.
		// c.mu is held here (defer Unlock above), so reading c.serviceIPs is safe.
		// Seeding with existing IPs means containers that start late see a useful
		// /etc/hosts from the first moment rather than an empty file (SOC2-CC6).
		// The atomic rename in writeHostsFile prevents truncated reads (NIST-SI-10).
		if err := os.MkdirAll("/run/wendy/hosts", 0o700); err != nil {
			return fmt.Errorf("creating hosts dir: %w", err)
		}
		if err := writeHostsFile(hostsPath, c.serviceIPs[appID]); err != nil {
			return fmt.Errorf("initialising hosts file for %s: %w", appID, err)
		}
		localoci.InjectHostsMount(spec, hostsPath)
	}

	// Inject a read-only /etc/resolv.conf bind-mount for containers that need
	// gateway DNS: meshed multi-service isolated apps (device-N.cloud.wendy.dev
	// hostnames resolve via the mesh DNS listener on this app's bridge
	// gateway) and, per specs/2026-07-05-network-bridge-default-design.md,
	// single-service "bridge"-mode apps too — their isolated namespace has no
	// other way to reach a working upstream resolver (a host resolv.conf bind
	// mount is not reachable from inside an isolated netns when it points at a
	// loopback-scoped stub like systemd-resolved's). appCfg.Entitlements is the
	// same per-service entitlement slice ApplyEntitlements above already
	// consumed (entCfg is a shallow copy of appCfg), so this sees exactly the
	// entitlements this specific service was granted. needsGatewayDNS requires
	// isolation=="isolated" for the mesh case because meshGateway depends on
	// the per-app CNI bridge subnet, which only exists once CNI ADD has run
	// (see needsCNIBridgeWiring in StartContainer); a resolv.conf writer
	// failure only degrades DNS (VIP literals still work via the REDIRECT rule
	// applied at start, for meshed apps), so it is logged, not fatal.
	if needsGatewayDNS(appCfg.Isolation, appCfg.Entitlements) {
		if resolvPath, err := writeMeshResolvConf(appID); err == nil {
			localoci.InjectResolvMount(spec, resolvPath)
		} else {
			c.logger.Warn("bridge/mesh: resolv.conf setup failed; container keeps image resolv.conf",
				zap.String("app_id", appID), zap.Error(err))
		}
	}

	// Apply isolation-specific namespace and shm settings for shared-namespace groups.
	if appconfig.IsSharedNamespaceIsolation(appCfg.Isolation) {
		primaryPID, hasPrimary := c.getPrimaryPID(appID)
		// The recorded primary is only trustworthy while its task is alive: a
		// primary that exited on its own or was replaced by a redeploy never
		// passes through the StopContainer path that clears the entry.
		// Joining a stale (possibly recycled) PID would fail — or worse, join
		// the wrong namespace — so verify it against a running container task
		// and promote this service to primary when stale (SOC2-CC6,
		// NIST-SC-7, ISO27001-A.8).
		if hasPrimary && !c.primaryTaskAlive(ctx, appID, primaryPID) {
			c.logger.Info("Recorded primary for app group is stale; this service becomes the new primary",
				zap.String(logfields.AppID, appID), zap.Uint32("stale_pid", primaryPID))
			c.clearPrimaryPID(appID)
			hasPrimary = false
		}
		if hasPrimary {
			// Secondary service: join the primary's namespaces.
			// nsAnchors holds open fds for each namespace so the paths embedded
			// in the spec (/proc/self/fd/{n}) remain valid until runc opens them.
			nsAnchors, err := localoci.JoinGroupNamespaces(spec, primaryPID, appCfg.Isolation)
			if err != nil {
				return fmt.Errorf("joining group namespaces: %w", err)
			}
			defer func() {
				for _, f := range nsAnchors {
					f.Close()
				}
			}()
			if appCfg.Isolation == "shared-ipc" {
				shmPath, shmErr := ensureSharedSHM(appID)
				if shmErr != nil {
					return shmErr
				}
				localoci.RemoveDefaultSHM(spec)
				spec.Mounts = append(spec.Mounts, localoci.SharedSHMMount(shmPath))
			}
		} else {
			// Primary service: mount the shared shm segment too. Creating the
			// host dir alone is not enough — without the bind mount the
			// primary keeps its private tmpfs /dev/shm and never shares
			// segments with the secondaries that mount /run/wendy/shm/<appID>.
			if appCfg.Isolation == "shared-ipc" {
				shmPath, shmErr := ensureSharedSHM(appID)
				if shmErr != nil {
					return shmErr
				}
				localoci.RemoveDefaultSHM(spec)
				spec.Mounts = append(spec.Mounts, localoci.SharedSHMMount(shmPath))
			}
		}
	}

	// Remove duplicate device nodes before handing the spec to runc: independent
	// provisioners (CDI/L4T-CSV GPU setup and the gpu entitlement) can add the
	// same node, and runc mknod()s each entry, so a duplicate path would fail
	// container creation with EEXIST.
	localoci.DedupeDevices(spec)

	// A retained namespace is injected only after the complete new runtime
	// configuration has been validated and matched against the old network
	// identity. runc will join this nsfs bind mount instead of creating a fresh
	// network namespace, so StartContainer can safely skip CNI DEL+ADD.
	if reusedNetworkSandbox != nil {
		if err := localoci.JoinNetworkNamespace(spec, reusedNetworkSandbox.path); err != nil {
			return fmt.Errorf("joining reusable network sandbox: %w", err)
		}
	}

	// SECURITY (WDY-1102): backstop against any mount whose source resolves into
	// containerd's runtime directory (the control socket is a host-escape vector).
	// Runs on the fully assembled spec — entitlement, shared-SHM, and default
	// mounts — immediately before it is handed to the runtime.
	if err := localoci.ValidateMounts(spec); err != nil {
		return err
	}

	// Serialize our custom OCI spec to JSON for WithSpecFromBytes.
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return fmt.Errorf("marshaling OCI spec: %w", err)
	}
	specDuration = time.Since(phaseStarted)

	// Prefer the fresh writable rootfs PrepareImage created concurrently with
	// upload. Immutable layer snapshots are reused either way; consuming this
	// active snapshot moves the final snapshotter Prepare call off the replace
	// critical path. A prepared snapshot is single-use and has never backed a
	// task, so runtime filesystem mutations can never leak across deployments.
	snapshotKey := SnapshotKey(appID, serviceName)
	phaseStarted = time.Now()
	snapshotOpt := containerd.WithNewSnapshot(snapshotKey, image)
	var prepared *preparedSnapshot
	if diffIDs, rootErr := image.RootFS(ctx); rootErr == nil {
		prepared = c.takePreparedSnapshot(imageName, identity.ChainID(diffIDs).String())
		if prepared != nil {
			// The lease has a bounded expiration so an abandoned preparation
			// cannot pin storage forever. If GC already collected that snapshot,
			// discard the stale bookkeeping and retain the normal synchronous
			// WithNewSnapshot fallback selected above.
			if _, statErr := c.client.SnapshotService(c.snapshotter).Stat(ctx, prepared.key); statErr != nil {
				prepared.discard()
				prepared = nil
			} else {
				snapshotKey = prepared.key
				snapshotOpt = containerd.WithSnapshot(snapshotKey)
			}
		}
	}
	_, err = c.client.NewContainer(ctx, containerName,
		containerd.WithImage(image),
		containerd.WithSnapshotter(c.snapshotter),
		snapshotOpt,
		containerd.WithContainerLabels(labels),
		containerd.WithNewSpec(
			oci.WithSpecFromBytes(specJSON),
		),
	)
	if prepared != nil {
		if err != nil {
			prepared.discard()
		} else {
			prepared.releaseLease()
		}
	}
	if err != nil {
		return fmt.Errorf("creating container %q: %w", containerName, err)
	}
	reusedNetworkSandboxCommitted = true
	snapshotDuration = time.Since(phaseStarted)

	// Container created successfully; keep its external socket resources running.
	dbusProxyStarted = false
	systemAPIRefOwned = false

	report(&agentpb.CreateContainerProgress{Phase: agentpb.CreateContainerProgress_COMPLETE})

	createdFields := []zap.Field{
		zap.String("container_name", containerName),
		zap.String(logfields.AppID, appID),
		zap.String("image", imageName),
		zap.String("version", version),
	}
	if serviceName != "" {
		createdFields = append(createdFields, zap.String("service_name", serviceName))
	}
	c.logger.Info("Container created", createdFields...)
	c.logger.Info("Container create phase timings",
		zap.String("container_name", containerName),
		zap.String("snapshotter", c.snapshotter),
		zap.Bool("replaced_existing", replacedExisting),
		zap.Bool("image_was_unpacked", unpacked),
		zap.Duration("lock_wait", lockWait),
		zap.Duration("replace_existing", replaceDuration),
		zap.Duration("resolve_image", imageResolveDuration),
		zap.Duration("unpack_image", unpackDuration),
		zap.Duration("build_spec", specDuration),
		zap.Duration("prepare_snapshot_and_create", snapshotDuration),
		zap.Duration("total", time.Since(createStarted)),
	)

	// Cache services map for stop-order resolution and isolation mode for
	// StartContainer PID tracking. c.mu is already held for the full function
	// via defer c.mu.Unlock() above — no inner lock needed.
	if len(appCfg.Services) > 0 {
		if c.appServices == nil {
			c.appServices = make(map[string]map[string]*appconfig.ServiceConfig)
		}
		c.appServices[appID] = appCfg.Services
	}
	if appCfg.Isolation != "" {
		if c.appIsolation == nil {
			c.appIsolation = make(map[string]string)
		}
		c.appIsolation[appID] = appCfg.Isolation
	}

	return nil
}

// applyNvidiaCDI loads the NVIDIA CDI spec (generated by nvidia-ctk at boot)
// and applies the driver's device nodes, library mounts, and environment
// variables to the OCI spec. This handles platform-specific paths (Orin Nano vs
// Thor, etc.).
//
// The messages below say "NVIDIA driver" rather than "GPU" because
// needsNvidiaCDI reaches here for a display entitlement as well as a gpu one:
// on a Jetson the EGL/GLES userspace a display app needs arrives through this
// same injection. A warning naming a GPU would send someone debugging a
// display-only app looking for an entitlement it never declared.
func (c *Client) applyNvidiaCDI(spec *localoci.Spec) {
	mgr := cdi.NewManager()
	cdiSpec, err := mgr.LoadNVIDIACDISpec()
	if err != nil {
		// No nvidia-ctk-generated CDI spec. On Tegra/L4T this is expected when the
		// device's nvidia-container-toolkit predates `nvidia-ctk cdi generate`
		// (JetPack 5 / r35, toolkit ≤1.11). Fall back to the NVIDIA Container
		// Runtime CSV-mode file lists, which still ship on those images and list
		// the real libcuda.so.1 plus the Tegra iGPU device nodes (WDY-1716).
		if applied, csvErr := cdi.ApplyL4TCSV(spec); csvErr != nil {
			c.logger.Warn("L4T CSV fallback failed; NVIDIA driver mounts may be incomplete", zap.Error(csvErr))
		} else if applied > 0 {
			c.logger.Info("Applied L4T CSV NVIDIA driver provisioning (no CDI spec; nvidia-ctk predates CDI)",
				zap.Int("count", applied))
			return
		}
		c.logger.Warn("No NVIDIA CDI spec and no usable L4T CSV files; NVIDIA driver library mounts may be incomplete",
			zap.Error(err))
		return
	}

	// nvidia-ctk in CSV mode generates a device named "all".
	// Try that first, then fall back to the first device in the spec.
	if err := cdi.ApplyCDIDevice(spec, cdiSpec, "all"); err == nil {
		c.logger.Info("Applied NVIDIA CDI spec")
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

func (c *Client) StartContainer(ctx context.Context, appName, postStartAgentCommand string, restartPolicy *agentpb.RestartPolicy) (<-chan services.ContainerOutput, error) {
	return c.startContainer(ctx, appName, nil, postStartAgentCommand, restartPolicy)
}

// startContainer is the single task-start implementation for both streaming
// starts and starts with attached stdin. Keeping network sandbox validation and
// lifecycle serialization here prevents AttachContainer from bypassing CNI
// CHECK or racing an ordinary start while c.mu is released for external work.
func (c *Client) startContainer(ctx context.Context, appName string, stdin io.Reader, postStartAgentCommand string, restartPolicy *agentpb.RestartPolicy) (<-chan services.ContainerOutput, error) {
	startStarted := time.Now()
	// Accept both "appID" and "appID_serviceName" forms. ParseContainerName
	// validates both components so a crafted value cannot reach the label filter
	// in the containersForApp fallback path (SOC2-CC6, ISO27001-A.8).
	appID, serviceName, err := ParseContainerName(appName)
	if err != nil {
		return nil, fmt.Errorf("StartContainer: invalid app name: %w", err)
	}
	unlockNetwork := c.lockNetworkOperation(appName)
	defer unlockNetwork()
	// Hold c.mu for the initial container/metadata snapshot. It is released for
	// sandbox health checks and CNI cleanup, then reacquired around NewTask so a
	// concurrent DeleteContainer cannot remove the container between the final
	// stop-state check and task creation (TOCTOU, SOC2-CC6).
	lockStarted := time.Now()
	c.mu.Lock()
	lockWait := time.Since(lockStarted)
	var resolveDuration, staleTaskDuration, newTaskDuration, waitDuration, runtimeStartDuration, netnsAnchorDuration time.Duration
	var cniDeleteDuration, cniAddDuration, networkFinalizeDuration time.Duration
	muHeld := true
	defer func() {
		if muHeld {
			c.mu.Unlock()
		}
	}()
	ctx = c.withNamespace(ctx)
	if c.appStopping[appID] {
		return nil, fmt.Errorf("%w: %q", errAppStopping, appID)
	}

	// Start streams one container's output, so a bare appID naming a group is
	// an error here rather than a fan-out.
	phaseStarted := time.Now()
	ctrs, _, _, err := c.resolveTargets(ctx, appName)
	if err != nil {
		return nil, fmt.Errorf("loading container %q: %w", appName, err)
	}
	if len(ctrs) > 1 {
		return nil, fmt.Errorf("app %q has multiple service containers; use the full container name (appID_serviceName) to start a specific service", appName)
	}
	container := ctrs[0]

	// Name parsing is ambiguous for multi-service containers because '_' is
	// also legal inside appIDs: "app_talker" parses as a bare appID, which
	// would key the group bookkeeping below (isolation mode, primary PID for
	// namespace joins, CNI per-service records) under the wrong identity.
	// The labels written at create time are authoritative — prefer them,
	// re-validating since labels are external state (SOC2-CC6, NIST-SI-10).
	var containerLabels map[string]string
	if labels, lerr := container.Labels(ctx); lerr == nil {
		containerLabels = labels
		if id := labels[labelKeyAppID]; id != "" && appconfig.ValidateAppID(id) == nil {
			svc := labels[labelKeyServiceName]
			if svc == "" || appconfig.ValidateServiceName(svc) == nil {
				appID, serviceName = id, svc
			}
		}
		// Defensive rehydrate: covers StartContainer calls not preceded by
		// ListBootContainers (e.g. a direct restart of a single container).
		// c.mu is already held here (muHeld), so use the lock-free core.
		c.hydrateIsolationLocked(appID, labels)
	}
	// The parsed name above can be ambiguous when app IDs contain underscores;
	// repeat the check after authoritative labels resolve the actual app ID.
	// Since StopContainer sets appStopping while holding this same mutex, a
	// start is either fully ordered before the stop snapshot or rejected here.
	if c.appStopping[appID] {
		return nil, fmt.Errorf("%w: %q", errAppStopping, appID)
	}
	isolation := c.getIsolation(appID)

	// Resources created under /run disappear across reboot even though
	// containerd preserves the container and its OCI spec. If this start fails
	// after recreating a proxy, release it again unless the complete start
	// lifecycle (including network setup) succeeds below.
	dbusProxyStartedForTask := false
	dbusProxyContainerName := container.ID()
	defer func() {
		if dbusProxyStartedForTask && c.proxyManager != nil {
			_ = c.proxyManager.Stop(dbusProxyContainerName)
		}
	}()

	// Sandbox verification can self-exec CNI CHECK, query netlink, and tear down
	// mounts/IPAM. None of that may run under the global client mutex; the keyed
	// network-operation lock above still serializes this container's lifecycle.
	muHeld = false
	c.mu.Unlock()

	// Reboot resilience for wendy-managed DNS files: host-network and meshed
	// containers bind-mount /etc/resolv.conf from tmpfs paths written once at
	// CreateContainerWithProgress time. containerd persists the container/spec
	// across a reboot but /run does not survive it, and
	// ReconcileBootContainers reaches this function directly (never
	// CreateContainer) to bring surviving containers back — so the source
	// must be recreated here, before container.NewTask below processes the
	// spec's mounts, or the runtime's bind mount fails and the container never
	// starts again. The gates are the exact persisted resolv.conf mount sources,
	// Bluetooth mount, and serial device mounts, so a Spec load failure just
	// skips the recovery hooks (NewTask will report an invalid stored spec).
	storedSpec, storedSpecErr := container.Spec(ctx)
	if storedSpecErr == nil {
		c.recreateHostResolvConfForStart(storedSpec.Mounts)
		c.recreateMeshResolvConfForStart(storedSpec.Mounts)

		started, proxyErr := c.ensureDBusProxyForStart(ctx, dbusProxyContainerName, storedSpec.Mounts)
		if proxyErr != nil {
			return nil, proxyErr
		}
		dbusProxyStartedForTask = started

		identities, identityErr := decodeSerialIdentities(containerLabels[labelKeySerialIdentities])
		if identityErr != nil {
			return nil, fmt.Errorf("loading serial device identities for %q: %w", appName, identityErr)
		}
		changed, serialErr := refreshSerialMountsForStart(storedSpec, identities)
		if serialErr != nil {
			return nil, fmt.Errorf("refreshing serial devices for %q: %w", appName, serialErr)
		}
		if changed {
			if updateErr := container.Update(ctx, withUpdatedContainerSpec(storedSpec)); updateErr != nil {
				return nil, fmt.Errorf("persisting refreshed serial devices for %q: %w", appName, updateErr)
			}
		}
	} else {
		c.logger.Warn("could not load container spec to recreate managed start resources",
			zap.String("app_name", appName), zap.Error(storedSpecErr))
	}

	// Resolve network policy before NewTask. A sandbox is reusable only when
	// both its health/identity checks pass and the stored OCI spec explicitly
	// joins its bind mount. The second condition prevents skipping CNI for a
	// legacy/private spec that would start in an unrelated fresh namespace.
	entitlements := parseEntitlementsFromAnnotations(containerLabels)
	needsBridge := needsCNIBridgeWiring(isolation, serviceName, entitlements)
	identity, retainsBridge := networkIdentityFromLabels(containerLabels)
	var reusedNetworkSandbox *networkSandbox
	if retainsBridge {
		if candidate, ok := c.reusableNetworkSandbox(ctx, appName, identity); ok &&
			runtimeSpecJoinsNetworkSandbox(storedSpec, candidate.path) {
			reusedNetworkSandbox = candidate
		} else if candidate, ok := c.recoverNetworkSandbox(ctx, container, identity, containerLabels, storedSpec); ok {
			reusedNetworkSandbox = candidate
		} else {
			c.destroyNetworkSandbox(ctx, appName)
			c.purgePersistedNetworkSandbox(ctx, container, containerLabels, storedSpec)
			if containerLabels[labelKeyNetworkSandboxIP] != "" || runtimeSpecHasNetworkNamespacePath(storedSpec) {
				if err := c.persistNetworkNamespace(ctx, container, "", identity, ""); err != nil {
					return nil, fmt.Errorf("clearing stale reusable network namespace for %q: %w", appName, err)
				}
			}
		}
	} else {
		// Host/shared namespaces are never eligible, even if stale process
		// memory claims a sandbox exists for this name.
		c.destroyNetworkSandbox(ctx, appName)
		c.purgePersistedNetworkSandbox(ctx, container, containerLabels, storedSpec)
		if containerLabels[labelKeyNetworkSandboxIP] != "" || runtimeSpecHasNetworkNamespacePath(storedSpec) {
			if err := c.persistNetworkNamespace(ctx, container, "", "", ""); err != nil {
				return nil, fmt.Errorf("clearing ineligible reusable network namespace for %q: %w", appName, err)
			}
		}
	}

	// Re-enter the narrow task-creation critical section after all potentially
	// slow sandbox validation and cleanup. StopContainer publishes appStopping
	// under this mutex, so a stop that won the unlocked interval fails closed.
	c.mu.Lock()
	muHeld = true
	if c.appStopping[appID] {
		return nil, fmt.Errorf("%w: %q", errAppStopping, appID)
	}

	if restartPolicy != nil {
		if err := c.applyRestartPolicyLabel(ctx, container, restartPolicy); err != nil {
			return nil, fmt.Errorf("updating restart policy for %q: %w", appName, err)
		}
	}
	resolveDuration = time.Since(phaseStarted)

	// Clean up any stale task from a previous run.
	phaseStarted = time.Now()
	c.deleteStaleTask(ctx, container, appName)
	staleTaskDuration = time.Since(phaseStarted)

	// The task's IO pipeline must live as long as the task, not the RPC that
	// started it: containerd's fifo package closes the FIFO fds when the
	// creation context is canceled. Bound to the request context, a client
	// disconnect (e.g. `wendy run --detach` returning) would orphan the
	// container's stdout — the FIFO fills and the app blocks on its next
	// write, freezing it minutes to hours later.
	taskCtx := c.withNamespace(context.Background())

	// Create pipes for stdout/stderr capture.
	stdoutR, stdoutW := io.Pipe()
	stderrR, stderrW := io.Pipe()

	// Create a new task with pipe-based stdio for programmatic capture.
	phaseStarted = time.Now()
	task, err := container.NewTask(taskCtx, cio.NewCreator(cio.WithStreams(stdin, stdoutW, stderrW)))
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
					task, err = container.NewTask(taskCtx, cio.NewCreator(cio.WithStreams(stdin, stdoutW, stderrW)))
				}
			}
		}
		if err != nil {
			stdoutR.Close()
			stdoutW.Close()
			stderrR.Close()
			stderrW.Close()
			c.recordStartFailure(ctx, appName, err)
			return nil, fmt.Errorf("creating task for %q: %w", appName, err)
		}
	}
	newTaskDuration = time.Since(phaseStarted)

	// Set up the wait channel before starting. Uses taskCtx so the exit
	// monitor — and with it the output pipeline — survives client disconnect.
	phaseStarted = time.Now()
	exitStatusCh, err := task.Wait(taskCtx)
	if err != nil {
		_, _ = task.Delete(taskCtx)
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		c.recordStartFailure(ctx, appName, err)
		return nil, fmt.Errorf("waiting on task for %q: %w", appName, err)
	}
	waitDuration = time.Since(phaseStarted)

	// Start the task.
	phaseStarted = time.Now()
	if err := task.Start(taskCtx); err != nil {
		_, _ = task.Delete(taskCtx)
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		c.recordStartFailure(ctx, appName, err)
		return nil, fmt.Errorf("starting task for %q: %w", appName, err)
	}
	runtimeStartDuration = time.Since(phaseStarted)

	// From this point through network setup, every failure must kill the task,
	// close its unconsumed pipes, and record a did-not-start diagnostic. The
	// post-start hook is intentionally delayed until network setup commits.
	failStartedTask := func(cause error) error {
		_, _ = task.Delete(taskCtx, containerd.WithProcessKill)
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		c.recordStartFailure(ctx, appName, cause)
		return cause
	}

	// Track the primary PID for shared-namespace app groups.
	// getIsolation requires c.mu (held here via muHeld).
	if appconfig.IsSharedNamespaceIsolation(isolation) {
		// Presence alone is not enough: a per-service stop leaves the group
		// entry behind, so a dead PID must be replaced rather than kept.
		primaryPID, hasPrimary := c.getPrimaryPID(appID)
		if !hasPrimary || !c.primaryTaskAlive(ctx, appID, primaryPID) {
			c.setPrimaryPID(appID, task.Pid())
		}
	}

	// Entitlements are read back from the container's own labels (written at
	// create time via wendyLabels/BuildEntitlementAnnotations) rather than
	// threaded through as a parameter, since StartContainer is only given an
	// app/container name by its callers. Needed up front (before the netns
	// anchor below), because needsCNIBridgeWiring must also recognise a
	// single-service "bridge"-mode app, which isolation+serviceName alone
	// cannot detect (unlike a multi-service isolated app service).
	phaseStarted = time.Now()
	// Anchor the network namespace with an open fd BEFORE releasing the mutex.
	// This eliminates the TOCTOU race where a concurrent StopContainer could
	// recycle the PID between mutex release and CNI ADD, causing the plugin to
	// operate on the wrong process's netns (SOC2-CC6, NIST-SC-7, ISO27001-A.8).
	var netnsRef *os.File
	if needsBridge && reusedNetworkSandbox == nil {
		nsPath := fmt.Sprintf("/proc/%d/ns/net", task.Pid())
		netnsRef, err = os.Open(nsPath)
		if err != nil {
			return nil, failStartedTask(fmt.Errorf("anchoring network namespace for app %q: %w", appID, err))
		}
	}
	netnsAnchored := netnsRef != nil
	netnsAnchorDuration = time.Since(phaseStarted)

	// Release the mutex before launching the streaming goroutine, which does
	// not need it (it only reads from pipes).
	muHeld = false
	c.mu.Unlock()

	// CNI ADD for containers that need bridge wiring — see needsCNIBridgeWiring
	// for the two cases this covers: multi-service isolated app services
	// (assign IP, then update /etc/hosts below) and single-service
	// "bridge"-mode apps (assign IP for NAT egress only; no /etc/hosts, since
	// there are no sibling services to resolve). Both share this block rather
	// than duplicating it (specs/2026-07-05-network-bridge-default-design.md).
	// bindNetnsForCNI creates a stable bind-mount under /run/wendy/netns/ so
	// CNI_NETNS is a real filesystem path as required by the CNI spec — not a
	// /proc/self/fd/<n> reference that third-party CNI plugins may not honour.
	// It also closes the fd (the bind-mount anchors the namespace independently).
	// On Linux the bind-mount is used; on other platforms the fd path is the fallback.
	if reusedNetworkSandbox != nil {
		// Mandatory for both in-memory reuse and restart recovery: neither path
		// may skip this post-start task<->namespace binding check.
		if !taskUsesNetworkSandbox(reusedNetworkSandbox.path, task.Pid()) {
			c.destroyNetworkSandbox(ctx, appName)
			return nil, failStartedTask(fmt.Errorf("reusable network sandbox validation failed for app %q after task start", appID))
		}
		c.logger.Info("Reused CNI network sandbox",
			zap.String(logfields.AppID, appID),
			zap.String("container_id", appName),
			zap.String("ip", reusedNetworkSandbox.ip))
	} else if needsBridge && netnsRef != nil {
		netnsPath, cleanupNetns, persistentNetns := bindNetnsForCNI(appName, netnsRef)
		bridgeDNSHealthy := !retainsBridge
		// Release any stale host-local IPAM reservation for this container ID
		// before ADD (WDY-1834). The CNI allocation is keyed by container ID
		// (== appName, which is stable across restart/redeploy), and
		// deleteStaleTask above removes the old containerd task but NOT the CNI
		// reservation. A previous teardown can be skipped entirely — the health
		// monitor's restartSingle path calls StartContainer with no intervening
		// stopOne/CNIDel — or fail (a bridge-plugin DEL error leaves the
		// host-local allocation behind). Either way the next ADD for the same
		// container ID collides with "duplicate allocation is not allowed",
		// leaving the container with no mesh netns: its resolv.conf gateway is
		// unreachable (DNS "Temporary failure in name resolution") and the
		// ingress DNAT points at an IP nothing answers on ("no route to host").
		// CNIDel is idempotent and best-effort — a no-op on a clean first deploy
		// (the fresh netns has no eth0 to remove and host-local has no
		// reservation for this ID) — so a pre-ADD DEL makes ADD collision-proof
		// no matter how the previous instance was (or wasn't) torn down.
		phaseStarted = time.Now()
		if delErr := c.CNIDel(ctx, appID, appName, netnsPath); delErr != nil {
			c.logger.Warn("CNI DEL before ADD (stale-allocation reclaim) failed (non-fatal)",
				zap.String(logfields.AppID, appID), zap.Error(delErr))
		}
		cniDeleteDuration = time.Since(phaseStarted)
		phaseStarted = time.Now()
		ip, cniResult, cniErr := c.CNIAdd(ctx, appID, appName, netnsPath)
		cniAddDuration = time.Since(phaseStarted)
		if cniErr != nil {
			// Roll back any partial state the failed ADD left behind (e.g. a
			// host-local IPAM allocation recorded before a later step, such as
			// installing iptables rules, failed) — without this, the IPAM
			// allocation for this exact container ID leaks and every future
			// CNI ADD for the same appID/serviceName fails with "duplicate
			// allocation is not allowed" until an agent restart or a renamed
			// appID sidesteps it (found via repeated RemoteCam redeploy cycles).
			// Best-effort: DEL is itself idempotent/safe against a partial or
			// absent ADD, so a DEL failure here is logged, not fatal.
			if delErr := c.CNIDel(ctx, appID, appName, netnsPath); delErr != nil {
				c.logger.Warn("CNI DEL rollback after failed ADD also failed (non-fatal)",
					zap.String(logfields.AppID, appID), zap.Error(delErr))
			}
			cleanupNetns()
			c.logger.Error("CNI ADD failed", zap.String(logfields.AppID, appID), zap.Error(cniErr))
			return nil, failStartedTask(fmt.Errorf("CNI ADD failed for app %q: %w", appID, cniErr))
		} else {
			phaseStarted = time.Now()
			// netnsPath (the bind-mount) must stay mounted through mesh egress
			// setup below, which needs a live netns to install the service-CIDR
			// route via nsenter (SetMeshRoute). Until every subsequent setup step
			// succeeds, a deferred rollback releases both IPAM and the bind mount.
			networkReady := false
			keepNetns := false
			defer func() {
				if !networkReady {
					_ = c.CNIDel(context.WithoutCancel(ctx), appID, appName, netnsPath)
					if needsGatewayDNS(isolation, entitlements) {
						c.releaseMeshDNS(appName, appID)
					}
					c.teardownMeshEgress(entitlements, appName, appID, ip)
					cleanupNetns()
				} else if !keepNetns {
					cleanupNetns()
				}
			}()

			// /etc/hosts bookkeeping is specific to multi-service isolated app
			// groups (sibling services resolve each other by name); a
			// single-service bridge-mode app has no siblings, so serviceName is
			// empty and this whole block — including the concurrent-stop
			// discard guard below, which is keyed on the isolated-group cache
			// c.appIsolation and would misfire for a bridge app that never sets
			// it — is skipped entirely for bridge mode. This is an accepted
			// gap, not an oversight: a concurrent StopContainer racing a
			// bridge-mode app's CNI ADD is left to self-heal via the CNI DEL
			// issued at stop time (stopOne/deleteOne, gated by
			// needsCNIBridgeWiring), instead of duplicating this guard for
			// serviceName == "".
			if serviceName != "" {
				c.mu.Lock()
				// Guard against a concurrent StopContainer that may have deleted
				// c.appIsolation[appID] during the window between CNI ADD and this
				// re-lock. If the app is already gone, discard the IP silently rather
				// than writing stale state (SOC2-CC6, NIST-SI-16, ISO27001-A.8).
				if c.appIsolation[appID] == "" {
					c.mu.Unlock()
					c.logger.Warn("CNI ADD: app already stopped before IP could be recorded, discarding IP",
						zap.String(logfields.AppID, appID), zap.String("ip", ip))
					return nil, failStartedTask(fmt.Errorf("app %q stopped during CNI ADD; container not started", appID))
				}
				c.recordServiceIP(appID, serviceName, ip)
				hostsPath, pathErr := safeJoin("/run/wendy/hosts", appID)
				if pathErr != nil {
					// Hard error: a validated appID must never produce an unsafe path.
					// Remove the just-recorded IP so it cannot pollute future writeHostsFile
					// calls for the same appID (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
					if c.serviceIPs != nil {
						delete(c.serviceIPs[appID], serviceName)
					}
					c.logger.Error("security: appID produces unsafe hosts path",
						zap.String(logfields.AppID, appID), zap.Error(pathErr))
					c.mu.Unlock()
					return nil, failStartedTask(fmt.Errorf("security: appID %q produces unsafe hosts path: %w", appID, pathErr))
				}
				_ = writeHostsFile(hostsPath, c.serviceIPs[appID])
				c.mu.Unlock()
			} else if _, ok := findMeshEntitlement(entitlements); ok {
				// Single-service mesh app (serviceName == ""): record the
				// CNI-assigned IP under the empty service key so
				// teardownMeshEgress (stopOne/deleteOne) can find it to remove
				// this container's ingress port-forward. No sibling services
				// means no /etc/hosts and no isolated-group guard (WDY-1853).
				c.mu.Lock()
				c.recordServiceIP(appID, serviceName, ip)
				c.mu.Unlock()
			}

			// Gateway DNS listener for single-service bridge-mode apps only
			// (see needsGatewayDNS, findBridgeEntitlement). Meshed multi-service
			// isolated apps are intentionally excluded here: applyMeshEgress
			// below acquires their DNS listener itself, as its last fallible
			// step, only after route/rule/redirect setup has already
			// succeeded (see applyMeshEgress). Acquiring it here too, before
			// applyMeshEgress runs, would hold a DNS-listener ref across a
			// window where a later applyMeshEgress failure returns without
			// ever releasing it (the failure path below only deletes the
			// task) — a refcount leak. ensureMeshDNS is idempotent per
			// container so this is not about avoiding a double-acquire; it is
			// about keeping the mesh acquire ordered strictly after mesh
			// egress's fallible steps, as applyMeshEgress's own comment
			// requires.
			//
			// Best-effort — without it, DNS lookups fail inside the namespace
			// but the container still starts (NAT egress by IP literal still
			// works), mirroring the rest of this block's error handling.
			if _, ok := findBridgeEntitlement(entitlements); ok {
				if gw, gwErr := meshGateway(appID); gwErr == nil {
					bridgeDNSHealthy = c.ensureMeshDNS(appName, gw)
				} else {
					c.logger.Warn("bridge: could not derive gateway for DNS listener",
						zap.String(logfields.AppID, appID), zap.Error(gwErr))
				}
			}

			// Mesh egress (fail-closed): a container with the network/mesh
			// entitlement must never start believing it has mesh egress it
			// does not actually have. applyMeshEgress is a complete no-op for
			// apps without that entitlement (including bridge-mode apps).
			if meshErr := c.applyMeshEgress(entitlements, appName, appID, netnsPath, ip); meshErr != nil {
				c.logger.Error("mesh egress setup failed; failing container start",
					zap.String("app_id", appID), zap.Error(meshErr))
				return nil, failStartedTask(fmt.Errorf("mesh egress setup failed for app %q: %w", appID, meshErr))
			}

			// All network configuration is now complete. Transfer ownership to the
			// client map and persist the namespace path into container metadata so
			// future NewTask calls join it without another CNI DEL+ADD cycle.
			c.mu.Lock()
			if c.appStopping[appID] {
				c.mu.Unlock()
				return nil, failStartedTask(fmt.Errorf("app %q stopped during network sandbox setup; container not started", appID))
			}
			if retainsBridge && bridgeDNSHealthy && persistentNetns && identity != "" {
				if err := writeNetworkSandboxResult(appName, cniResult); err == nil {
					if err := c.persistNetworkNamespace(ctx, container, netnsPath, identity, ip); err == nil {
						c.registerNetworkSandbox(&networkSandbox{
							appID: appID, serviceName: serviceName, containerID: appName,
							identity: identity, path: netnsPath, ip: ip, result: cniResult,
							isolation: isolation, entitlements: append([]appconfig.Entitlement(nil), entitlements...), cleanup: cleanupNetns,
						})
						keepNetns = true
					} else {
						_ = os.Remove(networkSandboxResultPath(appName))
						c.logger.Warn("Could not persist reusable network namespace", zap.String("container_id", appName), zap.Error(err))
					}
				} else {
					c.logger.Warn("Could not persist reusable CNI result", zap.String("container_id", appName), zap.Error(err))
				}
			}
			networkReady = true
			c.mu.Unlock()
			networkFinalizeDuration = time.Since(phaseStarted)
		}
	}

	// The fully configured Bluetooth task now owns the proxy until stop/delete.
	// Do not let the failure-only defer above tear it down on the successful path.
	dbusProxyStartedForTask = false
	c.logger.Info("Container started", zap.String("app_name", appName))
	c.startPostStartAgentHook(postStartAgentCommand, appName)

	c.logger.Info("Container start phase timings",
		zap.String("container_name", appName),
		zap.Bool("cni_bridge", needsBridge),
		zap.Bool("netns_anchored", netnsAnchored),
		zap.Duration("lock_wait", lockWait),
		zap.Duration("resolve_container", resolveDuration),
		zap.Duration("delete_stale_task", staleTaskDuration),
		zap.Duration("new_task", newTaskDuration),
		zap.Duration("register_wait", waitDuration),
		zap.Duration("runtime_start", runtimeStartDuration),
		zap.Duration("anchor_netns", netnsAnchorDuration),
		zap.Duration("cni_delete", cniDeleteDuration),
		zap.Duration("cni_add", cniAddDuration),
		zap.Duration("network_finalize", networkFinalizeDuration),
		zap.Duration("total", time.Since(startStarted)),
	)

	// Stream output from the pipes.
	outputCh := make(chan services.ContainerOutput, 64)
	go c.streamOutput(taskCtx, task, exitStatusCh, outputCh, appName, stdoutR, stderrR, stdoutW, stderrW)

	// Recompute camera-loopback nodes/consumers from truth now that this
	// container is running: it may have just become an entitled consumer.
	// context.WithoutCancel because ctx belongs to this RPC and must not be
	// canceled by the caller returning before the sync goroutine runs; `go`
	// because a loopback problem must never delay or fail container start
	// (see SyncCameraLoopbacks).
	go c.SyncCameraLoopbacks(context.WithoutCancel(ctx))

	return outputCh, nil
}

// StartContainerWithStdin uses the same validated task-start lifecycle as
// StartContainer and only changes the task's stdin stream.
func (c *Client) StartContainerWithStdin(ctx context.Context, appName string, stdin io.Reader, postStartAgentCommand string, restartPolicy *agentpb.RestartPolicy) (<-chan services.ContainerOutput, error) {
	return c.startContainer(ctx, appName, stdin, postStartAgentCommand, restartPolicy)
}

// execCounter disambiguates concurrent exec IDs within the agent process.
var execCounter atomic.Uint64

// Compile-time guarantee that *Client satisfies the optional exec capability the
// ExecContainer RPC type-asserts for (the assertion there is only runtime).
var _ services.ContainerExecer = (*Client)(nil)

// runningContainerForApp resolves the single container for appName, mirroring
// the lookup in StartContainerWithStdin: LoadContainer by name, then the
// app-label fallback (rejecting ambiguous multi-service apps).
func (c *Client) runningContainerForApp(ctx context.Context, appName string) (containerd.Container, error) {
	container, err := c.client.LoadContainer(ctx, appName)
	if err != nil {
		ctrs, labelErr := c.containersForApp(ctx, appName)
		if labelErr != nil || len(ctrs) == 0 {
			return nil, fmt.Errorf("loading container %q: %w", appName, err)
		}
		if len(ctrs) > 1 {
			return nil, fmt.Errorf("app %q has multiple service containers; use the full container name (appID_serviceName) to exec into a specific service", appName)
		}
		container = ctrs[0]
	}
	return container, nil
}

// ExecInContainer runs command inside the named app's running container,
// docker `exec -it` style. When tty is true a PTY is allocated (stderr is
// merged into stdout) and resize events ([rows, cols]) are applied to the
// process. Returns the process exit code. Implements services.ContainerExecer.
func (c *Client) ExecInContainer(ctx context.Context, appName string, command []string, tty bool, stdin io.Reader, stdout, stderr io.Writer, resize <-chan [2]uint32) (int, error) {
	if _, _, err := ParseContainerName(appName); err != nil {
		return -1, fmt.Errorf("ExecInContainer: invalid app name: %w", err)
	}
	if len(command) == 0 {
		return -1, fmt.Errorf("ExecInContainer: empty command")
	}
	ctx = c.withNamespace(ctx)

	container, err := c.runningContainerForApp(ctx, appName)
	if err != nil {
		return -1, err
	}
	task, err := container.Task(ctx, nil)
	if err != nil {
		return -1, fmt.Errorf("container %q not running: %w", appName, err)
	}
	spec, err := container.Spec(ctx)
	if err != nil {
		return -1, fmt.Errorf("reading container spec for %q: %w", appName, err)
	}
	pspec := spec.Process
	pspec.Terminal = tty
	pspec.Args = command
	// Defensive copy of Env before use (mirrors the ROS 2 exec path) so we never
	// mutate the slice backing the cached container spec.
	pspec.Env = append([]string(nil), pspec.Env...)

	var ioCreator cio.Creator
	if tty {
		// With a terminal, stdout/stderr are multiplexed onto the PTY master, so
		// stderr is left nil (containerd ignores it in terminal mode).
		ioCreator = cio.NewCreator(cio.WithStreams(stdin, stdout, nil), cio.WithTerminal)
	} else {
		ioCreator = cio.NewCreator(cio.WithStreams(stdin, stdout, stderr))
	}

	execID := fmt.Sprintf("exec-%d-%d", time.Now().UnixNano(), execCounter.Add(1))
	proc, err := task.Exec(ctx, execID, pspec, ioCreator)
	if err != nil {
		return -1, fmt.Errorf("exec in container %q: %w", appName, err)
	}
	defer func() { _, _ = proc.Delete(ctx, containerd.WithProcessKill) }()

	statusC, err := proc.Wait(ctx)
	if err != nil {
		return -1, fmt.Errorf("waiting on exec: %w", err)
	}
	if err := proc.Start(ctx); err != nil {
		return -1, fmt.Errorf("starting exec: %w", err)
	}

	if tty && resize != nil {
		// proc.Resize takes (width=cols, height=rows). The initial size was sent
		// by the handler as the first resize frame.
		go func() {
			for sz := range resize {
				_ = proc.Resize(ctx, sz[1], sz[0])
			}
		}()
	}

	select {
	case st := <-statusC:
		// Block until containerd's stdout/stderr copy goroutines have drained so
		// the caller can send its final frame (e.g. an exit code) only after the
		// last output byte, not racing in-flight output.
		if pio := proc.IO(); pio != nil {
			pio.Wait()
		}
		return int(st.ExitCode()), st.Error()
	case <-ctx.Done():
		_ = proc.Kill(ctx, syscall.SIGKILL)
		<-statusC
		return -1, ctx.Err()
	}
}

var deviceHostnameWithSuffix = func() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return ""
	}
	return h + ".local"
}

// fallbackContainerEnv holds generic default env entries that are injected
// only when neither the image config nor the caller defines the same key.
// Unlike the WENDY_* identity vars (which are appended last so they always
// win), these are plain fallbacks: appending them unconditionally after the
// image env would silently clobber image-set values under OCI last-one-wins
// semantics — e.g. CUDA images ship PATH=/usr/local/cuda/bin:… which a
// trailing generic PATH discarded (WDY-1825).
var fallbackContainerEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"TERM=xterm",
}

// appendFallbackEnv appends each fallbackContainerEnv entry to env unless env
// already defines the same key. It must be called after the image env and the
// caller-supplied env have been merged.
func appendFallbackEnv(env []string) []string {
	for _, def := range fallbackContainerEnv {
		key, _, _ := strings.Cut(def, "=")
		if !envHasKey(env, key) {
			env = append(env, def)
		}
	}
	return env
}

// envHasKey reports whether env contains an entry that sets key (an exact
// "KEY=" prefix match).
func envHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return true
		}
	}
	return false
}

// buildContainerBaseEnv builds the Wendy identity environment variables for a
// container. These are appended after the image/user env so they always win
// (last KEY wins in OCI semantics); generic defaults like PATH and TERM live
// in fallbackContainerEnv instead so they never clobber image values.
//
// Precondition: appID must pass ValidateAppID and serviceName (when non-empty)
// must pass ValidateServiceName. CreateContainerWithProgress enforces this at
// its entry point; callers that bypass it are responsible for their own check.
//
// For single-container apps (serviceName == ""):
//   - WENDY_HOSTNAME is set to the device hostname (e.g. "device.local").
//
// For multi-service apps (serviceName != ""):
//   - WENDY_HOSTNAME is set to "{serviceName}.local" so each service has a
//     distinct hostname identity.
//   - WENDY_APP_GROUP is set to appID so the service can discover its siblings.
func buildContainerBaseEnv(appID, serviceName string) ([]string, error) {
	// Defence-in-depth: reject non-empty inputs that fail validation at the
	// injection site so callers can't accidentally inject control characters
	// into OCI env vars (SOC2-CC6, ISO27001-A.8, NIST-SI-10). Empty values are
	// allowed; they simply skip the corresponding env var (see guards below).
	if appID != "" {
		if err := appconfig.ValidateAppID(appID); err != nil {
			return nil, fmt.Errorf("buildContainerBaseEnv: invalid appID: %w", err)
		}
		// Explicit fast-fail: ValidateAppID's regex rejects these, but guard
		// explicitly at the concatenation site as well.
		if strings.ContainsAny(appID, "\x00\n\r=\t") {
			return nil, fmt.Errorf("buildContainerBaseEnv: appID contains forbidden characters")
		}
	}
	if serviceName != "" {
		if err := appconfig.ValidateServiceName(serviceName); err != nil {
			return nil, fmt.Errorf("buildContainerBaseEnv: invalid serviceName: %w", err)
		}
		if strings.ContainsAny(serviceName, "\x00\n\r=\t") {
			return nil, fmt.Errorf("buildContainerBaseEnv: serviceName contains forbidden characters")
		}
	}

	var env []string
	deviceHost := deviceHostnameWithSuffix()
	if serviceName != "" {
		// Multi-service: hostname is the service name, not the device hostname.
		env = append(env, "WENDY_HOSTNAME="+serviceName+".local")
		env = append(env, "WENDY_APP_GROUP="+appID)
	} else {
		if deviceHost != "" {
			env = append(env, "WENDY_HOSTNAME="+deviceHost)
		}
	}
	// WENDY_DEVICE_HOSTNAME is the mDNS hostname of the host device, available
	// in both single- and multi-service containers so workloads can always reach
	// the device regardless of what WENDY_HOSTNAME is set to.
	if deviceHost != "" {
		env = append(env, "WENDY_DEVICE_HOSTNAME="+deviceHost)
	}
	// WENDY_APP_ID is injected unconditionally (all network modes) so app code
	// can always read its own identity. The OTel identity vars are injected only
	// under host networking (in injectOTELEnvIfNeeded) because the OTLP receiver
	// is only reachable in that mode.
	if appID != "" {
		env = append(env, "WENDY_APP_ID="+appID)
	}
	return env, nil
}

// validateUserEnv rejects caller-supplied env entries that contain characters
// which could break the OCI env format or enable injection attacks.
// Mirrors the defence-in-depth checks in buildContainerBaseEnv (SOC2-CC6, NIST-SI-10).

// posixEnvKeyPattern is an allowlist for POSIX-compliant environment variable
// names. It accepts only ASCII letters, digits, and underscores, with an
// underscore or letter as the first character. This allowlist prevents leading-
// whitespace bypass (e.g. " LD_PRELOAD") and eliminates Unicode case-folding
// ambiguity before the denylist check below (SOC2-CC6, NIST-SI-10).
var posixEnvKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// blockedEnvPrefixes is the set of key prefixes that user-supplied env vars
// must not use. These keys affect dynamic linker behavior (LD_*) or are
// reserved by Wendy (WENDY_*); a compromised or malicious caller could use
// them to preload arbitrary code or override Wendy internals
// (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
var blockedEnvPrefixes = []string{
	"LD_",    // LD_PRELOAD, LD_LIBRARY_PATH, LD_AUDIT, LD_DEBUG, etc.
	"DYLD_",  // macOS dynamic linker (defense-in-depth for cross-platform images)
	"WENDY_", // Wendy-internal variables must not be overrideable by callers
}

// maxUserEnvEntries is the maximum number of caller-supplied env entries accepted.
// Prevents OCI spec bloat / DoS via unbounded env injection (SOC2-CC6, NIST-SI-10).
const maxUserEnvEntries = 512

// maxUserEnvEntryLen is the maximum byte length of a single KEY=VALUE entry.
// 32 KB covers all practical use cases while bounding spec-JSON size (SOC2-CC6, NIST-SI-10).
const maxUserEnvEntryLen = 32 * 1024

func validateUserEnv(entries []string) error {
	if len(entries) > maxUserEnvEntries {
		return fmt.Errorf("too many env entries: %d exceeds limit of %d (SOC2-CC6, NIST-SI-10)", len(entries), maxUserEnvEntries)
	}
	for _, kv := range entries {
		if len(kv) > maxUserEnvEntryLen {
			return fmt.Errorf("env entry exceeds maximum length of %d bytes (SOC2-CC6, NIST-SI-10)", maxUserEnvEntryLen)
		}
		if strings.ContainsAny(kv, "\x00\n\r") {
			return fmt.Errorf("env entry contains forbidden control character: %q", sanitizeForLog(kv, 80))
		}
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("env entry missing '=' separator: %q", sanitizeForLog(kv, 80))
		}
		// Reject keys that do not conform to the POSIX env key format. This also
		// closes the leading-whitespace bypass (" LD_PRELOAD") and the Unicode
		// case-folding bypass that strings.ToUpper alone cannot prevent.
		if !posixEnvKeyPattern.MatchString(key) {
			return fmt.Errorf("env key %q is not a valid POSIX environment variable name (SOC2-CC6, NIST-SI-10)", sanitizeForLog(key, 80))
		}
		upper := strings.ToUpper(key) // safe: key is ASCII-only after pattern check
		for _, prefix := range blockedEnvPrefixes {
			if strings.HasPrefix(upper, prefix) {
				return fmt.Errorf("env key %q is reserved and cannot be set by callers (SOC2-CC6, NIST-SI-10)", key)
			}
		}
	}
	return nil
}

// cycloneDDSInlineConfig is the CycloneDDS configuration passed inline via
// CYCLONEDDS_URI (not a file mount). SharedMemory (iceoryx zero-copy) is
// DISABLED: it requires an iox-roudi daemon that WendyOS does not run, and
// enabling it makes CycloneDDS block at startup ("RouDi not found - waiting")
// until the container is SIGKILLed, restart-looping. With it off, CycloneDDS
// uses UDP. ROS_LOCALHOST_ONLY selects loopback for app-scoped discovery; for
// host-scoped discovery CycloneDDS selects a device interface automatically.
// No <Interfaces> block is supplied so the same inline config works for both
// scopes. Re-enabling zero-copy needs an iox-roudi system service on the device
// first (WDY-884).
const cycloneDDSInlineConfig = `<CycloneDDS><Domain><SharedMemory><Enable>false</Enable></SharedMemory></Domain></CycloneDDS>`

// buildROS2Env returns ROS2 environment variables for the container resolved
// from the app's frameworks.ros2 config (group-level, overridden by the
// service-level config for multi-service apps). The injected set is
// ROS_DOMAIN_ID, RMW_IMPLEMENTATION, CYCLONEDDS_URI (CycloneDDS only), and both
// discovery-scope variables (WDY-884).
//
// Returns an error rather than nil for an invalid config. It used to return nil,
// which meant the container started with *no* ROS 2 environment at all — so
// ROS_DOMAIN_ID fell back to 0, the global default domain everything else is on.
// A validation gap upstream therefore produced maximum exposure instead of a
// failure, which is the wrong direction for a mechanism whose job is isolation.
func buildROS2Env(appCfg *appconfig.AppConfig, appID, serviceName string) ([]string, error) {
	ros2 := appCfg.ResolveROS2ConfigForService(serviceName)
	if ros2 == nil {
		return nil, nil
	}
	domainID := ros2.ResolvedDomainID(appID)
	if domainID < 0 {
		return nil, fmt.Errorf("frameworks.ros2.domainId is outside [%d,%d]; refusing to start "+
			"the container without ROS_DOMAIN_ID isolation (it would default to domain 0)",
			appconfig.ROS2DomainIDMin, appconfig.ROS2DomainIDMax)
	}
	discoveryScope := ros2.ResolvedDiscoveryScope()
	if discoveryScope == "" {
		return nil, fmt.Errorf("frameworks.ros2.discoveryScope %q is not %q or %q; refusing to "+
			"start the container with an undefined DDS discovery scope",
			ros2.DiscoveryScope, appconfig.ROS2DiscoveryScopeApp, appconfig.ROS2DiscoveryScopeHost)
	}
	env := []string{fmt.Sprintf("ROS_DOMAIN_ID=%d", domainID)}
	// ResolvedRMW validates against a fixed allowlist and returns "" for
	// unknown values, so arbitrary wendy.json strings can never reach the
	// container environment (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
	if rmw := ros2.ResolvedRMW(); rmw != "" {
		env = append(env, "RMW_IMPLEMENTATION="+rmw)
		if rmw == appconfig.ROS2DefaultRMW {
			env = append(env, "CYCLONEDDS_URI="+cycloneDDSInlineConfig)
		}
	}
	// Services are app-local by default. Host-scoped tools such as Foxglove
	// explicitly opt in to discovering ROS 2 participants on the device network.
	//
	// Both variables are set, not just ROS_LOCALHOST_ONLY. That one has been
	// deprecated since Iron in favour of ROS_AUTOMATIC_DISCOVERY_RANGE, and
	// ROS2Config.Distro explicitly advertises "jazzy" — so on the newer distros we
	// claim to support, the sole mechanism enforcing app-local isolation was at
	// best deprecated. For an app carrying `network: host` it is the only thing
	// between that app and every other DDS participant on the device.
	if discoveryScope == appconfig.ROS2DiscoveryScopeHost {
		env = append(env, "ROS_LOCALHOST_ONLY=0", "ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET")
	} else {
		env = append(env, "ROS_LOCALHOST_ONLY=1", "ROS_AUTOMATIC_DISCOVERY_RANGE=LOCALHOST")
	}
	return env, nil
}

// injectOTELEnvIfNeeded appends OTEL exporter env vars to env when host
// networking is in effect and the endpoint is not already configured. Besides
// the endpoint and protocol, it sets OTEL_SERVICE_NAME to the single-container
// app ID or multi-service container name, and OTEL_RESOURCE_ATTRIBUTES
// (wendy.app.name) to the owning app ID. This keeps services distinguishable
// while allowing `wendy device logs --app <id>` to select the whole app. It
// must be called after the image env has been merged so that image-set values
// take precedence.
//
// appID is passed explicitly (rather than read from appCfg.AppID) so the
// caller's AppConfig struct is never mutated, which would affect concurrent or
// retry uses of the same pointer.
func injectOTELEnvIfNeeded(env []string, appCfg *appconfig.AppConfig, appID string) []string {
	if !hasHostNetworkEntitlement(appCfg) {
		return env
	}
	hasEndpoint, hasProtocol := false, false
	hasServiceName, hasResourceAttrs := false, false
	for _, e := range env {
		switch {
		case strings.HasPrefix(e, "OTEL_EXPORTER_OTLP_ENDPOINT="):
			hasEndpoint = true
		case strings.HasPrefix(e, "OTEL_EXPORTER_OTLP_PROTOCOL="):
			hasProtocol = true
		case strings.HasPrefix(e, "OTEL_SERVICE_NAME="):
			hasServiceName = true
		case strings.HasPrefix(e, "OTEL_RESOURCE_ATTRIBUTES="):
			hasResourceAttrs = true
		}
	}
	// Endpoint/protocol: only point the exporter at our receiver when the image
	// hasn't already configured one.
	if !hasEndpoint {
		otelPort := os.Getenv("WENDY_OTEL_PORT")
		if otelPort == "" {
			otelPort = "4317"
		}
		if p, err := strconv.Atoi(otelPort); err != nil || p < 1 || p > 65535 {
			otelPort = "4317"
		}
		env = append(env, "OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:"+otelPort)
		if !hasProtocol {
			env = append(env, "OTEL_EXPORTER_OTLP_PROTOCOL=grpc")
		}
	}
	// Identity: set regardless of where the exporter points, so `wendy device
	// logs --app <id>` can match even when the image preset its own endpoint.
	// Image-set values still take precedence.
	if appID != "" {
		if !hasServiceName {
			serviceName := appID
			if appCfg.ServiceName != "" {
				serviceName = ContainerName(appID, appCfg.ServiceName)
			}
			env = append(env, "OTEL_SERVICE_NAME="+serviceName)
		}
		if !hasResourceAttrs {
			env = append(env, "OTEL_RESOURCE_ATTRIBUTES=wendy.app.name="+appID)
		}
	}
	return env
}

func hasHostNetworkEntitlement(appCfg *appconfig.AppConfig) bool {
	return entitlementsUseHostNetwork(appCfg.Entitlements)
}

// boardDetect identifies the host SBC. Behind a var so tests can simulate a
// Jetson or a non-Jetson host without touching the filesystem, mirroring the
// oci package's hook of the same name.
var boardDetect = board.Detect

// needsNvidiaCDI reports whether CreateContainer should apply the host's
// NVIDIA CDI spec (library mounts, extra device nodes, driver env vars) to
// this app's OCI spec. Both the explicit gpu entitlement AND — on a Jetson —
// the display entitlement trigger it: applyDisplay's own doc comment promises
// that "the NVIDIA EGL/GLES userspace is injected from the host via CDI" for a
// Jetson app, but before this fix that injection only happened for apps that
// also declared gpu — an app requesting display alone (no gpu) got /dev/dri and
// the NVIDIA_DRIVER_CAPABILITIES=all env var from applyDisplay but none of
// the actual library/device mounts CDI provides, so its EGL/GLES calls had
// nothing real to bind to. Merging CDI's container edits is not a new grant
// of trust for a display app: applyDisplay already sets
// NVIDIA_VISIBLE_DEVICES=all and NVIDIA_DRIVER_CAPABILITIES=all on Jetson
// once display is requested, so this only fulfills what that entitlement
// already declares.
func needsNvidiaCDI(appCfg *appconfig.AppConfig) bool {
	// An explicit GPU entitlement attempts NVIDIA CDI/CSV provisioning on every
	// board, as it did before this change — including the warnings applyNvidiaCDI
	// logs when the host has no NVIDIA provisioning at all, which are the point
	// of asking for a GPU that isn't there.
	if appCfg.HasEntitlement(appconfig.EntitlementGPU) {
		return true
	}
	// A display entitlement only implies NVIDIA userspace on a Jetson. Gating on
	// the same board check applyDisplay uses for NVIDIA_DRIVER_CAPABILITIES keeps
	// the two in step, and keeps a Raspberry Pi display app — which has no NVIDIA
	// anything — out of applyNvidiaCDI's "no CDI spec found" warning path.
	return appCfg.HasEntitlement(appconfig.EntitlementDisplay) && boardDetect().IsJetson()
}

// entitlementsUseHostNetwork reports whether the entitlements put the container
// on the HOST network namespace — a network entitlement with mode host,
// host-admin, or omitted (empty), matching applyNetwork's host-netns selection.
// Such a container's non-loopback listening ports are reachable on the device's
// real interfaces.
func entitlementsUseHostNetwork(ents []appconfig.Entitlement) bool {
	for _, e := range ents {
		if e.Type == appconfig.EntitlementNetwork && (e.Mode == "host" || e.Mode == "host-admin" || e.Mode == "") {
			return true
		}
	}
	return false
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

var startPostStartHookCommand = func(argv []string) (func() error, error) {
	// SECURITY (WDY-1009): exec the hook directly via argv. The command must
	// never be passed to a shell — doing so would let any app's wendy.json
	// inject arbitrary commands that run as the agent (root) on the host,
	// bypassing the container sandbox and entitlement boundary.
	if len(argv) == 0 {
		// Keep the argv[0] invariant local to the runner so a future caller
		// gets an error rather than a panic.
		return nil, errors.New("postStart hook argv is empty")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd.Wait, nil
}

func (c *Client) startPostStartAgentHook(command, appName string) bool {
	if command == "" {
		return false
	}

	// Expand ${WENDY_*}/env references, then split into argv on whitespace.
	// Because the result is exec'd directly (no shell), shell metacharacters in
	// the command or in any expanded value are inert — they become literal
	// arguments rather than new commands.
	argv := strings.Fields(expandAgentHook(command, appName))
	if len(argv) == 0 {
		// Log the raw (pre-expansion) command, not the expanded value: it is the
		// developer-authored wendy.json string (variable references, not their
		// expanded values), so it is safe to log and tells the operator which
		// hook misfired.
		c.logger.Warn("postStart agent hook expanded to an empty command; skipping",
			zap.String("app_name", appName),
			zap.String("configured_command", command),
		)
		return false
	}
	// strings.Fields does not honor shell quoting, so a quoted argument is split
	// on whitespace. Warn rather than mis-execute silently; quoting users should
	// move the logic into a script file.
	if strings.ContainsAny(command, `"'`) {
		c.logger.Warn("postStart agent hook contains quote characters; quoting is not honored and arguments are split on whitespace — move shell logic into a script file",
			zap.String("app_name", appName),
			zap.String("configured_command", command),
		)
	}
	wait, err := startPostStartHookCommand(argv)
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
	if err := c.terminateTask(ctx, existingTask, appName, syscall.SIGKILL, killWaitTimeout, killWaitTimeout); err != nil {
		c.logger.Warn("Failed to delete stale task",
			zap.String("app_name", appName),
			zap.Error(err))
	}
}

// isMissingRuncStateDir reports whether err is runc's own "cannot open
// directory" failure for a task's state directory under
// /run/containerd/runc/<namespace>/<containerID>. Observed live: a half-dead
// task whose state dir had been removed out from under it wedged both
// StopContainer and `ctr tasks rm -f` indefinitely — runc's delete path
// stats the directory before it will proceed, so an absent (even empty) dir
// is a permanent failure until the directory exists again. Returns the
// missing directory path so the caller can recreate it and retry.
//
// The literal string this matches was captured from the error observed on
// hardware:
//
//	cannot open directory `/run/containerd/runc/default/com.wendylabs.examples.mcp-example`: No such file or directory
func isMissingRuncStateDir(err error) (dir string, ok bool) {
	if err == nil {
		return "", false
	}
	msg := err.Error()
	const marker = "cannot open directory `"
	idx := strings.Index(msg, marker)
	if idx < 0 {
		return "", false
	}
	rest := msg[idx+len(marker):]
	end := strings.IndexByte(rest, '`')
	if end < 0 {
		return "", false
	}
	dir = rest[:end]
	if !strings.HasPrefix(dir, "/run/containerd/runc/") {
		return "", false
	}
	return dir, true
}

// recoverMissingRuncStateDir classifies err via isMissingRuncStateDir and, if
// it matches, delegates the actual recovery (mkdir + retry) to
// recreateRuncStateDirAndRetry. Any other error — including nil — passes
// through unchanged and retry is never called.
//
// Shared by forceDeleteTask (replace path) and terminateTask (stop path,
// task_teardown.go): both ultimately delete a containerd task and both can
// hit the exact same live-observed wedge, so both need the same recovery
// rather than only the replace path having it (the stop path was the one
// actually observed wedged on hardware).
func (c *Client) recoverMissingRuncStateDir(containerID string, err error, retry func() error) error {
	dir, ok := isMissingRuncStateDir(err)
	if !ok {
		return err
	}
	return c.recreateRuncStateDirAndRetry(containerID, dir, err, retry)
}

// runcStateDirMkdirAll is a seam over os.MkdirAll: recreateRuncStateDirAndRetry
// calls through it rather than os.MkdirAll directly so tests can verify the
// full forceDeleteTask/terminateTask recovery wiring — including
// isMissingRuncStateDir's hardcoded /run/containerd/runc/ prefix check —
// without needing real root-owned /run filesystem access on the test host.
// Production code never reassigns it.
var runcStateDirMkdirAll = os.MkdirAll

// recreateRuncStateDirAndRetry recreates dir (the runc task state directory
// isMissingRuncStateDir extracted from origErr) and, if that succeeds, calls
// retry and returns its result. If MkdirAll fails, origErr is returned
// unchanged and retry is never called. Split out from
// recoverMissingRuncStateDir so it can be unit-tested directly against an
// arbitrary writable directory, independent of isMissingRuncStateDir's
// /run/containerd/runc/ prefix requirement (which a test can't satisfy
// without root on a real Linux host).
func (c *Client) recreateRuncStateDirAndRetry(containerID, dir string, origErr error, retry func() error) error {
	// Mode 0o711 matches runc's own permissions for per-container state
	// dirs: traversable by root, opaque to other users.
	if mkErr := runcStateDirMkdirAll(dir, 0o711); mkErr != nil {
		c.logger.Warn("Failed to recreate missing runc state dir",
			zap.String("container_id", containerID),
			zap.String("dir", dir),
			zap.Error(mkErr),
		)
		return origErr
	}
	c.logger.Info("Recreated missing runc state dir, retrying delete",
		zap.String("container_id", containerID),
		zap.String("dir", dir),
	)
	return retry()
}

// forceDeleteTask uses the low-level containerd task service to delete a task
// by container ID. This handles orphaned tasks where container.Task() fails
// because the shim process is gone but task metadata remains in the runtime.
//
// If the delete fails because runc's state directory for the task is missing
// (recoverMissingRuncStateDir), it recreates the empty directory runc expects
// and retries once — otherwise the caller (and any subsequent
// `ctr tasks rm -f`) would fail this exact way forever, since nothing else
// ever recreates it.
func (c *Client) forceDeleteTask(ctx context.Context, containerID string) {
	_, err := c.client.TaskService().Delete(ctx, &tasks.DeleteTaskRequest{
		ContainerID: containerID,
	})
	err = c.recoverMissingRuncStateDir(containerID, err, func() error {
		_, retryErr := c.client.TaskService().Delete(ctx, &tasks.DeleteTaskRequest{
			ContainerID: containerID,
		})
		return retryErr
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

	// Derive appID and serviceName from labels — they are the authoritative
	// source (set at creation time by wendyLabels). Parsing the container name
	// is intentionally avoided: the name format is an encoded composite of
	// appID+serviceName and labels are unambiguous (SOC2-CC8).
	labelAppID := info.Labels[labelKeyAppID]
	labelSvcName := info.Labels[labelKeyServiceName]
	if labelAppID == "" {
		// Fallback for containers created before label-based identity was
		// introduced; parse the name as a best-effort recovery.
		var parseErr error
		labelAppID, labelSvcName, parseErr = ParseContainerName(appName)
		if parseErr != nil {
			return fmt.Errorf("refusing to recreate container with malformed name: %w", parseErr)
		}
	}
	if err := appconfig.ValidateAppID(labelAppID); err != nil {
		return fmt.Errorf("refusing to recreate container with invalid appID in labels: %w", err)
	}
	if labelSvcName != "" {
		if err := appconfig.ValidateServiceName(labelSvcName); err != nil {
			return fmt.Errorf("refusing to recreate container with invalid serviceName in labels: %w", err)
		}
	}

	// Delete the container (cascades to orphaned task).
	if err := ctr.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		return fmt.Errorf("deleting container: %w", err)
	}
	snapshotKey := SnapshotKey(labelAppID, labelSvcName)
	_, err = c.client.NewContainer(ctx, ContainerName(labelAppID, labelSvcName),
		containerd.WithImage(image),
		containerd.WithSnapshotter(c.snapshotter),
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

// Compile-time assertion that *Client provides the group-restart capability the
// container monitor type-asserts for. Without this, a signature drift would make
// the monitor's runtime type assertion silently fail and fall back to
// single-container restarts, leaving shared-namespace groups broken on restart.
var _ services.GroupRestarter = (*Client)(nil)

// GroupRestartAppID reports whether appName is a member of a shared-namespace
// app group (shared-ipc/shared-network with more than one service) and, if so,
// returns the bare appID. The container monitor uses this to route a member's
// restart through RestartGroup instead of an independent StartContainer, which
// would leave a secondary attached to the primary's now-dead namespace.
func (c *Client) GroupRestartAppID(ctx context.Context, appName string) (string, bool) {
	appID, svcName, err := ParseContainerName(appName)
	if err != nil || svcName == "" {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !appconfig.IsSharedNamespaceIsolation(c.getIsolation(appID)) {
		return "", false
	}
	if len(c.appServices[appID]) <= 1 {
		return "", false
	}
	return appID, true
}

// RestartGroup restarts every service of a shared-namespace app group as a unit.
// A secondary's namespace join is resolved at container-create time against the
// primary's *running* task, and the resolved /proc/<pid>/ns/* path is baked into
// the stored OCI spec. When the primary restarts it gets a new PID and brand-new
// kernel namespaces, so any secondary still pointing at the old PID is stranded
// in a dead (or worse, recycled) namespace — observable as a secondary that
// shares /dev/shm (a host bind-mount, PID-independent) but cannot reach the
// primary over localhost (network namespace gone).
//
// To restore the invariant it: (1) stops every member task, (2) clears the
// stale primary PID, (3) starts the primary so it re-registers a live PID, then
// (4) re-resolves each secondary's namespace join against that new PID before
// starting it. It returns the per-service output channels keyed by full
// container name so the caller can drain them.
func (c *Client) RestartGroup(ctx context.Context, appID string) (map[string]<-chan services.ContainerOutput, error) {
	ctx = c.withNamespace(ctx)

	c.mu.Lock()
	isolation := c.getIsolation(appID)
	servicesMap := c.appServices[appID]
	c.mu.Unlock()

	if !appconfig.IsSharedNamespaceIsolation(isolation) {
		return nil, fmt.Errorf("RestartGroup: app %q is not a shared-namespace group (isolation %q)", appID, isolation)
	}
	if len(servicesMap) <= 1 {
		return nil, fmt.Errorf("RestartGroup: app %q has %d service(s); not a group", appID, len(servicesMap))
	}
	order, err := appconfig.ServiceTopoOrder(servicesMap)
	if err != nil {
		return nil, fmt.Errorf("RestartGroup: resolving service order for %q: %w", appID, err)
	}
	if err := c.ensureGroupRestartAllowed(ctx, appID, order...); err != nil {
		return nil, err
	}

	// 1. Stop every member task so no secondary is left attached to a namespace
	//    about to be recreated. Containers are kept; only tasks are deleted.
	for _, svc := range order {
		name := ContainerName(appID, svc)
		if serr := c.stopOne(ctx, name); serr != nil {
			c.logger.Warn("RestartGroup: failed to stop group member (continuing)",
				zap.String(logfields.AppID, appID), zap.String(logfields.ServiceName, svc), zap.Error(serr))
		}
	}

	// 2. Clear the stale primary PID; the primary started below re-registers it.
	c.mu.Lock()
	c.clearPrimaryPID(appID)
	c.mu.Unlock()

	results := make(map[string]<-chan services.ContainerOutput, len(order))

	// 3. Start the primary first so setPrimaryPID records the new live PID
	//    before any secondary resolves its join against it.
	primaryName := ContainerName(appID, order[0])
	if err := c.ensureGroupRestartAllowed(ctx, appID, order[0]); err != nil {
		return nil, err
	}
	primaryCh, err := c.StartContainer(ctx, primaryName, "", nil)
	if err != nil {
		return nil, fmt.Errorf("RestartGroup: starting primary %q: %w", primaryName, err)
	}
	results[primaryName] = primaryCh

	c.mu.Lock()
	primaryPID, hasPrimary := c.getPrimaryPID(appID)
	c.mu.Unlock()
	if !hasPrimary || primaryPID == 0 {
		return results, fmt.Errorf("RestartGroup: primary %q started but no PID recorded", primaryName)
	}

	// 4. Re-resolve each secondary's namespace join against the new primary PID,
	//    then start it.
	for _, svc := range order[1:] {
		name := ContainerName(appID, svc)
		if err := c.ensureGroupRestartAllowed(ctx, appID, svc); err != nil {
			return results, err
		}
		if rerr := c.refreshSecondaryNamespaces(ctx, name, primaryPID, isolation); rerr != nil {
			c.logger.Error("RestartGroup: failed to refresh secondary namespaces",
				zap.String(logfields.AppID, appID), zap.String(logfields.ServiceName, svc), zap.Error(rerr))
			continue
		}
		ch, serr := c.StartContainer(ctx, name, "", nil)
		if serr != nil {
			if errors.Is(serr, errAppStopping) {
				return results, serr
			}
			c.logger.Error("RestartGroup: failed to start secondary",
				zap.String(logfields.AppID, appID), zap.String(logfields.ServiceName, svc), zap.Error(serr))
			continue
		}
		results[name] = ch
	}
	return results, nil
}

// ensureGroupRestartAllowed prevents a stale monitor action from reviving an
// app after a user stop. Before teardown callers pass every member so an
// already-stopped group is left untouched. Before a start they pass only that
// member: a user stop persists the marker on every member, while appStopping
// covers teardown in progress, so checking the full group again would only add
// quadratic containerd metadata reads.
func (c *Client) ensureGroupRestartAllowed(ctx context.Context, appID string, serviceNames ...string) error {
	c.mu.Lock()
	stopping := c.appStopping[appID]
	c.mu.Unlock()
	if stopping {
		return fmt.Errorf("%w: %q", errAppStopping, appID)
	}
	for _, svc := range serviceNames {
		name := ContainerName(appID, svc)
		ctr, err := c.client.LoadContainer(ctx, name)
		if err != nil {
			return fmt.Errorf("RestartGroup: checking stop state for %q: %w", name, err)
		}
		labels, err := ctr.Labels(ctx)
		if err != nil {
			return fmt.Errorf("RestartGroup: reading stop state for %q: %w", name, err)
		}
		if labels[labelKeyStoppedByUser] == "true" {
			return fmt.Errorf("RestartGroup: app %q was explicitly stopped", appID)
		}
	}
	return nil
}

// refreshSecondaryNamespaces rewrites a secondary container's stored OCI spec so
// its namespace join targets primaryPID, then delete+recreates the container
// with the refreshed spec (the spec is immutable on a live container; recreating
// is the same mechanism used by recreateContainer). The container's image and
// labels are preserved.
func (c *Client) refreshSecondaryNamespaces(ctx context.Context, name string, primaryPID uint32, isolation string) error {
	ctr, err := c.client.LoadContainer(ctx, name)
	if err != nil {
		return fmt.Errorf("loading container %q: %w", name, err)
	}
	info, err := ctr.Info(ctx)
	if err != nil {
		return fmt.Errorf("getting container info: %w", err)
	}
	image, err := ctr.Image(ctx)
	if err != nil {
		return fmt.Errorf("getting container image: %w", err)
	}
	if info.Spec == nil {
		return fmt.Errorf("container %q has no stored spec", name)
	}

	// Decode the stored spec into our spec type. The agent always stores a
	// localoci.Spec-shaped JSON (via WithSpecFromBytes), so this round-trips.
	var spec localoci.Spec
	if err := json.Unmarshal(info.Spec.GetValue(), &spec); err != nil {
		return fmt.Errorf("decoding stored spec for %q: %w", name, err)
	}

	// Re-resolve the namespace join against the new primary PID. JoinGroupNamespaces
	// overwrites the Path on the existing ipc/network/uts entries.
	anchors, err := localoci.JoinGroupNamespaces(&spec, primaryPID, isolation)
	if err != nil {
		return fmt.Errorf("re-resolving group namespaces: %w", err)
	}
	defer func() {
		for _, f := range anchors {
			f.Close()
		}
	}()

	newSpecJSON, err := json.Marshal(&spec)
	if err != nil {
		return fmt.Errorf("marshaling refreshed spec: %w", err)
	}

	// Derive identity from labels (authoritative; set at creation by wendyLabels),
	// falling back to the name only when the label is absent (SOC2-CC8).
	labelAppID := info.Labels[labelKeyAppID]
	labelSvcName := info.Labels[labelKeyServiceName]
	if labelAppID == "" {
		var parseErr error
		labelAppID, labelSvcName, parseErr = ParseContainerName(name)
		if parseErr != nil {
			return fmt.Errorf("refusing to recreate container with malformed name: %w", parseErr)
		}
	}
	if err := appconfig.ValidateAppID(labelAppID); err != nil {
		return fmt.Errorf("refusing to recreate container with invalid appID in labels: %w", err)
	}
	if labelSvcName != "" {
		if err := appconfig.ValidateServiceName(labelSvcName); err != nil {
			return fmt.Errorf("refusing to recreate container with invalid serviceName in labels: %w", err)
		}
	}

	if err := ctr.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		return fmt.Errorf("deleting container: %w", err)
	}
	snapshotKey := SnapshotKey(labelAppID, labelSvcName)
	_, err = c.client.NewContainer(ctx, ContainerName(labelAppID, labelSvcName),
		containerd.WithImage(image),
		containerd.WithSnapshotter(c.snapshotter),
		containerd.WithNewSnapshot(snapshotKey, image),
		containerd.WithContainerLabels(info.Labels),
		containerd.WithNewSpec(
			oci.WithSpecFromBytes(newSpecJSON),
		),
	)
	if err != nil {
		return fmt.Errorf("recreating container with refreshed namespaces: %w", err)
	}
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
	code, exitedAt, err := exitStatus.Result()
	// taskExited is true only once we know the process has actually
	// terminated (see the context.Canceled branch below, where it hasn't).
	// Gates the IO drain below: task.IO().Wait() blocks until the task's
	// stdio FIFOs reach EOF, which only happens once the process's fds
	// close — calling it while the container is genuinely still running
	// would hang this goroutine forever.
	taskExited := err == nil
	if err != nil {
		if errors.Is(err, context.Canceled) {
			// The wait was canceled because the RPC that started this monitor
			// ended — e.g. `wendy run --detach` returned and tore down its
			// deploy stream. The container keeps running; this is normal
			// teardown, not a task failure, so don't log it as an error — and
			// crucially, don't record an exit: the container is still up.
			c.logger.Debug("Stopped monitoring task exit (stream canceled)",
				zap.String("app_name", appName),
			)
		} else {
			c.logger.Error("Task exited with error",
				zap.String("app_name", appName),
				zap.Error(err),
			)
		}
	} else {
		c.logger.Info("Task exited",
			zap.String("app_name", appName),
			zap.Uint32("exit_code", code),
		)
		// Persist why this run ended so a stopped/crashed container can explain
		// itself later (the task and this live stream are about to disappear).
		// Detached context: the RPC ctx may be torn down the instant we return.
		recCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		reason := classifyExit(code, taskOOMKilled(recCtx, task))
		c.recordContainerExit(recCtx, appName, int32(code), reason, exitedAt)
		cancel()
	}

	if taskExited {
		// The exit status arriving does NOT mean containerd has finished
		// copying the task's stdout/stderr FIFOs into stdoutW/stderrW: that
		// copy runs on containerd's own goroutines (cio.copyIO), a path
		// independent of the process-exit signal task.Wait() delivers here.
		// For a container that prints one line and exits immediately — the
		// crash-loop case — that copy can still be in flight when the exit
		// status arrives, so closing stdoutW/stderrW right away can slam the
		// pipe shut before the in-flight Write() lands, silently dropping
		// the very output a crash-looping container needs most (live
		// symptom: `wendy device logs --tail` returned zero lines for an
		// actively crash-looping app). drainTaskIOThenClose waits for that
		// copy to finish before closing, mirroring the same fix already
		// applied to ExecContainer/ROS2 exec (proc.IO().Wait()) in this
		// package.
		drainTaskIOThenClose(task.IO(), stdoutW, stderrW)
	} else {
		// Task not confirmed exited (context.Canceled path, effectively
		// unreachable since taskCtx derives from context.Background() and is
		// never canceled): preserve prior behavior and close unconditionally
		// rather than risk blocking forever in task.IO().Wait().
		stdoutW.Close()
		stderrW.Close()
	}

	// Wait for readers to finish.
	wg.Wait()

	outputCh <- services.ContainerOutput{Done: true}
}

// drainTaskIOThenClose blocks until taskIO's stdio copy goroutines (if any)
// have finished flushing container output into stdoutW/stderrW, then closes
// both writers. Must only be called once the task is confirmed to have
// exited — see the taskExited gate in streamOutput's caller — since
// taskIO.Wait() blocks until the underlying FIFOs reach EOF, which a still-
// running task never delivers. taskIO may be nil (some cio.IO
// implementations, e.g. NullIO, are legitimately IO-less).
func drainTaskIOThenClose(taskIO cio.IO, stdoutW, stderrW io.Closer) {
	if taskIO != nil {
		taskIO.Wait()
	}
	stdoutW.Close()
	stderrW.Close()
}

// resolveTargets resolves name to the containers it addresses:
//
//  1. a container whose ID equals name — a single-container app, or one service
//     addressed as "{appID}_{serviceName}"
//  2. containers whose labelKeyAppID equals name — every service in the group
//  3. neither — errdefs.ErrNotFound
//
// Container IDs are unique within the namespace, so rule 1 has at most one hit.
// appID is the label-derived group identity; wholeApp reports whether name
// addressed every container in the group.
//
// Caller must hold c.mu. ctx must already have the containerd namespace set.
func (c *Client) resolveTargets(ctx context.Context, name string) (ctrs []containerd.Container, appID string, wholeApp bool, err error) {
	// name reaches the containerd filter expression via containersForApp
	// (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
	if verr := appconfig.ValidateAppID(name); verr != nil {
		return nil, "", false, fmt.Errorf("invalid app name: %w", verr)
	}

	ctr, loadErr := c.client.LoadContainer(ctx, name)
	switch {
	case loadErr == nil:
		// The "default" namespace is shared with anything else that speaks to
		// containerd, so an ID match alone is not proof this is ours: require
		// the app-id label (SOC2-CC6, NIST-AC-3, ISO27001-A.8).
		if id, ok := c.wendyAppIDOf(ctx, ctr); ok {
			group, groupErr := c.containersForApp(ctx, id)
			if groupErr != nil {
				return nil, "", false, groupErr
			}
			return []containerd.Container{ctr}, id, len(group) <= 1, nil
		}
		// Unlabelled or foreign: fall through, which reports NotFound.
	case !errdefs.IsNotFound(loadErr):
		// A real lookup failure must not be laundered into NotFound.
		return nil, "", false, fmt.Errorf("loading container %q: %w",
			sanitizeForLog(name, 253), loadErr)
	}

	group, err := c.containersForApp(ctx, name)
	if err != nil {
		return nil, "", false, err
	}
	if len(group) == 0 {
		return nil, "", false, fmt.Errorf("%w: no app or service named %q",
			errdefs.ErrNotFound, sanitizeForLog(name, 253))
	}
	return group, name, true, nil
}

// wendyAppIDOf returns ctr's group identity from its app-id label, and whether
// ctr is Wendy-managed at all. The container ID cannot stand in for a missing
// label: '_' is legal inside an appID, so "myapp_alpha" is ambiguous. Labels are
// external state, so the value is re-validated (SOC2-CC6, NIST-SI-10).
func (c *Client) wendyAppIDOf(ctx context.Context, ctr containerd.Container) (string, bool) {
	labels, err := ctr.Labels(ctx)
	if err != nil {
		return "", false
	}
	id := labels[labelKeyAppID]
	if id == "" || appconfig.ValidateAppID(id) != nil {
		return "", false
	}
	return id, true
}

// containersForApp returns all Wendy-managed containers whose labelKeyAppID
// label equals appID. Both single-container apps (one container) and
// multi-service apps (one container per service) are found this way, with no
// dependency on container-name conventions.
// ctx must already have the containerd namespace set.
func (c *Client) containersForApp(ctx context.Context, appID string) ([]containerd.Container, error) {
	// Defence-in-depth: re-validate appID at the injection site so that a future
	// caller that bypasses the RPC entry-point validation cannot inject into the
	// containerd filter expression (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
	// ValidateAppID allows only [a-zA-Z0-9._-], none of which are special in
	// the containerd filter grammar, so %q quoting is safe for this character set.
	if err := appconfig.ValidateAppID(appID); err != nil {
		return nil, fmt.Errorf("containersForApp: invalid appID: %w", err)
	}
	all, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q==%q", labelKeyAppID, appID))
	if err != nil {
		return nil, fmt.Errorf("listing containers for app %q: %w", appID, err)
	}
	// Post-filter in Go to confirm the label value matches exactly, providing
	// defence-in-depth against any future filter grammar edge case.
	// Use a fresh slice — reusing all[:0] would alias the backing array and
	// risk reading overwritten elements during the range loop (SOC2-CC6).
	var ctrs []containerd.Container
	for _, ctr := range all {
		labels, lerr := ctr.Labels(ctx)
		if lerr != nil || labels[labelKeyAppID] != appID {
			continue
		}
		ctrs = append(ctrs, ctr)
	}
	return ctrs, nil
}

// ContainerIDsForApp returns the containerd container IDs for all services
// belonging to appID. Single-container apps return one ID; multi-service apps
// return one ID per service. The service layer uses this to mark each
// container in the monitor before issuing a stop or delete.
func (c *Client) ContainerIDsForApp(ctx context.Context, appID string) ([]string, error) {
	ctx = c.withNamespace(ctx)
	ctrs, err := c.containersForApp(ctx, appID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(ctrs))
	for i, ctr := range ctrs {
		ids[i] = ctr.ID()
	}
	return ids, nil
}

// ResolveAppContainerIDs returns the container IDs addressed by name. See
// resolveTargets.
func (c *Client) ResolveAppContainerIDs(ctx context.Context, name string) ([]string, error) {
	ctx = c.withNamespace(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	ctrs, _, _, err := c.resolveTargets(ctx, name)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(ctrs))
	for i, ctr := range ctrs {
		ids[i] = ctr.ID()
	}
	return ids, nil
}

// stopOne stops the task for a single container.
// ctx must already have the containerd namespace set.
func (c *Client) stopOne(ctx context.Context, containerID string) error {
	container, err := c.client.LoadContainer(ctx, containerID)
	if err != nil {
		return fmt.Errorf("loading container %q: %w", containerID, err)
	}

	// Explicit stop is a lifecycle boundary: unlike crash-monitor restart or a
	// compatible redeploy, it must release the retained namespace and all CNI,
	// DNS, and mesh state even when the task has already exited.
	labels, _ := container.Labels(ctx)
	spec, _ := container.Spec(ctx)
	desiredNetworkIdentity, _ := networkIdentityFromLabels(labels)
	releasedSandbox := c.destroyNetworkSandbox(ctx, containerID)
	if !releasedSandbox {
		releasedSandbox = c.purgePersistedNetworkSandbox(ctx, container, labels, spec)
	}
	var clearSandboxErr error
	if releasedSandbox {
		if err := c.persistNetworkNamespace(ctx, container, "", desiredNetworkIdentity, ""); err != nil {
			clearSandboxErr = fmt.Errorf("clearing reusable network namespace for %q: %w", containerID, err)
			c.logger.Warn("Failed to clear reusable network metadata; continuing task termination", zap.String("container_id", containerID), zap.Error(err))
		}
	}

	task, err := container.Task(ctx, nil)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return clearSandboxErr // No task running.
		}
		return errors.Join(clearSandboxErr, fmt.Errorf("getting task for %q: %w", containerID, err))
	}

	// For containers with bridge wiring — multi-service isolated app services
	// and single-service bridge-mode apps, per needsCNIBridgeWiring — call CNI
	// DEL while the task's network namespace still exists (the PID is live, so
	// /proc/PID/ns/net is valid). After SIGTERM/SIGKILL the netns reference
	// disappears. CNI DEL is best-effort — failure is logged but does not
	// block the stop path.
	if appID, svcName, parseErr := ParseContainerName(containerID); parseErr == nil {
		var entitlements []appconfig.Entitlement
		c.mu.Lock()
		isolation := c.getIsolation(appID)
		c.mu.Unlock()
		if labels, lerr := container.Labels(ctx); lerr == nil {
			entitlements = parseEntitlementsFromAnnotations(labels)
			// Prefer this container's own persisted label
			// (labelKeyIsolation, written at create) over the in-memory
			// c.appIsolation cache. The cache is only best-effort rebuilt
			// from container labels at agent boot (rebuildCachesFromLabels);
			// if that warm-up missed this appID, c.getIsolation silently
			// returns "" and needsCNIBridgeWiring below would wrongly skip
			// CNI DEL / DNS-listener release / mesh egress teardown for a
			// container that is actually isolated — leaking its IP,
			// iptables rules, and DNS refcount until agent restart. The
			// label is written once at create and never goes stale, so it
			// is authoritative here even when the cache is not.
			if iso, ok := labels[labelKeyIsolation]; ok {
				isolation = iso
			}
		}

		if needsCNIBridgeWiring(isolation, svcName, entitlements) && !releasedSandbox {
			netnsPath := fmt.Sprintf("/proc/%d/ns/net", task.Pid())
			if cniErr := c.CNIDel(ctx, appID, containerID, netnsPath); cniErr != nil {
				c.logger.Warn("CNI DEL failed during stop (non-fatal)",
					zap.String("container_id", containerID), zap.Error(cniErr))
			}

			// Release the gateway DNS listener reference this container held,
			// if any (see needsGatewayDNS / ensureMeshDNS in StartContainer).
			// releaseMeshDNS is idempotent (held-map guarded), so calling it
			// here AND from teardownMeshEgress below for a meshed app is safe —
			// the second call is a no-op.
			if needsGatewayDNS(isolation, entitlements) {
				c.releaseMeshDNS(containerID, appID)
			}

			// Mesh egress teardown: remove the host iptables rule installed by
			// applyMeshEgress at start (StartContainer). teardownMeshEgress is a
			// no-op for apps without the network/mesh entitlement (including
			// bridge-mode apps). The netns route itself needs no cleanup — it
			// is destroyed with the namespace when the task exits below.
			//
			// The container's IP is recovered from c.serviceIPs (populated by
			// recordServiceIP after CNI ADD, keyed by appID/serviceName — empty
			// for single-service bridge apps, which is fine: teardownMeshEgress
			// no-ops for them regardless of ip), not from the CNI host-local
			// IPAM on-disk state — that state lives under cniStateDir/<appID>
			// keyed by allocated IP, not by containerID, so recovering "this
			// container's IP" from it would require an extra reverse-index the
			// plugin does not provide as a stable public format.
			c.mu.Lock()
			ip := c.serviceIPs[appID][svcName]
			c.mu.Unlock()
			c.teardownMeshEgress(entitlements, containerID, appID, ip)
		}
	}

	// SIGTERM the whole process group for graceful shutdown, escalating to
	// SIGKILL after the grace period. Group-wide signalling (not just init)
	// ensures no descendant survives holding devices/ports (WDY-1818).
	if err := c.terminateTask(ctx, task, containerID, syscall.SIGTERM, stopGracePeriod, killWaitTimeout); err != nil {
		return errors.Join(clearSandboxErr, err)
	}

	if c.proxyManager != nil {
		_ = c.proxyManager.Stop(containerID)
	}

	c.logger.Info("Container stopped", zap.String("container_id", containerID))

	// Recompute camera-loopback nodes/consumers from truth now that this
	// container is no longer running: it may have just dropped out of the
	// entitled-and-running set (see StartContainer for the WithoutCancel/go
	// rationale).
	go c.SyncCameraLoopbacks(context.WithoutCancel(ctx))

	return clearSandboxErr
}

// StopContainer stops the containers addressed by name: a bare appID stops
// every service, "{appID}_{serviceName}" stops one. See resolveTargets.
// appStopping closes the gap between the container-list snapshot and the stop
// loop; resolved per-container network locks then drain any start already in
// flight before CNI teardown begins (TOCTOU, SOC2-CC6, NIST-AC-4).
func (c *Client) StopContainer(ctx context.Context, name string) error {
	ctx = c.withNamespace(ctx)
	unlockNetwork := c.lockNetworkOperation(name)
	defer unlockNetwork()

	// Hold mutex only long enough to enumerate containers and resolve stop order.
	// Releasing before stopOne prevents holding c.mu across potentially long
	// blocking I/O (SIGTERM wait, 10 s timeout), which would starve concurrent
	// StartContainer / CreateContainerWithProgress calls (SOC2-CC6, NIST-AC-3).
	c.mu.Lock()
	ctrs, appID, wholeApp, err := c.resolveTargets(ctx, name)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if c.appStopping[appID] {
		c.mu.Unlock()
		return fmt.Errorf("%w: %q", errAppStopping, appID)
	}
	stopOrder := c.resolveStopOrder(ctx, appID, ctrs)
	// Mark app as stopping before releasing the mutex so any concurrent
	// CreateContainerWithProgress call will see it and abort (SOC2-CC6, NIST-AC-3).
	if c.appStopping == nil {
		c.appStopping = make(map[string]bool)
	}
	c.appStopping[appID] = true
	c.mu.Unlock()

	// A whole-app operation is keyed by bare appID, while service starts and
	// creates are keyed by concrete container ID. Hold every member key for the
	// remainder of teardown so CNI ADD/CHECK/DEL cannot overlap this group stop.
	unlockTargets := c.lockAdditionalNetworkOperations(name, stopOrder)
	defer unlockTargets()

	// Pause the restart monitor for every container about to be stopped, for
	// the whole rest of this function: without it, a crash-looping member's
	// automatic restart can race stopOne's kill+delete below and wedge the
	// stop (same "cannot delete running task: failed precondition" race as
	// the replace path in CreateContainerWithProgress).
	resumeFns := make([]func(), 0, len(stopOrder))
	for _, ctrID := range stopOrder {
		resumeFns = append(resumeFns, c.suppressRestarts(ctrID))
	}
	defer func() {
		for _, resume := range resumeFns {
			resume()
		}
	}()

	var errs []error
	for _, ctrID := range stopOrder {
		if err := c.stopOne(ctx, ctrID); err != nil {
			c.logger.Error("Failed to stop service container",
				zap.String("container_id", ctrID),
				zap.Error(err))
			errs = append(errs, err)
		}
	}

	// Per-app state is only released once the whole group is stopped; siblings
	// still need it.
	if wholeApp {
		// Re-acquire mutex for map cleanup. Both reads and writes of these maps
		// are protected by c.mu to prevent data races with concurrent callers
		// (SOC2-CC6, NIST-AC-3, ISO27001-A.8).
		// clearPrimaryPID under the lock; other per-app metadata is kept alive until
		// after the late sweep so that appIsolation is still readable by any
		// concurrent code that observes appStopping (SOC2-CC6, NIST-AC-3).
		c.mu.Lock()
		c.clearPrimaryPID(appID)
		c.mu.Unlock()

		// Re-enumerate to catch any containers that appeared after
		// resolveStopOrder snapshotted the list (e.g. a concurrent StartContainer
		// mid-CNI-ADD). stopOne is idempotent for already-stopped containers.
		// appStopping is still set during this sweep to block new concurrent creates.
		if lateCtrs, lateErr := c.containersForApp(ctx, appID); lateErr == nil && len(lateCtrs) > 0 {
			for _, ctr := range lateCtrs {
				if stopErr := c.stopOne(ctx, ctr.ID()); stopErr != nil {
					c.logger.Error("StopContainer: failed to stop late-appearing container",
						zap.String("container_id", ctr.ID()), zap.Error(stopErr))
					errs = append(errs, stopErr)
				}
			}
		}
	}

	// Release per-app metadata in one atomic section AFTER the late sweep, so
	// no partial-state window exists between metadata deletion and appStopping
	// clearance. Concurrent CreateContainerWithProgress remains blocked (via
	// appStopping) until this section completes (SOC2-CC6, NIST-AC-3, NIST-SI-16,
	// ISO27001-A.8, SOC2-CC8/ISO27001-A.12 unbounded-growth prevention).
	c.mu.Lock()
	if wholeApp {
		delete(c.appServices, appID)
		delete(c.appIsolation, appID)
		delete(c.serviceIPs, appID)
	}
	delete(c.appStopping, appID)
	c.mu.Unlock()

	return errors.Join(errs...)
}

// resolveStopOrder returns container IDs in reverse dependency order (dependents first).
// Falls back to arbitrary order for single-container apps or unknown graphs.
// Caller must hold c.mu.
func (c *Client) resolveStopOrder(ctx context.Context, appID string, ctrs []containerd.Container) []string {
	if len(ctrs) <= 1 {
		ids := make([]string, len(ctrs))
		for i, ctr := range ctrs {
			ids[i] = ctr.ID()
		}
		return ids
	}

	services := c.appServices[appID]
	if len(services) == 0 {
		ids := make([]string, len(ctrs))
		for i, ctr := range ctrs {
			ids[i] = ctr.ID()
		}
		return ids
	}

	// Build serviceName→containerID map from containerd labels.
	svcToID := make(map[string]string, len(ctrs))
	for _, ctr := range ctrs {
		labels, err := ctr.Labels(ctx)
		if err != nil {
			continue
		}
		if svcName := labels[labelKeyServiceName]; svcName != "" {
			svcToID[svcName] = ctr.ID()
		}
	}

	ordered, err := appconfig.ServiceTopoOrder(services)
	if err != nil {
		c.logger.Warn("resolveStopOrder: topo sort failed, using arbitrary order",
			zap.String(logfields.AppID, appID), zap.Error(err))
		ids := make([]string, len(ctrs))
		for i, ctr := range ctrs {
			ids[i] = ctr.ID()
		}
		return ids
	}

	// Reverse for stop order: dependents first, then dependencies.
	result := make([]string, 0, len(ctrs))
	for i := len(ordered) - 1; i >= 0; i-- {
		if id, ok := svcToID[ordered[i]]; ok {
			result = append(result, id)
		}
	}
	return result
}

// sharedSHMPath returns the host-side shared memory directory for a shared-ipc
// app group after validating the app ID. It does NOT create the directory — use
// ensureSharedSHM for that. Its presence on disk is the agent's signal that an
// app group runs with shared-ipc isolation.
func sharedSHMPath(appID string) (string, error) {
	if err := appconfig.ValidateAppID(appID); err != nil {
		return "", fmt.Errorf("sharedSHMPath: %w", err)
	}
	return "/run/wendy/shm/" + appID, nil
}

// ensureSharedSHM creates the host-side shared memory directory for a
// shared-ipc app group. Returns the path so it can be bind-mounted.
func ensureSharedSHM(appID string) (string, error) {
	path, err := sharedSHMPath(appID)
	if err != nil {
		return "", err
	}
	// Lock the OS thread so that the umask change below is thread-local and
	// does not race with other goroutines on the same process (SOC2-CC6,
	// NIST-SC-7, ISO27001-A.8). Without this, a permissive umask could widen
	// 0o1770 → 0o1750 or looser during the MkdirAll call, creating a window
	// before the subsequent Chmod during which the directory is accessible to
	// unintended users.
	// 0o1700: owner-only sticky directory. The agent runs as root (uid 0) and
	// is the sole writer; group/other bits are cleared so no GID-0 sibling
	// daemon can traverse or modify the shm tree (SOC2-CC6, NIST-AC-3,
	// ISO27001-A.9). The sticky bit prevents any in-container process from
	// unlinking entries owned by a different container even if it somehow
	// gains access to the host mount.
	runtime.LockOSThread()
	oldUmask := syscall.Umask(0)
	mkdirErr := os.MkdirAll(path, 0o1700)
	syscall.Umask(oldUmask)
	runtime.UnlockOSThread()
	if mkdirErr != nil {
		return "", fmt.Errorf("creating shared shm dir %q: %w", path, mkdirErr)
	}
	// Explicit Chmod to handle the case where the directory already existed
	// with looser permissions (MkdirAll is a no-op for existing dirs).
	if err := os.Chmod(path, 0o1700); err != nil {
		return "", fmt.Errorf("setting permissions on shared shm dir %q: %w", path, err)
	}
	return path, nil
}

// deleteOne kills any running task, deletes a single container and its
// snapshot, and stops the D-Bus proxy. It returns the image name so the caller
// can batch image deletions across services. ctx must have the namespace set
// and the caller must hold c.mu.
func (c *Client) deleteOne(ctx context.Context, ctr containerd.Container, wantImg bool) (imgName string, err error) {
	// Delete may be called without StopContainer first. Release an owned
	// reusable sandbox even when there is no live task left to provide a procfs
	// namespace path.
	persistedLabels, _ := ctr.Labels(ctx)
	persistedSpec, _ := ctr.Spec(ctx)
	releasedSandbox := c.destroyNetworkSandbox(ctx, ctr.ID())
	if !releasedSandbox {
		releasedSandbox = c.purgePersistedNetworkSandbox(ctx, ctr, persistedLabels, persistedSpec)
	}
	// Mesh/bridge teardown for delete-without-stop: `wendy device apps remove`
	// on a RUNNING meshed or bridge-mode app goes DeleteContainer → deleteOne
	// without ever passing through stopOne, which would otherwise leak the
	// DNS-listener refcount and the ACCEPT/REDIRECT iptables rules for this
	// container's IP until agent restart. Mirrors the stopOne teardown block;
	// safe to run after a stop too — teardownMeshEgress and releaseMeshDNS are
	// both idempotent (rule removal tolerates absent rules; the DNS release is
	// guarded by the held map), so a stopOne-then-deleteOne sequence releases
	// exactly once. c.serviceIPs is read without locking because deleteOne's
	// only caller, DeleteContainer, holds c.mu for its full duration.
	// isolation is read from the container's own persisted label
	// (labelKeyIsolation) rather than c.getIsolation's in-memory cache, for
	// the same reason as stopOne above: the cache is only best-effort warmed
	// at boot, and a miss there must not silently skip CNI DEL / DNS release
	// / mesh teardown for a container that is actually isolated.
	appID, svcName, parseErr := ParseContainerName(ctr.ID())
	var entitlements []appconfig.Entitlement
	isolation := c.getIsolation(appID)
	needsBridge := false
	if parseErr == nil {
		if labels, lerr := ctr.Labels(ctx); lerr == nil {
			entitlements = parseEntitlementsFromAnnotations(labels)
			if iso, ok := labels[labelKeyIsolation]; ok {
				isolation = iso
			}
		}
		needsBridge = needsCNIBridgeWiring(isolation, svcName, entitlements)
	}
	if needsBridge && !releasedSandbox {
		if needsGatewayDNS(isolation, entitlements) {
			c.releaseMeshDNS(ctr.ID(), appID)
		}
		ip := c.serviceIPs[appID][svcName]
		c.teardownMeshEgress(entitlements, ctr.ID(), appID, ip)
	}

	if task, taskErr := ctr.Task(ctx, nil); taskErr == nil {
		// CNI DEL while the task's network namespace still exists (mirrors
		// stopOne): deleteOne is DeleteContainer's only path for a container
		// that was never explicitly stopped first (`wendy device apps remove`
		// on a running app), so without this the CNI bridge plugin's host-local
		// IPAM allocation for this exact container ID leaks forever — the next
		// create for the same appID/serviceName then fails CNI ADD with
		// "duplicate allocation is not allowed" (WDY mesh: found via repeated
		// remove+redeploy cycles during RemoteCam demo debugging).
		if needsBridge && !releasedSandbox {
			netnsPath := fmt.Sprintf("/proc/%d/ns/net", task.Pid())
			if cniErr := c.CNIDel(ctx, appID, ctr.ID(), netnsPath); cniErr != nil {
				c.logger.Warn("CNI DEL failed during delete (non-fatal)",
					zap.String("container_id", ctr.ID()), zap.Error(cniErr))
			}
		}
		if termErr := c.terminateTask(ctx, task, ctr.ID(), syscall.SIGKILL, killWaitTimeout, killWaitTimeout); termErr != nil {
			// Keep going: the container Delete below surfaces a meaningful
			// error if the task record is truly stuck, but log the root cause
			// so leaked processes are attributable (WDY-1818).
			c.logger.Warn("Failed to delete task during container delete",
				zap.String("container_id", ctr.ID()),
				zap.Error(termErr))
		}
	}
	if wantImg {
		if img, imgErr := ctr.Image(ctx); imgErr == nil {
			imgName = img.Name()
		}
	}
	if err := ctr.Delete(ctx, containerd.WithSnapshotCleanup); err != nil {
		return "", fmt.Errorf("deleting container %q: %w", ctr.ID(), err)
	}
	if c.proxyManager != nil {
		if proxyErr := c.proxyManager.Stop(ctr.ID()); proxyErr != nil {
			c.logger.Warn("Failed to stop D-Bus proxy",
				zap.String("container_id", ctr.ID()),
				zap.Error(proxyErr))
		}
	}
	c.logger.Info("Container deleted", zap.String("container_id", ctr.ID()))
	return imgName, nil
}

// DeleteContainer deletes all containers belonging to appID. For multi-service
// apps all service containers are removed. When deleteImage is true, each
// distinct image is deleted once (services sharing an image are handled safely).
func (c *Client) DeleteContainer(ctx context.Context, name string, deleteImage bool) error {
	unlockNetwork := c.lockNetworkOperation(name)
	defer unlockNetwork()
	c.mu.Lock()

	ctx = c.withNamespace(ctx)
	// A bare appID deletes every service; "{appID}_{serviceName}" deletes one.
	ctrs, appID, wholeApp, err := c.resolveTargets(ctx, name)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if c.appStopping[appID] {
		c.mu.Unlock()
		return fmt.Errorf("%w: %q", errAppStopping, appID)
	}
	if c.appStopping == nil {
		c.appStopping = make(map[string]bool)
	}
	c.appStopping[appID] = true
	c.mu.Unlock()

	containerIDs := make([]string, 0, len(ctrs))
	for _, ctr := range ctrs {
		containerIDs = append(containerIDs, ctr.ID())
	}
	// See StopContainer: the bare group key alone does not serialize against
	// per-service create/start operations. Acquire all concrete member keys
	// before re-taking c.mu, preserving network-lock -> c.mu ordering.
	unlockTargets := c.lockAdditionalNetworkOperations(name, containerIDs)
	defer unlockTargets()

	c.mu.Lock()
	defer func() {
		delete(c.appStopping, appID)
		c.mu.Unlock()
	}()

	seen := make(map[string]bool)
	var errs []error
	for _, ctr := range ctrs {
		imgName, delErr := c.deleteOne(ctx, ctr, deleteImage)
		if delErr != nil {
			c.logger.Error("Failed to delete service container",
				zap.String("container_id", ctr.ID()),
				zap.Error(delErr))
			errs = append(errs, delErr)
			continue
		}
		if imgName != "" && !seen[imgName] {
			seen[imgName] = true
			imgSvc := c.client.ImageService()
			if err := imgSvc.Delete(ctx, imgName); err != nil && !errdefs.IsNotFound(err) {
				c.logger.Warn("Failed to delete image", zap.String("image", imgName), zap.Error(err))
			} else {
				c.logger.Info("Image deleted", zap.String("image", imgName))
			}
		}
	}
	// The system-API socket is per app: only release it once the app is gone.
	if len(errs) == 0 && wholeApp && c.systemAPISocketProvider != nil {
		c.systemAPISocketProvider.ReleaseApp(appID)
	}

	// Recompute camera-loopback nodes/consumers from truth now that some or
	// all of this app's containers are gone (unconditional: even a partial
	// delete removed real containers, so the entitled-and-running set may
	// have changed regardless of errs — see StartContainer for the
	// WithoutCancel/go rationale).
	go c.SyncCameraLoopbacks(context.WithoutCancel(ctx))

	return errors.Join(errs...)
}

// ListContainers lists all Wendy-managed apps. Multi-service apps (whose
// container IDs follow the {appID}_{serviceName} convention) are grouped under
// their bare appID: the aggregate entry is RUNNING if any service is running,
// and AppContainer.Services is populated with one ServiceEntry per service so
// callers can display individual service state. This ensures that
// stop/start/remove — which address by appID — operate on the same granularity
// shown in the list and picker.
// ListBootContainers returns the containers that should be (re)started when the
// agent boots: every Wendy container whose restart policy keeps it running
// (anything other than "no") and that was NOT explicitly stopped by the user.
// The returned Name is the containerd container ID (the key the restart monitor
// uses — bare appID for single-container apps, "{appID}_{serviceName}" for
// services). An absent/empty restart-policy label is treated as keep-running, so
// apps deployed with the default policy come back on boot.
func (c *Client) ListBootContainers(ctx context.Context) ([]services.BootContainer, error) {
	ctx = c.withNamespace(ctx)

	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppVersion))
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	var result []services.BootContainer
	for _, ctr := range ctrs {
		info, err := ctr.Info(ctx)
		if err != nil {
			c.logger.Warn("Failed to get container info", zap.String("id", ctr.ID()), zap.Error(err))
			continue
		}

		// An agent-only restart leaves containerd tasks running, but Close stops
		// the agent-owned xdg-dbus-proxy processes. Restore the socket in the
		// persisted bind-mount source even when the monitor will not restart the
		// already-running task.
		running := c.containerIsRunning(ctx, ctr)
		if running {
			if spec, specErr := ctr.Spec(ctx); specErr != nil {
				c.logger.Warn("Could not inspect running container start resources",
					zap.String("id", ctr.ID()), zap.Error(specErr))
			} else if _, proxyErr := c.restoreDBusProxyForRunningTask(ctx, ctr.ID(), running, spec.Mounts); proxyErr != nil {
				c.logger.Error("Could not restore running container D-Bus proxy",
					zap.String("id", ctr.ID()), zap.Error(proxyErr))
			}
		}

		// Rehydrate c.appIsolation from the persisted label BEFORE the monitor's
		// planRestartActions runs GroupRestartAppID/RestartGroup for this
		// container, so the reboot restart path sees the isolation mode this
		// container was created with instead of the empty in-memory default
		// (WDY reboot-fix). Best-effort: an unlabelled or malformed appID just
		// skips hydration for this one container; it must never fail the whole
		// reconcile.
		if appID := info.Labels[labelKeyAppID]; appID != "" && appconfig.ValidateAppID(appID) == nil {
			c.hydrateIsolation(appID, info.Labels)
		} else {
			c.logger.Warn("ListBootContainers: missing/invalid app id label, skipping isolation hydration",
				zap.String("id", ctr.ID()))
		}

		if info.Labels[labelKeyStoppedByUser] == "true" {
			continue // user stopped it on purpose — stay down across reboot
		}
		policy, maxRetries := parseRestartPolicyLabel(info.Labels[labelKeyRestartPolicy])
		if policy == "no" {
			continue // opted out of auto-restart (e.g. wendy run --no-restart)
		}
		result = append(result, services.BootContainer{
			Name:          ctr.ID(),
			RestartPolicy: policy,
			MaxRetries:    maxRetries,
		})
	}
	return result, nil
}

// SetStoppedByUser sets or clears the persisted stopped-by-user label on a
// single container (keyed by container ID). Used by the stop/start RPCs so a
// deliberate stop survives a reboot. A missing container is not an error — the
// caller may be operating on a best-effort set of IDs.
func (c *Client) SetStoppedByUser(ctx context.Context, containerID string, stopped bool) error {
	ctx = c.withNamespace(ctx)
	ctr, err := c.client.LoadContainer(ctx, containerID)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("loading container %q: %w", containerID, err)
	}
	return ctr.Update(ctx, func(ctx context.Context, client *containerd.Client, c *containers.Container) error {
		if c.Labels == nil {
			c.Labels = map[string]string{}
		}
		if stopped {
			c.Labels[labelKeyStoppedByUser] = "true"
		} else {
			delete(c.Labels, labelKeyStoppedByUser)
		}
		return nil
	})
}

// bootMigrationMarker records that the one-time stopped-by-user back-fill has
// run on this device. It lives under the persistent state dir so the migration
// runs exactly once over the device's lifetime, not once per boot.
const bootMigrationMarker = "/var/lib/wendy/boot-reconcile-migrated"

// MigrateStoppedByUserOnce back-fills the stopped-by-user mark for apps that
// predate it, so the upgrade to boot-reconcile doesn't resurrect apps the user
// had deliberately stopped. Apps stopped under an older agent carry no
// stopped-by-user label, so without this the first boot after upgrade would see
// them as eligible and start them.
//
// On its single run it marks every container that is NOT currently running as
// stopped-by-user; running apps (live tasks) are left unmarked so they keep
// coming back on future boots. This is only correct while the device is up —
// i.e. at agent upgrade (`wendy device update`), when stopped/running still
// reflect the user's intent — NOT after a reboot, when every task is dead. The
// persistent marker guarantees it runs once, on that upgrade. (Residual edge:
// if the device reboots after the binary is installed but before the agent ever
// runs, the first run is post-reboot and would mark everything; the normal
// update path restarts the agent immediately, so this is rare.)
func (c *Client) MigrateStoppedByUserOnce(ctx context.Context) error {
	if _, err := os.Stat(bootMigrationMarker); err == nil {
		return nil // already migrated
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat migration marker: %w", err)
	}

	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppVersion))
	if err != nil {
		return fmt.Errorf("listing containers: %w", err) // transient; retry, don't mark done
	}

	var marked int
	for _, ctr := range ctrs {
		if c.containerIsRunning(ctx, ctr) {
			continue // running now → keep eligible for boot reconcile
		}
		info, infoErr := ctr.Info(ctx)
		if infoErr != nil {
			c.logger.Warn("Boot migration: failed to read container info", zap.String("id", ctr.ID()), zap.Error(infoErr))
			continue
		}
		if info.Labels[labelKeyStoppedByUser] == "true" {
			continue // already marked
		}
		if err := c.SetStoppedByUser(ctx, ctr.ID(), true); err != nil {
			c.logger.Warn("Boot migration: failed to mark stopped-by-user", zap.String("id", ctr.ID()), zap.Error(err))
			continue
		}
		marked++
	}

	// Enumeration succeeded, so the snapshot is valid even if a few per-container
	// updates failed — mark done so we don't re-run (and re-snapshot post-reboot).
	if err := os.MkdirAll("/var/lib/wendy", 0o755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}
	if err := os.WriteFile(bootMigrationMarker, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("writing migration marker: %w", err)
	}
	c.logger.Info("Boot reconcile migration complete", zap.Int("marked_stopped", marked), zap.Int("containers", len(ctrs)))
	return nil
}

// containerIsRunning reports whether the container currently has a running task.
func (c *Client) containerIsRunning(ctx context.Context, ctr containerd.Container) bool {
	task, err := ctr.Task(ctx, nil)
	if err != nil {
		return false
	}
	st, err := task.Status(ctx)
	return err == nil && st.Status == containerd.Running
}

func (c *Client) ListContainers(ctx context.Context) ([]*agentpb.AppContainer, error) {
	ctx = c.withNamespace(ctx)

	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppVersion))
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	type serviceEntry struct {
		name         string
		runningState agentpb.AppRunningState
	}
	type entry struct {
		version      string
		runningState agentpb.AppRunningState
		mcpPort      uint32
		httpPort     uint32
		services     []serviceEntry
		exitCode     int32
		exitReason   string // "" until an exit label is seen for this app
	}
	grouped := make(map[string]*entry)
	var order []string

	for _, ctr := range ctrs {
		info, err := ctr.Info(ctx)
		if err != nil {
			c.logger.Warn("Failed to get container info", zap.String("id", ctr.ID()), zap.Error(err))
			continue
		}

		appVersion := info.Labels[labelKeyAppVersion]
		runningState := agentpb.AppRunningState_STOPPED
		if task, err := ctr.Task(ctx, nil); err == nil {
			if st, err := task.Status(ctx); err == nil && st.Status == containerd.Running {
				runningState = agentpb.AppRunningState_RUNNING
			}
		}

		var mcpPort uint32
		if portStr, ok := info.Labels[labelKeyMCPPort]; ok && portStr != "" {
			if p, err := strconv.ParseUint(portStr, 10, 32); err == nil {
				mcpPort = uint32(p)
			}
		}

		var httpPort uint32
		if portStr, ok := info.Labels[labelKeyHTTPPort]; ok && portStr != "" {
			if p, err := strconv.ParseUint(portStr, 10, 32); err == nil {
				httpPort = uint32(p)
			}
		}

		// labelKeyAppID is always set by wendyLabels; fall back to container ID
		// for containers created before this label was introduced.
		appID := info.Labels[labelKeyAppID]
		if appID == "" {
			appID = ctr.ID()
		}
		serviceName := info.Labels[labelKeyServiceName]

		svc := serviceEntry{name: serviceName, runningState: runningState}
		exitCode, exitReason, hasExit := parseExitLabels(info.Labels)
		// A container the user deliberately stopped isn't a crash to report,
		// even though its task exited (SIGTERM). The stopped-by-user mark wins.
		if info.Labels[labelKeyStoppedByUser] == "true" {
			hasExit = false
		}

		if e, ok := grouped[appID]; !ok {
			order = append(order, appID)
			ne := &entry{
				version:      appVersion,
				runningState: runningState,
				mcpPort:      mcpPort,
				httpPort:     httpPort,
				services:     []serviceEntry{svc},
			}
			if hasExit {
				ne.exitCode, ne.exitReason = exitCode, exitReason
			}
			grouped[appID] = ne
		} else {
			if runningState == agentpb.AppRunningState_RUNNING {
				e.runningState = agentpb.AppRunningState_RUNNING
			}
			if mcpPort != 0 && e.mcpPort == 0 {
				e.mcpPort = mcpPort
			}
			if httpPort != 0 && e.httpPort == 0 {
				e.httpPort = httpPort
			}
			// Keep the first exit reason seen for the app (multi-service apps
			// aggregate; a single stopped service's cause is better than none).
			if e.exitReason == "" && hasExit {
				e.exitCode, e.exitReason = exitCode, exitReason
			}
			e.services = append(e.services, svc)
		}
	}

	result := make([]*agentpb.AppContainer, 0, len(grouped))
	for _, appID := range order {
		e := grouped[appID]

		// Populate per-service entries for any app that declares named services
		// (single- or multi-service services-map apps). Single-container and
		// flattened single-service apps have an empty service name and leave
		// Services empty so callers can still distinguish them cheaply. Exposing
		// the per-service identity for single-service apps is what lets the
		// monitor reconcile them by their "{appID}_{serviceName}" container name
		// instead of restart-looping a healthy app (WDY-1552).
		var services []*agentpb.ServiceEntry
		hasNamedService := false
		for _, s := range e.services {
			if s.name != "" {
				hasNamedService = true
				break
			}
		}
		if hasNamedService {
			services = make([]*agentpb.ServiceEntry, len(e.services))
			for i, s := range e.services {
				services[i] = &agentpb.ServiceEntry{
					Name:         s.name,
					RunningState: s.runningState,
				}
			}
		}

		ac := &agentpb.AppContainer{
			AppName:      appID,
			AppVersion:   e.version,
			RunningState: e.runningState,
			McpPort:      e.mcpPort,
			HttpPort:     e.httpPort,
			Services:     services,
		}
		// Exit diagnostics are only meaningful for a stopped app; a running app
		// may carry stale labels from a prior run, so don't surface them.
		if e.runningState == agentpb.AppRunningState_STOPPED && e.exitReason != "" {
			ac.ExitCode = e.exitCode
			ac.TerminationReason = e.exitReason
		}
		result = append(result, ac)
	}
	return result, nil
}

// AppDeclaredVolumes maps every Wendy-managed app (bare appID) to the
// persistent volume names its containers declare via persist entitlement
// labels. Names are sanitized the same way applyPersist sanitizes them before
// creating the host directory, so they match the directory names under
// /var/lib/wendy/volumes. Multi-service apps are grouped under their appID
// with the union of all services' declarations.
//
// This is the source of truth for volume ownership: volumes are shared across
// apps by name, so a name may appear under several apps. Containers deployed
// before entitlement labels existed carry no persist labels and contribute
// nothing — callers must treat an app that is absent from the map as
// "ownership unknown", not "owns nothing", and fail safe.
func (c *Client) AppDeclaredVolumes(ctx context.Context) (map[string][]string, error) {
	ctx = c.withNamespace(ctx)

	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppVersion))
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	declared := make(map[string]map[string]bool)
	for _, ctr := range ctrs {
		info, err := ctr.Info(ctx)
		if err != nil {
			// Propagate rather than skip: a container we cannot inspect might
			// declare a volume another app is about to delete, and callers rely
			// on a complete map to protect shared volumes.
			return nil, fmt.Errorf("getting container info for %q: %w", ctr.ID(), err)
		}
		appID := info.Labels[labelKeyAppID]
		if appID == "" {
			appID = ctr.ID()
		}
		for _, ent := range parseEntitlementsFromAnnotations(info.Labels) {
			if ent.Type != appconfig.EntitlementPersist {
				continue
			}
			name := filepath.Base(ent.Name)
			if name == "." || name == ".." || name == "/" || name == "" {
				continue
			}
			if declared[appID] == nil {
				declared[appID] = make(map[string]bool)
			}
			declared[appID][name] = true
		}
	}

	out := make(map[string][]string, len(declared))
	for app, names := range declared {
		list := make([]string, 0, len(names))
		for n := range names {
			list = append(list, n)
		}
		sort.Strings(list)
		out[app] = list
	}
	return out, nil
}

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

// cpuUsageNanos returns cumulative user+sys CPU nanoseconds, clamped at 0.
func cpuUsageNanos(m services.ContainerMetrics) uint64 {
	total := m.UserCPUNanos + m.SysCPUNanos
	if total < 0 {
		return 0
	}
	return uint64(total)
}

// GetResourceStats returns cumulative per-container CPU nanoseconds and current
// memory usage, keyed by container ID (matching GetContainerStats). The client
// computes CPU percentages from deltas between consecutive samples.
func (c *Client) GetResourceStats(ctx context.Context) ([]*agentpb.ResourceContainerStats, error) {
	ctx = c.withNamespace(ctx)

	containers, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppVersion))
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	var result []*agentpb.ResourceContainerStats
	for _, ctr := range containers {
		stat := &agentpb.ResourceContainerStats{AppName: ctr.ID()}
		if task, taskErr := ctr.Task(ctx, nil); taskErr == nil {
			if metric, metErr := task.Metrics(ctx); metErr == nil {
				m := extractContainerMetrics(metric)
				stat.CpuUsageNanos = cpuUsageNanos(m)
				stat.MemoryBytes = m.MemBytes
			}
		}
		result = append(result, stat)
	}
	return result, nil
}

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

// requireDBusProxy enforces the D-Bus sandboxing invariant for WDY-1093: a
// container that declares the bluetooth (D-Bus) entitlement may only start when
// xdg-dbus-proxy is available to scope D-Bus to org.bluez. When the proxy
// manager is absent, starting the container would silently break bluetooth (or,
// in older builds, grant unfiltered system-bus access), so refuse loudly
// instead of degrading silently. Returns nil when it is safe to proceed.
func (c *Client) requireDBusProxy(cfg *appconfig.AppConfig, containerName string) error {
	if hasBluetooth(cfg) && c.proxyManager == nil {
		return fmt.Errorf("cannot start container %q: the bluetooth entitlement requires xdg-dbus-proxy to filter D-Bus access, which is not available on this device", containerName)
	}
	return nil
}
