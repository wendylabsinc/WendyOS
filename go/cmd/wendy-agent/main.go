package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/wendylabsinc/wendy/go/internal/agent/audioloop"
	"github.com/wendylabsinc/wendy/go/internal/agent/bluetooth"
	"github.com/wendylabsinc/wendy/go/internal/agent/cdi"
	"github.com/wendylabsinc/wendy/go/internal/agent/configpartition"
	"github.com/wendylabsinc/wendy/go/internal/agent/container"
	agentcontainerd "github.com/wendylabsinc/wendy/go/internal/agent/containerd"
	agentdata "github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/dbusproxy"
	"github.com/wendylabsinc/wendy/go/internal/agent/framesource"
	"github.com/wendylabsinc/wendy/go/internal/agent/hardware"
	"github.com/wendylabsinc/wendy/go/internal/agent/hostexec"
	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/localsocket"
	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	agentnet "github.com/wendylabsinc/wendy/go/internal/agent/network"
	"github.com/wendylabsinc/wendy/go/internal/agent/networkaccess"
	"github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/agent/registry"
	"github.com/wendylabsinc/wendy/go/internal/agent/robotarm"
	"github.com/wendylabsinc/wendy/go/internal/agent/ros2camera"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"github.com/wendylabsinc/wendy/go/internal/agent/udpmessage"
	"github.com/wendylabsinc/wendy/go/internal/agent/usbgadget"
	"github.com/wendylabsinc/wendy/go/internal/rtps"
	"github.com/wendylabsinc/wendy/go/internal/shared/browseropen"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
)

const (
	defaultAgentPort    = "50051"
	defaultOTELPort     = "4317"
	defaultOTELHTTPPort = "4318"
)

// containerMonitorAdapter satisfies services.ContainerMonitorRegistrar without a
// circular import: container imports services, so we bridge with plain-int policy values
// that mirror container.RestartPolicy.
type containerMonitorAdapter struct {
	m *container.ContainerMonitor
}

func (a *containerMonitorAdapter) Register(appName string, policy int, maxRetries int) {
	var rp container.RestartPolicy
	switch policy {
	case services.RestartPolicyAlways:
		rp = container.RestartAlways
	case services.RestartPolicyUnlessStopped:
		rp = container.RestartUnlessStopped
	case services.RestartPolicyOnFailure:
		rp = container.RestartOnFailure
	default:
		// Unknown or RestartPolicyNo — skip registration.
		return
	}
	a.m.Register(appName, rp, maxRetries)
}

func (a *containerMonitorAdapter) Unregister(appName string) {
	a.m.Unregister(appName)
}

func (a *containerMonitorAdapter) MarkExplicitStop(appName string) {
	a.m.MarkExplicitStop(appName)
}

func (a *containerMonitorAdapter) ClearExplicitStop(appName string) {
	a.m.ClearExplicitStop(appName)
}

// RestartStatuses exposes the monitor's restart bookkeeping so ListContainers
// can report crash-looping apps truthfully (services.RestartStatusProvider).
func (a *containerMonitorAdapter) RestartStatuses() map[string]services.RestartStatus {
	return a.m.RestartStatuses()
}

func main() {
	if handled, code := handleUtilityCommand(os.Args[1:]); handled {
		os.Exit(code)
	}

	logCfg := zap.NewProductionConfig()
	if os.Getenv("WENDY_DEBUG") != "" {
		logCfg = zap.NewDevelopmentConfig()
	}
	logger, err := logCfg.Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize logger: %v\n", err)
		os.Exit(1)
	}
	defer logger.Sync()

	// Create the telemetry broadcaster early so we can tee agent logs into it.
	broadcaster := services.NewTelemetryBroadcaster()

	telemetryBuf := services.NewTelemetryBuffer(services.TelemetryBufferConfig{}, broadcaster, logger)
	if !telemetryBuf.DiskEnabled() {
		logger.Warn("telemetry disk buffer unavailable, falling back to in-memory only")
	}

	// Wrap the logger so agent internal logs are published to the telemetry stream.
	telemetryCore := services.NewTelemetryCore(telemetryBuf, zapcore.DebugLevel)
	logger = zap.New(zapcore.NewTee(logger.Core(), telemetryCore))

	logger.Info("Starting wendy-agent", zap.String("version", version.Version))

	configPath := "/etc/wendy-agent"
	if envPath := os.Getenv("WENDY_CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}
	accessPolicy, err := networkaccess.Load(configPath)
	if err != nil {
		logger.Fatal("Invalid network access configuration", zap.Error(err))
	}
	logger.Info("Agent network access", zap.Bool("allowed", accessPolicy.AllowsNetwork))
	configpartition.ApplyDiscoveryAccess(logger, accessPolicy)

	configpartition.Apply(logger, configPath)

	// Restore the mDNS advertisement for a device renamed via
	// 'wendy device rename'. The boot-time identity units — and
	// configpartition.Apply just above, when the config partition carries a
	// device name — re-derive the name/displayname TXT records from
	// /etc/wendyos/device-name, reverting the rename. Ordered last so the
	// operator's explicit choice wins, matching generate-hostname.sh's
	// precedence for the hostname itself. A no-op on a device that was never
	// renamed.
	services.ReassertHostnameAdvertisement(logger)

	// Advertise the board id, so `wendy discover` can tell a local VM from a
	// device on the sighting alone. A no-op once the record is in place.
	services.EnsureDeviceTypeAdvertisement(logger)

	// Time sync: apply config-partition floor immediately, then start
	// background Roughtime + multicast sync.
	timesyncMgr := timesync.NewManager(logger, configPath)
	timesyncMgr.ApplyFloor()

	// Run the OS-update gate after the time floor is applied so the marker
	// staleness check and the persisted result timestamps use a sane clock.
	services.RunOSUpdateGate(logger)

	services.CleanupOldBackups(logger)
	cdi.EnsureNVIDIACDISpec(logger)
	cdi.EnsureQualcommNPURuntime(logger)

	var networkMgr services.NetworkManager
	if nm := agentnet.NewNMCLINetworkManager(logger); nm != nil {
		networkMgr = nm
	}
	hwDiscoverer := hardware.NewSystemHardwareDiscoverer(logger)
	linkWatcher := bluetooth.NewWatcher(logger)
	btManager := bluetooth.NewManager(logger, bluetooth.WithLinkReporter(linkWatcher))

	var proxyMgr *dbusproxy.Manager
	if dbusproxy.IsAvailable() {
		proxyMgr = dbusproxy.NewManager(logger)
	} else {
		// WDY-1093: without xdg-dbus-proxy there is no way to scope D-Bus to
		// org.bluez, so containers declaring the bluetooth entitlement are
		// refused rather than started with unfiltered access.
		logger.Warn("xdg-dbus-proxy not found; containers with the bluetooth entitlement will be refused")
	}

	// Initialize containerd client (best-effort; may fail on non-Linux or without containerd).
	var containerdClient services.ContainerdClient
	// Declared here (rather than inside the else block below) so it stays in
	// scope down at the mesh dialer / friendly-name resolver wiring, where the
	// provisioning identity needed to build the roster becomes available.
	var meshDNS *mesh.DNSServer
	meshIngress := meshingress.NewRegistry()
	meshCatalog := newMeshCatalogManager(configPath, meshIngress, logger)
	// One sharing manager per catalog: the Linux carrier runner drives it,
	// and the admin service reports its live status on every platform.
	meshSharing := newMeshSharingManager(configPath, meshCatalog, logger)
	containerdAddr := os.Getenv("WENDY_CONTAINERD_ADDR")
	if containerdAddr == "" {
		containerdAddr = agentcontainerd.DefaultAddress
	}
	ctrdClient, ctrdErr := agentcontainerd.NewClient(logger, containerdAddr, proxyMgr)
	if ctrdErr != nil {
		logger.Warn("Failed to connect to containerd (container features will be unavailable)", zap.Error(ctrdErr))
	}
	// Typed separately from containerdClient: assigning a nil *containerd.Client
	// into an interface yields a non-nil interface holding a nil pointer, which
	// panics on first use rather than failing the service's nil check.
	var buildChunkSource services.ChunkSource
	if ctrdErr == nil {
		buildChunkSource = ctrdClient
	}
	if ctrdErr == nil {
		containerdClient = ctrdClient
		defer ctrdClient.Close()
		ctrdClient.SetMeshIngressRegistry(meshIngress)
		ctrdClient.SetMeshMDNSManager(meshCatalog)

		// Inject the shared mesh DNS server so applyMeshEgress/teardownMeshEgress
		// can resolve peer-device names for containers on the mesh network mode.
		meshDNS = mesh.NewDNSServer(logger, "127.0.0.53:53")
		ctrdClient.SetMeshDNS(meshDNS)
	}

	// Ensure the host-side WENDY-MESH iptables chain exists so the wendy-mesh
	// CNI plugin has a chain to append per-container ACCEPT rules into.
	// Best-effort/non-fatal: containers that don't use the mesh network mode
	// must still work even if this fails (non-Linux dev host, missing
	// iptables binary, insufficient privileges, etc).
	if err := hostnetwork.InitMeshChain(); err != nil {
		logger.Warn("failed to init wendy net chain", zap.Error(err))
	}
	// A live app netns can survive agent restart, including its /32 LAN route.
	// Revoke old port grants before any inherited task is reauthorized. Keep
	// destination DROP guards so a stale route cannot fall through FORWARD.
	if runtime.GOOS == "linux" {
		if err := hostnetwork.RevokeOrphanLANServiceAllows(); err != nil {
			logger.Warn("physical LAN service grants could not be revoked; projection disabled", zap.Error(err))
			meshCatalog.DisablePhysicalLANProjection()
		}
	}
	// Same non-fatal treatment for the NAT chain that redirects mesh VIP
	// traffic to the local mesh proxy (started below, once cert material is
	// in scope).
	if err := hostnetwork.InitMeshNATChain(); err != nil {
		logger.Warn("failed to init wendy net nat chain", zap.Error(err))
	}
	meshUDPReady := false
	if runtime.GOOS == "linux" {
		if err := hostnetwork.InitMeshUDPTProxy(mesh.UDPProxyPort); err != nil {
			logger.Warn("net UDP VIP interception unavailable", zap.Error(err))
		} else {
			meshUDPReady = true
		}
	}
	// Same non-fatal treatment for the nat-table chain that forwards the
	// agent's own loopback MeshDial dial into a meshed container's published
	// port (see hostnetwork.MeshPortsChainName).
	if err := hostnetwork.InitMeshPortsChain(); err != nil {
		logger.Warn("failed to init wendy net ports chain", zap.Error(err))
	}

	// Populate the CNI bin dir with symlinks back at this binary so isolated
	// apps' bridge networking self-execs instead of depending on a
	// third-party /opt/cni/bin plugin (see CNIAdd/CNIDel in
	// internal/agent/containerd/cni.go and cniPluginName above). Only
	// meaningful on Linux, where containerd/CNI networking is used.
	if runtime.GOOS == "linux" {
		ensureCNIBinDir(logger)
	}

	logManager := services.NewContainerLogManager(logger, telemetryBuf)

	installer := &services.AgentInstaller{}
	agentSvc := services.NewAgentService(logger, networkMgr, hwDiscoverer, btManager, installer)
	// Pin the executable path while the mount topology this process started
	// under is intact: merging and later removing a driver add-on leaves
	// /proc/self/exe reporting a path that no longer resolves.
	agentSvc.PrimeExecPath()
	agentSvc.WarmBinaryHash()

	var monitor *container.ContainerMonitor
	if containerdClient != nil {
		monitor = container.NewContainerMonitor(logger, containerdClient, logManager, 15*time.Second)
		if ctrdClient != nil {
			// Let the low-level client pause the monitor's restart cycle for a
			// container it is mid-replace/stop on, so a crash-looping app's
			// automatic restart cannot race the kill+delete (WDY debug:
			// "cannot delete running task: failed precondition").
			ctrdClient.SetRestartSuppressor(monitor)
		}
	}

	containerSvcOpts := []services.ContainerServiceOption{
		services.WithLogManager(logManager),
	}
	if monitor != nil {
		containerSvcOpts = append(containerSvcOpts, services.WithMonitor(&containerMonitorAdapter{m: monitor}))
	}
	containerSvc := services.NewContainerService(logger, containerdClient,
		containerSvcOpts...,
	)
	shellSvc := services.NewShellService(logger, hostexec.New())
	audioSvc := services.NewAudioService(logger)

	provisioningSvc := services.NewProvisioningService(logger, configPath, timesyncMgr)
	telemetrySvc := services.NewTelemetryService(logger, broadcaster, telemetryBuf)

	deviceInfoSvc := services.NewDeviceInfoService(logger, hwDiscoverer)
	timeSyncSvc := services.NewTimeSyncService(logger, timesyncMgr)
	wifiSvc := services.NewWiFiService(logger, networkMgr)
	bluetoothSvc := services.NewBluetoothService(logger, btManager)
	agentUpdateSvc := services.NewAgentUpdateService(logger, installer)
	agentUpdateSvc.PrimeExecPath()
	osUpdateSvc := services.NewOSUpdateService(logger, installer)
	driverSvc := services.NewDriverService(logger)
	// Before anything reads the store: devices updated from an older agent still
	// have add-ons in the pre-keyed flat layout.
	driverSvc.MigrateStore()
	containerSvcV2 := services.NewContainerServiceV2(containerSvc)
	provisioningSvcV2 := services.NewProvisioningServiceV2(provisioningSvc)
	audioSvcV2 := services.NewAudioServiceV2(audioSvc)
	telemetrySvcV2 := services.NewTelemetryServiceV2(logger, broadcaster, telemetryBuf)
	// WENDY_DATA_DIR wins outright. Without it the store goes on the data
	// partition when /var/lib/wendy is a separate, larger filesystem than the
	// one holding /var/lib/wendy-agent, and stays there otherwise; the log says
	// which and why.
	dataRoot := os.Getenv("WENDY_DATA_DIR")
	dataManager, err := agentdata.NewManager(dataRoot)
	if err != nil {
		logger.Fatal("Failed to initialize episode data manager", zap.Error(err))
	}
	dataStore := dataManager.StoreLocation()
	logger.Info("Wendy Data episode store",
		zap.String("path", dataStore.Path),
		zap.String("campaigns", dataStore.CampaignDir),
		zap.String("inference", dataStore.InferenceDir),
		zap.String("reason", dataStore.Reason),
		zap.String("campaignsReason", dataStore.CampaignReason),
		zap.String("inferenceReason", dataStore.InferenceReason))
	if msg := dataStore.LeftBehindMessage(); msg != "" {
		logger.Warn(msg, zap.Int("episodes", dataStore.LeftBehind), zap.Int64("bytes", dataStore.LeftBehindBytes),
			zap.Int("notUploaded", dataStore.LeftBehindNotUploaded))
	}
	dataManager.SetConsensusProvider(func(ctx context.Context) (timesync.Consensus, error) {
		return timesync.QueryConsensus(ctx, timesync.Servers)
	})
	dataManager.SetWarnLogger(func(msg string) { logger.Warn(msg) })
	// Episode store bounds. The enforced quota is the smaller of a fifth of the
	// data filesystem and this cap, and eviction preserves the reserve as free
	// space. A device whose data partition wants different bounds sets these
	// rather than being stuck with the built-in numbers.
	dataManager.SetQuota(
		envBytes(logger, "WENDY_DATA_MAX_BYTES", agentdata.DefaultMaxQuotaBytes, 1),
		envBytes(logger, "WENDY_DATA_RESERVE_BYTES", agentdata.DefaultReserveBytes, 0),
	)
	dataSvc := services.NewDataService(dataManager)
	dataSvc.SetAudioService(audioSvc)
	// ROS 2 inspection requires the containerd-backed sidecar runtime; the
	// service is only registered when containerd connected (WDY-1332).
	var ros2Svc *services.ROS2Service
	if ctrdClient != nil {
		ros2Svc = services.NewROS2Service(logger, ctrdClient, agentcontainerd.ROS2BagDir)
		dataSvc.SetROS2Service(ros2Svc)
	}

	// OTEL receivers.
	otelLogReceiver := services.NewOTELLogsReceiver(telemetryBuf)
	otelMetricReceiver := services.NewOTELMetricsReceiver(telemetryBuf)
	otelTraceReceiver := services.NewOTELTraceReceiver(telemetryBuf)

	// Optional startup work can block after resource-owning workers begin.
	// Handle termination now so those workers can drain during startup too.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	// A verified completed reset, including a recovered pending reset, has
	// already erased its credentials. Reclaim owned kernel policy before
	// exposing its receipt. A local-only reset has no receipt but leaves a
	// verified completion marker. A fresh unenrolled host has neither and
	// must not require mesh firewall tools just to begin enrollment.
	if resetStatus, err := provisioningSvcV2.IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{}); err == nil {
		completed := len(resetStatus.GetNotProvisioned().GetUnenrollmentCompletion()) != 0 || provisioningSvc.LocalResetCompleted()
		recoverCompletedResetMesh(ctx, completed, 5*time.Second,
			cleanupResetMesh, provisioningSvc.SetResetCleanupError, func(err error) {
				logger.Error("Reset net cleanup incomplete; completion and enrollment remain unavailable", zap.Error(err))
			})
	}
	defer cancel()
	var meshUDPPolicyDone <-chan struct{}
	if meshUDPReady {
		meshUDPPolicyDone = repairMeshUDPPolicy(ctx, 5*time.Second, hostnetwork.EnsureMeshUDPTProxyPolicy, func(err error) {
			logger.Warn("net UDP VIP policy repair failed", zap.Error(err))
		})
	}

	// Keep the chunk store bounded (WDY-3212, WDY-3217). This retires the
	// staging left by the previous run, so it must precede serving any RPC.
	if ctrdErr == nil {
		ctrdClient.StartChunkStoreMaintenance(ctx)
	}

	// The video service is constructed before the app socket managers because it
	// owns the camera producer shared by capture, inference, and app loopback
	// nodes, so it must exist before the capture adapters are registered.
	discoveryPool := rtps.NewPool()
	defer discoveryPool.Close()
	var videoROSRuntime []services.ROS2Runtime
	if ctrdClient != nil {
		videoROSRuntime = append(videoROSRuntime, ctrdClient)
	}
	videoSvc := services.NewVideoService(ctx, logger, discoveryPool, videoROSRuntime...)
	if err := videoSvc.LoadCameraDHCPAllowlist(filepath.Join(configPath, "camera-dhcp-interfaces")); err != nil {
		logger.Warn("camera DHCP disabled: invalid allowlist", zap.Error(err))
	}
	dataSvc.SetVideoService(videoSvc)
	// Arm pre-roll for campaigns deployed in a previous agent lifetime so their
	// next trigger opens BEFORE the trigger instant, not only campaigns deployed
	// during this run.
	dataSvc.ReconcileArming(ctx)
	defer videoSvc.Shutdown()

	notificationSender := services.NewCloudNotificationSender(logger, provisioningSvc)
	stopInference := dataSvc.StartCampaignInference(ctx, &inference.ManagedFactory{Root: dataManager.InferenceDirectory()}, &services.CampaignCloudSender{Cloud: notificationSender})
	defer stopInference()
	systemAPISocketManager := services.NewAppSystemAPISocketManager(ctx, logger, notificationSender)
	appDataSocketManager := services.NewAppDataSocketManager(ctx, logger, dataManager)
	if ctrdClient != nil {
		ctrdClient.SetAppSystemAPISocketProvider(systemAPISocketManager)
		ctrdClient.SetAppDataSocketProvider(appDataSocketManager)
		ctrdClient.RestoreAppSystemAPISockets(ctx)
	}

	go timesyncMgr.RunDirect(ctx)
	if accessPolicy.AllowsNetwork {
		go timesyncMgr.RunMulticast(ctx)
	}

	startROS2BatteryMonitor(ctx, logger, configPath, discoveryPool)

	// Calibrated frames are the other thing this device can publish from a
	// camera: colour plus depth aligned to it, with the scale and intrinsics
	// that make both measurable. It is a separate service, not a mode of
	// StreamVideo, because a measurement is not a picture. The RealSense
	// source is native — calibration over UVC controls, Z16 via the agent's
	// own capture loop, colour borrowed from the shared camera hub — so there
	// is nothing to install beyond this binary.
	calibratedFrameSvc := services.NewCalibratedFrameService(ctx, logger,
		framesource.NewRegistry(services.NewRealSenseProvider(logger, videoSvc, "")))
	defer calibratedFrameSvc.Shutdown()
	if metadataStore, err := services.OpenCameraMetadataStore("/var/lib/wendy/camera-metadata.json"); err != nil {
		logger.Error("camera metadata unavailable", zap.Error(err))
	} else {
		calibratedFrameSvc.SetMetadataStore(metadataStore)
	}
	// Network cameras have to be found before they can be listed, so probe
	// periodically rather than only when a client asks.
	videoSvc.StartDiscovery()

	// Wire videoSvc as the camera-loopback provider for entitled containers
	// (Task C6). Built after ctrdClient, so this must be a separate wiring
	// step rather than an argument at construction. The immediate sync
	// reconciles camera-loopback state against whatever containers are
	// already running after an agent restart, mirroring
	// RestoreAppSystemAPISockets above; the periodic sync then catches any
	// drift the create/start/stop/delete lifecycle nudges in the containerd
	// client miss (e.g. a container's task crash-exiting on its own).
	if ctrdClient != nil {
		ctrdClient.SetCameraLoopbackProvider(videoSvc)
		ctrdClient.SyncCameraLoopbacks(ctx)
		go ctrdClient.RunCameraLoopbackSync(ctx, time.Minute)
	}

	// Sensor pairing (Task 8): mounts a remote source device's cameras as
	// local loopback nodes, reusing videoSvc's v4l2loopback manager (MCU band,
	// ipcam.MCUBandStart..MCUBandEnd) so module detection and node paths have
	// one owner. Every pairing shares this one Supervisor because its node-id
	// allocator must be shared across pairings; the dialer is still resolved
	// per-pairing (mcuIdentity below) so each source's mTLS handshake pins
	// that specific source's asset identity.
	//
	// mcuIdentity is read fresh on every dial (never cached) so a certificate
	// issued or rotated while the agent runs (BLE first-boot enrollment,
	// re-provisioning) is picked up without a restart — the same contract as
	// the PushTLS closure in registerAllServices below.
	mcuIdentity := func() (certPEM, chainPEM, keyPEM string) {
		var keyData []byte
		certPEM, chainPEM, keyData = provisioningSvc.ProvisioningCerts()
		keyPEM = string(keyData)
		for i := range keyData {
			keyData[i] = 0
		}
		return certPEM, chainPEM, keyPEM
	}
	robotArmSvc := services.NewRobotArmService(robotarm.NewStore("/var/lib/wendy/robot-arm-bindings.json"), robotarm.Discover)
	sensorStore := mcusource.NewPairingStore("/var/lib/wendy/sensor-pairings.json")
	if err := sensorStore.Load(); err != nil {
		logger.Warn("loading sensor pairing store failed", zap.Error(err))
	}
	sensorTransportFor := func(p mcusource.SensorPairing, addr string) (mcusource.SensorTransport, error) {
		if p.Transport == "wendycom" {
			certPEM, chainPEM, keyPEM := mcuIdentity()
			return mcusource.NewWendyComTransport(logger, certPEM, chainPEM, keyPEM, p, addr)
		}
		if p.Transport == "grpc" {
			certPEM, chainPEM, keyPEM := mcuIdentity()
			return mcusource.NewGRPCTransport(logger, certPEM, chainPEM, keyPEM, p, addr)
		}
		d, err := mcusource.NewMTLSDialer(logger, mcuIdentity)(p)
		if err != nil {
			return nil, err
		}
		return mcusource.NewTCPTransport(d, addr), nil
	}
	audioLoop := audioloop.NewManager(logger)
	// Let `wendy device audio list` name mounted Loopback capture subdevices
	// after the remote source they carry, instead of a generic "plughw:2,1".
	audioSvc.SetSensorLinkNaming(audioLoop.Mounts, sensorStore.NameFor)
	audioSvc.SetSensorLinkPrincipalNaming(sensorStore.NameForKey)
	// The camera hub takes SensorLink frames from this feed instead of reading
	// them back from their v4l2loopback node, which serves one reader at a time
	// and so would lock out any app that opens the camera directly.
	sensorFrames := services.NewSensorFrameFeed()
	videoSvc.SetSensorFrameFeed(sensorFrames)
	sensorSup := mcusource.NewSupervisor(logger, videoSvc.Loopback(), sensorTransportFor, sensorFrames.WrapWriters(ros2camera.NewFrameWriter), audioLoop)
	videoSvc.SetSensorCameraIdentities(sensorSup.CameraIdentities)
	sensorSup.SetSourceResolver(mcusource.NewSourceResolver(meshCatalog.SensorSources))
	sensorRunner := mcusource.NewRunner(logger, sensorSup)
	sensorAgentScope := func() certs.Scope {
		certPEM, _, _ := mcuIdentity()
		scope, _ := certs.ScopeFromCertPEM(certPEM)
		_, scope.OrgID, _, _ = provisioningSvc.ProvisioningInfo()
		return scope
	}
	sensorSvc := services.NewSensorPairingService(logger, sensorStore, sensorAgentScope, sensorRunner.StartDiscovered, sensorRunner.StopPairing, sensorSup.IsPairingConnected)
	// `device pair` lists sources from this device's network, so this device
	// must not offer itself. Its asset ID is read per request because
	// enrollment can assign one while the agent runs.
	sensorSvc.SetSourceDiscovery(browseSensorSources(meshCatalog.SensorSources), func() services.SensorSourceSelf {
		_, _, assetID, _ := provisioningSvc.ProvisioningInfo()
		return services.LocalSensorSourceSelf(assetID)
	})
	_, _, _, unitreeEnrolled := provisioningSvc.ProvisioningInfo()
	unitreeSvc := services.NewUnitreeService(ctx, filepath.Join(configPath, "unitree-pairings.json"), unitreePairingOwner(deviceScope(provisioningSvc), unitreeEnrolled))
	defer unitreeSvc.Close()
	unitreeRobotSvc := services.NewUnitreeRobotService(unitreeSvc, logger)
	if err := unitreeSvc.Restore(); err != nil {
		logger.Warn("loading Unitree pairings failed", zap.Error(err))
	}
	// Boot-resume: every previously paired source reconnects on its own —
	// addr == "" tells the runner to resolve the source's current LAN address
	// by asset id rather than trusting a possibly-stale one from disk.
	for _, p := range sensorStore.List() {
		sensorRunner.Start(p, "")
	}

	bleDispatcher := bluetooth.NewDispatcher(networkMgr, containerdClient, hwDiscoverer, btManager)

	// Returns nil if the PEM data is invalid, which causes the registry to stay HTTP.
	registryTLSConfig := func(certPEM, chainPEM, keyPEM string) *tls.Config {
		tlsConfig, err := mtls.NewTLSConfig(certPEM, chainPEM, keyPEM, nil, certNotBeforeFloor(certPEM))
		if err != nil {
			logger.Error("Failed to build registry TLS config", zap.Error(err))
			return nil
		}
		return tlsConfig
	}

	var (
		registrySrv   *registry.Server
		registrySrvMu sync.Mutex
	)

	// When tlsConfig is non-nil serves HTTPS; nil means plain HTTP (pre-provisioning only).
	startRegistry := func(tlsConfig *tls.Config) {
		registrySrvMu.Lock()
		defer registrySrvMu.Unlock()

		if registrySrv != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := registrySrv.Shutdown(shutdownCtx); err != nil {
				logger.Warn("Registry shutdown error during restart", zap.Error(err))
			}
			registrySrv = nil
		}

		registryAddr, err := accessPolicy.RegistryAddress(os.Getenv("WENDY_REGISTRY_ADDR"))
		if err != nil {
			logger.Error("Invalid registry address", zap.Error(err))
			return
		}

		srv, err := registry.Start(ctx, containerdAddr, registryAddr, logger, tlsConfig)
		if err != nil {
			logger.Warn("Failed to start embedded dev registry (image push will be unavailable)", zap.Error(err))
			return
		}
		registrySrv = srv
	}

	var wg sync.WaitGroup

	if monitor != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			monitor.Run(ctx)
		}()
		// Re-launch apps that should run after a reboot (per their restart
		// policy, minus user-stopped ones) now that the monitor is running.
		// Done in its own goroutine so agent startup isn't blocked on container I/O.
		go monitor.ReconcileBootContainers(ctx)
	}

	if containerdClient != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			services.CollectContainerMetrics(ctx, containerdClient, telemetryBuf, logManager)
		}()
	}

	if ctrdClient != nil {
		if err := ctrdClient.ReapOrphanedROS2Sidecars(ctx); err != nil {
			logger.Warn("ROS 2 sidecar reap on boot failed", zap.Error(err))
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		services.CollectAgentMetrics(ctx, telemetryBuf)
	}()

	// Collect kernel messages from /dev/kmsg as OTel debug/trace logs.
	// Opt-in: set WENDY_COLLECT_DMESG=true to enable. Disabled by default
	// because kernel messages may contain PII (MAC addresses, serial numbers,
	// process names, filesystem paths) that operators must consciously opt into
	// forwarding to their telemetry backend.
	collectDmesgEnv := os.Getenv("WENDY_COLLECT_DMESG")
	collectDmesg, collectDmesgErr := strconv.ParseBool(collectDmesgEnv)
	if collectDmesgEnv != "" && collectDmesgErr != nil {
		logger.Warn("WENDY_COLLECT_DMESG has unrecognised value; dmesg collection disabled",
			zap.String("value", collectDmesgEnv),
		)
	}
	if collectDmesg {
		// Dual-control gate: env-var (WENDY_COLLECT_DMESG) is the first domain;
		// the DPIA confirmation file is the second, filesystem domain. A process
		// with only env-var write access cannot enable collection on its own.
		// CollectDmesgLogs enforces this independently, but the pre-check here
		// makes both controls visible at the callsite and avoids starting a
		// goroutine that would immediately return on DPIA failure.
		// Check both existence and non-empty content to mirror CollectDmesgLogs.
		dpiaContent, dpiaErr := os.ReadFile(services.DmesgDPIAConfirmFile)
		dpiaValid := dpiaErr == nil && len(bytes.TrimSpace(dpiaContent)) > 0
		for i := range dpiaContent {
			dpiaContent[i] = 0
		}
		dpiaContent = nil
		if !dpiaValid {
			logger.Info("kernel dmesg collection skipped: DPIA confirmation file absent or empty",
				zap.String("file", services.DmesgDPIAConfirmFile),
				zap.String("reason", "filesystem-domain gate not satisfied; WENDY_COLLECT_DMESG alone is insufficient"),
			)
		} else {
			logger.Info("kernel dmesg collection enabled", zap.String("source", "/dev/kmsg"))
			wg.Add(1)
			go func() {
				defer wg.Done()
				services.CollectDmesgLogs(ctx, logger, broadcaster)
			}()
		}
	} else {
		logger.Info("kernel dmesg collection disabled (set WENDY_COLLECT_DMESG=true to enable)")
	}

	// mTLS organization-equality enforcement mode. Read once here so the
	// startMTLSServer closure can capture it. The default (empty value) is strict,
	// which rejects client certs that carry no org identity. Set to grace for
	// migration mode, which allows legacy certs without org identity.
	orgMode, ok := interceptor.ParseOrgMode(os.Getenv("WENDY_MTLS_ORG_ENFORCEMENT"))
	if !ok {
		logger.Warn("WENDY_MTLS_ORG_ENFORCEMENT has unrecognised value; defaulting to strict",
			zap.String("value", os.Getenv("WENDY_MTLS_ORG_ENFORCEMENT")))
	}
	logger.Info("mTLS org enforcement mode", zap.String("mode", orgMode.String()))

	// Main agent gRPC server port.
	agentPort := defaultAgentPort
	if p := os.Getenv("WENDY_AGENT_PORT"); p != "" {
		agentPort = p
	}

	// mtlsPortNum is agentPort+1; computed here so startTunnelBroker can capture it.
	agentPortNum, err := strconv.Atoi(agentPort)
	if err != nil {
		logger.Fatal("Invalid agent port", zap.String("port", agentPort), zap.Error(err))
	}
	mtlsPortNum := agentPortNum + 1

	// startTunnelBroker launches the tunnel broker presence loop in the background.
	// ProvisioningInfo() is called inside the goroutine to avoid re-entering the
	// provisioning mutex when called from the OnProvisioned callback.
	startTunnelBroker := func() {
		if !accessPolicy.AllowsNetwork {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cloudHost, _, _, enrolled := provisioningSvc.ProvisioningInfo()
			if !enrolled {
				return
			}
			if principal := provisioningSvc.ProvisioningPrincipal(); principal != "" {
				endpoint, err := cloudrelay.DeviceEndpoint(cloudHost, os.Getenv("WENDY_DEVICE_CLOUD_URL"))
				if err != nil {
					logger.Error("invalid device Cloud endpoint", zap.Error(err))
					return
				}
				issuer, err := cloudrelay.Issuer(cloudHost, os.Getenv("WENDY_CLOUD_GRANT_ISSUER"))
				if err != nil {
					logger.Error("invalid Cloud grant issuer", zap.Error(err))
					return
				}
				client := &cloudrelay.Agent{Endpoint: endpoint, Verifier: &cloudrelay.Verifier{Issuer: issuer},
					StateDir: filepath.Join(configPath, "cloud-relay"), Credentials: provisioningSvc.ProvisioningCerts,
					Logger: logger, MTLSPort: mtlsPortNum}
				client.Run(ctx)
				return
			}
			logger.Warn("Cloud relay requires a Cloud v2 device principal; re-enroll this device")
		}()
	}

	var mtlsServer *grpc.Server
	var mtlsMu sync.Mutex

	// Declared here, assigned further down once the provisioning identity is
	// available, because registerAllServices closes over it: the build service
	// dials peers through it to deliver a finished image. Every call site of
	// registerAllServices runs after the assignment; if that ever stops being
	// true, BuildImage reports "no mesh dialer" rather than panicking.
	var meshDialer *services.MeshDialer
	var meshRoster *services.MeshRoster
	var localMeshMu sync.RWMutex
	var localMeshSnapshot func() localmesh.NodeSnapshot
	// All listeners extract into the same per-app context directories. Share
	// their locks so a local-socket build cannot race an mTLS build for the same
	// app and replace its source tree while buildctl is reading it.
	buildContextLocks := services.NewBuildContextLockSet()

	// Share the read-modify-write lock across listener creation and replacement.
	// Local mesh administration is authorized against this device's own
	// tenant, so the identity is re-read per call: enrollment can replace
	// it while the agent runs.
	localMeshAdmin := services.NewLocalMeshAdminService(logger, configPath, func() (meshid.Tenant, meshid.Device) {
		return deviceIdentity(provisioningSvc)
	})
	localMeshAdmin.SetRuntimeStatusSource(meshSharing.Status)
	localMeshAdmin.SetTopologySource(func() localmesh.NodeSnapshot {
		localMeshMu.RLock()
		snapshot := localMeshSnapshot
		localMeshMu.RUnlock()
		if snapshot == nil {
			return localmesh.NodeSnapshot{}
		}
		return snapshot()
	})

	registerAllServices := func(srv *grpc.Server) {
		// Each authenticated listener is created with the device's current tenant.
		// The app RPC independently requires a same-tenant device principal and
		// refuses admission when this agent has no enrolled tenant.
		meshSvc := services.NewMeshService(logger, configPath, deviceScope(provisioningSvc))
		meshSvc.SetIngressAuthorizer(meshIngress)
		buildSvc := services.NewBuildService(logger, services.BuildServiceOptions{
			ConfigPath:   configPath,
			Chunks:       buildChunkSource,
			Peers:        meshDialer,
			ContextLocks: buildContextLocks,
			// Legacy fallback for a CLI that does not put each target's actual
			// mTLS agent port in PushTarget.
			TargetAgentPort: uint16(mtlsPortNum),
			// Reported with the build capabilities, so a device reachable only
			// over USB says it has no internet before a build fails on it.
			CheckInternet: services.CheckRegistryReachable,
			// The Cloud asset UUID is the device UUID. Still require membership
			// in this tenant's roster before delivery. Read fresh per build:
			// roster sync and enrollment can change membership while running.
			ResolveTarget: func(assetUUID string) (services.DeliveryTarget, error) {
				if meshRoster == nil {
					return services.DeliveryTarget{}, fmt.Errorf("this agent has no wendy net roster yet; cannot resolve asset %s", assetUUID)
				}
				entry, ok := meshRoster.LookupAsset(assetUUID)
				if !ok {
					return services.DeliveryTarget{}, fmt.Errorf("asset %s is not in this tenant's wendy net roster", assetUUID)
				}
				device, ok := entry.Device()
				if !ok {
					return services.DeliveryTarget{}, fmt.Errorf("asset %s has no wendy net device identity", assetUUID)
				}
				return services.DeliveryTarget{Device: device, AssetUUID: entry.AssetUUID}, nil
			},
			// Read fresh per build rather than captured: a certificate rotated
			// while the agent runs must be picked up without a restart.
			//
			// ExpectingDevice, not the plain client config: the plain one validates
			// the chain but skips hostname verification (device certs carry wendy
			// URN SANs, not DNS names), so any org-issued certificate could
			// terminate the registry hop and receive the image. The mesh LAN path
			// picks its peer from an unauthenticated mDNS TXT record, which is the
			// spoofing gap this helper was written for.
			PushTLS: func(device meshid.Device) (*tls.Config, error) {
				certPEM, chainPEM, keyData := provisioningSvc.ProvisioningCerts()
				return mtls.NewClientTLSConfigExpectingDevice(certPEM, chainPEM, string(keyData), logger, device)
			},
		})

		agentpb.RegisterWendyAgentServiceServer(srv, agentSvc)
		agentpb.RegisterWendyContainerServiceServer(srv, containerSvc)
		agentpb.RegisterWendyAudioServiceServer(srv, audioSvc)
		agentpb.RegisterWendyVideoServiceServer(srv, videoSvc)
		agentpb.RegisterWendyProvisioningServiceServer(srv, provisioningSvc)
		agentpb.RegisterWendyTelemetryServiceServer(srv, telemetrySvc)
		agentpbv2.RegisterWendyDeviceInfoServiceServer(srv, deviceInfoSvc)
		agentpbv2.RegisterWendyTimeSyncServiceServer(srv, timeSyncSvc)
		agentpbv2.RegisterWendyWiFiServiceServer(srv, wifiSvc)
		agentpbv2.RegisterWendyBluetoothServiceServer(srv, bluetoothSvc)
		agentpbv2.RegisterWendyAgentUpdateServiceServer(srv, agentUpdateSvc)
		agentpbv2.RegisterWendyOSUpdateServiceServer(srv, osUpdateSvc)
		agentpbv2.RegisterWendyContainerServiceServer(srv, containerSvcV2)
		agentpbv2.RegisterWendyProvisioningServiceServer(srv, provisioningSvcV2)
		agentpbv2.RegisterWendyAudioServiceServer(srv, audioSvcV2)
		agentpbv2.RegisterWendyTelemetryServiceServer(srv, telemetrySvcV2)
		agentpbv2.RegisterDataServiceServer(srv, dataSvc)
		agentpbv2.RegisterWendyMeshServiceServer(srv, meshSvc)
		agentpbv2.RegisterWendyLocalMeshAdminServiceServer(srv, localMeshAdmin)
		agentpbv2.RegisterWendyBuildServiceServer(srv, buildSvc)
		agentpbv2.RegisterWendySensorPairingServiceServer(srv, sensorSvc)
		agentpbv2.RegisterWendyCalibratedFrameServiceServer(srv, calibratedFrameSvc)
		agentpbv2.RegisterWendyRobotArmServiceServer(srv, robotArmSvc)
		if ros2Svc != nil {
			agentpbv2.RegisterROS2ServiceServer(srv, ros2Svc)
		}
	}

	meshAgentListener := meshoperatorlink.NewAgentListener()
	defer meshAgentListener.Close()

	startMTLSServer := func(certPEM, chainPEM, keyPEM string) {
		mtlsMu.Lock()
		defer mtlsMu.Unlock()

		if mtlsServer != nil {
			logger.Warn("mTLS server already running, skipping")
			return
		}

		floor := certNotBeforeFloor(certPEM)
		if floor.IsZero() && certPEM != "" {
			logger.Warn("Could not extract NotBefore from provisioning cert — NTP clock skew protection is disabled")
		} else if now := time.Now(); !floor.IsZero() && now.Before(floor) {
			logger.Warn("Device clock predates provisioning cert — using cert NotBefore as mTLS time floor; clock will sync when network is available",
				zap.Time("deviceClock", now),
				zap.Time("floor", floor),
				zap.Duration("clockBehindBy", floor.Sub(now)),
			)
		}

		// Derive this device's own organization from its leaf certificate so the
		// mTLS interceptor can enforce org-equality. We deliberately derive from
		// certPEM (the device's own leaf) rather than provisioningSvc.ProvisioningInfo():
		// startMTLSServer is also invoked from inside the OnProvisioned callback,
		// where taking the provisioning mutex would risk re-entrancy (see the comment
		// at the startTunnelBroker closure). Both call sites already pass certPEM.
		expectedScope, haveScope := certs.ScopeFromCertPEM(certPEM)
		effectiveMode := orgMode
		if orgMode != interceptor.OrgModeOff && !haveScope {
			logger.Error("cannot determine device identity scope; refusing to start mTLS server",
				zap.String("configuredMode", orgMode.String()))
			return
		}
		if effectiveMode != interceptor.OrgModeOff {
			logger.Info("mTLS server enforcing tenant",
				zap.String("scope", expectedScope.String()),
				zap.String("mode", effectiveMode.String()))
		}

		srv, err := mtls.NewServer(certPEM, chainPEM, keyPEM, logger, floor, expectedScope, effectiveMode,
			// UnaryMTLSInterceptor and StreamMTLSInterceptor are embedded inside
			// mtls.NewServer and run before these caller-provided interceptors.
			grpc.ChainUnaryInterceptor(interceptor.UnaryErrorInterceptor(logger)),
			grpc.ChainStreamInterceptor(interceptor.StreamErrorInterceptor(logger)),
		)
		if err != nil {
			logger.Error("Failed to create mTLS server", zap.Error(err))
			return
		}

		// Register all services on the mTLS server.
		registerAllServices(srv)
		// Robot pairing transfers keys and initiates LAN connections. Keep the
		// service off the plaintext provisioning listener, even though its
		// handlers also reject unauthenticated transports.
		agentpbv2.RegisterWendyUnitreeServiceServer(srv, unitreeSvc)
		agentpbv2.RegisterWendyUnitreeRobotServiceServer(srv, unitreeRobotSvc)

		// WendyShellService opens an interactive *host* root shell. It is
		// deliberately registered ONLY here, on the mTLS server, so it is
		// reachable only on a provisioned device over an authenticated,
		// org-checked connection. It is intentionally NOT part of
		// registerAllServices: that would also expose it on the unauthenticated
		// plaintext pre-provisioning server (handing anyone on the LAN a host
		// root shell) and on the local admin socket.
		agentpb.RegisterWendyShellServiceServer(srv, shellSvc)

		// WendyTunnelService can reach arbitrary valid UDP ports on agent
		// loopback. Register it only on the authenticated, org-checked server,
		// never on the plaintext provisioning listener or local admin socket.
		agentpbv2.RegisterWendyTunnelServiceServer(srv, services.NewTunnelService(logger))

		// WendyDriverService installs kernel driver add-ons — loading a module is
		// ring-0 code execution, as privileged as the root shell above. So it is
		// registered ONLY here on the mTLS server (authenticated, org-checked),
		// never via registerAllServices: signature verification guards WHAT runs,
		// but the transport must still guard WHO may install or remove a driver.
		// Linux only: the store is systemd-sysext and modprobe. Elsewhere the
		// client sees Unimplemented and treats the device as having no add-ons.
		if runtime.GOOS == "linux" {
			agentpbv2.RegisterWendyDriverServiceServer(srv, driverSvc)
		}

		// Compute mTLS port = agentPort + 1.
		portNum, err := strconv.Atoi(agentPort)
		if err != nil {
			logger.Error("Failed to parse agent port for mTLS", zap.String("port", agentPort), zap.Error(err))
			return
		}
		mtlsPort := strconv.Itoa(portNum + 1)

		lis, err := listenForAccess(accessPolicy, mtlsPort)
		if err != nil {
			logger.Error("Failed to listen on mTLS port", zap.String("port", mtlsPort), zap.Error(err))
			return
		}

		mtlsServer = srv
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Serve(meshAgentListener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
				logger.Error("authenticated mesh gRPC ingress stopped", zap.Error(err))
			}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("mTLS gRPC server listening", zap.String("port", mtlsPort))
			if err := srv.Serve(lis); err != nil {
				logger.Error("mTLS gRPC server error", zap.Error(err))
			}
		}()
	}

	// Only called after provisioning so the cert is available.
	startBLEPeripheral := func(certPEM, chainPEM, keyPEM string) {
		if !accessPolicy.AllowsNetwork {
			return
		}
		tlsConfig, err := mtls.NewTLSConfig(certPEM, chainPEM, keyPEM, logger, certNotBeforeFloor(certPEM))
		if err != nil {
			logger.Error("Failed to build BLE TLS config", zap.Error(err))
			return
		}
		bluetooth.StartBLEPeripheral(ctx, logger, bleDispatcher, tlsConfig)
	}

	// Check if already provisioned and start mTLS server and tunnel broker if certificates exist.
	certPEM, chainPEM, keyData := provisioningSvc.ProvisioningCerts()
	keyPEM := string(keyData)
	for i := range keyData {
		keyData[i] = 0
	}
	alreadyProvisioned := certPEM != "" && keyPEM != ""
	appUDPMessages := udpmessage.NewManager()
	defer appUDPMessages.Close()
	wg.Add(1)
	go func() { defer wg.Done(); logUDPMessageState(ctx, logger, appUDPMessages) }()
	var localMeshLife meshLifetime
	var localMeshAppClient *meshsession.Client
	// Persistent VIP table shared by DNS answers, the proxy's reverse
	// lookup, and every allocation: (tenant UUID, device UUID) ↔ VIP. A
	// corrupt table file refuses to open rather than silently re-allocating
	// — recycled VIPs would hijack live flows pinned to their old owners.
	meshVIPs, vipErr := mesh.OpenVIPTable(filepath.Join(configPath, "mesh-vips.json"))
	if vipErr == nil {
		// Enrollment replacement restarts the agent, so retirement must happen
		// on startup too, before any proxy can resolve a persisted old-tenant VIP.
		if tenant, _ := deviceIdentity(provisioningSvc); tenant.IsValid() {
			vipErr = meshVIPs.RetireTenant(tenant)
			if vipErr != nil {
				meshVIPs = nil
			}
		}
	}
	if vipErr != nil {
		logger.Error("wendy net: VIP table could not be opened; wendy net name answers and proxy dialing are disabled", zap.Error(vipErr))
	} else {
		stats := meshVIPs.Stats()
		logger.Info("wendy net: VIP table loaded", zap.Int("allocated", stats.Allocated), zap.Int("retired", stats.Retired), zap.Int("capacity", stats.Capacity))
	}
	sessionIngress := meshsession.WithAgentService(meshIngress, uint16(mtlsPortNum))
	startConfiguredLocalMesh := func(certPEM, chainPEM, keyPEM string) {
		if runtime.GOOS != "linux" {
			return
		}
		// Enrollment starts the supervisor even before the first wendy net configure.
		// The participation policy keeps its carrier runner inert until opt-in.
		credentials, err := localmesh.NewCredentials(certPEM, chainPEM, keyPEM)
		if err != nil {
			logger.Error("wendy net identity unavailable", zap.Error(err))
			return
		}
		localMeshLife.Start(ctx, func(meshCtx context.Context, launch func(func())) {
			meshCtx, stopSessions := context.WithCancel(meshCtx)
			appClient, err := meshsession.NewClient(credentials, appUDPMessages)
			if err != nil {
				logger.Error("wendy net app client unavailable", zap.Error(err))
				stopSessions()
				return
			}
			localMeshMu.Lock()
			localMeshAppClient = appClient
			localMeshMu.Unlock()
			launch(func() { <-meshCtx.Done(); _ = appClient.Close() })
			launch(func() {
				ticker := time.NewTicker(time.Second)
				defer ticker.Stop()
				for {
					select {
					case <-meshCtx.Done():
						return
					case <-ticker.C:
					}
					localMeshMu.RLock()
					snapshot := localMeshSnapshot
					localMeshMu.RUnlock()
					if snapshot == nil {
						appClient.PruneRoutes(localmesh.NodeSnapshot{})
					} else {
						appClient.PruneRoutes(snapshot())
					}
				}
			})
			sessionServer, err := meshsession.NewServer(credentials, sessionIngress, appUDPMessages)
			if err != nil {
				logger.Error("wendy net app session unavailable", zap.Error(err))
				stopSessions()
				return
			}
			// Publish app capabilities only after both sockets have bound. A
			// listener failure ends the carriers and their signed advertisement.
			ready := make(chan struct{}, 2)
			onReady := func() { ready <- struct{}{} }
			launch(func() {
				defer stopSessions()
				if err := sessionServer.RunTLS(meshCtx, fmt.Sprintf(":%d", meshsession.TLSPort), onReady); err != nil && meshCtx.Err() == nil {
					logger.Error("wendy net TLS app session stopped", zap.Error(err))
				}
			})
			launch(func() {
				defer stopSessions()
				if err := sessionServer.Run(meshCtx, fmt.Sprintf(":%d", meshsession.Port), onReady); err != nil && meshCtx.Err() == nil {
					logger.Error("wendy net app session stopped", zap.Error(err))
				}
			})
			for range 2 {
				select {
				case <-ready:
				case <-meshCtx.Done():
					return
				}
			}
			if meshCtx.Err() != nil {
				return
			}
			name, _ := os.Hostname()
			launch(func() {
				runMeshSupervisor(meshCtx, 5*time.Second, func(ctx context.Context) error {
					return runConfiguredNetCarriers(ctx, configPath, localmesh.TCPIdentity{
						Name: name, AgentPort: uint16(mtlsPortNum),
						Agent:        services.MeshAgentMetadata(),
						AppEndpoints: []localmesh.AppEndpoint{{Transport: localmesh.AppQUIC, Port: meshsession.Port}, {Transport: localmesh.AppTLS, Port: meshsession.TLSPort}},
						Certificate:  certPEM, Chain: chainPEM, Key: keyPEM,
					}, logger, meshCatalog, meshVIPs, meshSharing, func(snapshot func() localmesh.NodeSnapshot) {
						localMeshMu.Lock()
						localMeshSnapshot = snapshot
						localMeshMu.Unlock()
					}, meshAgentListener.Deliver)
				}, func(err error) {
					logger.Error("configured wendy net stopped", zap.Error(err))
				})
			})
		})
	}

	// Client side of the mesh data plane: dials peers LAN-direct or via the
	// cloud tunnel broker, fed by whatever REDIRECTed VIP connections the nat
	// chain above sends to ProxyPort. Constructed here — the first point where
	// cert material (certPEM/chainPEM/keyPEM, just above) and provisioning
	// info are both in scope — rather than gated on alreadyProvisioned: on an
	// unenrolled device the cert fields are empty, so DialDevice simply
	// fails closed at runtime with a clear error instead of never starting.
	cloudHost, orgID, assetID, _ := provisioningSvc.ProvisioningInfo()
	var activeProvisioningIdentity provisioningIdentityTracker
	if alreadyProvisioned {
		activeProvisioningIdentity.Changed(certPEM, chainPEM, keyPEM, orgID, assetID)
		startConfiguredLocalMesh(certPEM, chainPEM, keyPEM)
	}
	meshMetrics := services.NewMeshMetrics(telemetryBuf, logger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		meshMetrics.Collect(ctx)
	}()
	meshDialer = services.NewMeshDialer(logger, certPEM, keyPEM, chainPEM, meshMetrics)
	meshDialer.SetSelfDialer(func(dialCtx context.Context, port uint16) (net.Conn, error) {
		bounded, cancel := context.WithTimeout(dialCtx, 5*time.Second)
		defer cancel()
		return sessionIngress.DialAuthorized(port, func(address string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(bounded, "tcp4", address)
		})
	})
	meshDialer.SetLocalMeshDialer(func(dialCtx context.Context, peer meshid.Device, port uint16) (net.Conn, error) {
		localMeshMu.RLock()
		snapshot := localMeshSnapshot
		appClient := localMeshAppClient
		localMeshMu.RUnlock()
		if snapshot == nil || appClient == nil {
			return nil, meshsession.ErrNoRoute
		}
		view := snapshot()
		if address, native, err := meshsession.ResolveAgentMTLS(view, peer, port); native {
			if err != nil {
				return nil, err
			}
			return (&net.Dialer{}).DialContext(dialCtx, "tcp6", address.String())
		}
		address, err := meshsession.ResolveApp(view, peer)
		if err != nil {
			appClient.WithdrawRoute(peer)
			return nil, err
		}
		if address.Transport == localmesh.AppTLS {
			return appClient.DialTLS(dialCtx, peer, address.Address, port)
		}
		return appClient.DialWithRetryCheck(dialCtx, peer, address.Address, port, func() error {
			localMeshMu.RLock()
			currentSnapshot := localMeshSnapshot
			localMeshMu.RUnlock()
			if currentSnapshot == nil {
				return meshsession.ErrNoRoute
			}
			currentAddress, err := meshsession.ResolveApp(currentSnapshot(), peer)
			if err != nil {
				return err
			}
			if currentAddress != address {
				return meshsession.ErrNoRoute
			}
			return nil
		})
	})
	meshDialer.SetLocalServiceDialer(func(dialCtx context.Context, peer meshid.Device, port uint16) (net.Conn, error) {
		localMeshMu.RLock()
		snapshot := localMeshSnapshot
		localMeshMu.RUnlock()
		if snapshot == nil {
			return nil, meshsession.ErrNoRoute
		}
		if _, err := meshsession.Resolve(snapshot(), peer); err != nil {
			return nil, err
		}
		return (&net.Dialer{}).DialContext(dialCtx, "tcp6", net.JoinHostPort(peer.MeshAddress().String(), strconv.Itoa(int(port))))
	})
	// VIP dialing is mesh-local only. Without a configured local mesh the
	// app dialer fails closed with ErrNoRoute rather than falling back to
	// the asset-addressed cloud relay, which carries no device-principal
	// app admission.
	// Keep the dialer attached across first enrollment/configuration. Its local
	// callback resolves the current runtime on every dial and fails closed while
	// no admitted route exists; a boot-time file check would disable it forever.
	appDialer := services.NewMeshAppDialer(meshDialer)
	meshProxy := mesh.NewProxy(logger, appDialer, meshMetrics, meshVIPs)
	// The proxy listens on bridge addresses because container REDIRECT traffic
	// cannot reach a loopback-only listener. It accepts only mesh VIP targets;
	// the application dialer still enforces device-principal admission.
	if err := meshProxy.Start(fmt.Sprintf(":%d", mesh.ProxyPort)); err != nil {
		logger.Warn("wendy net proxy failed to start; wendy net egress disabled", zap.Error(err))
	}
	udpProxy, udpErr := mesh.NewUDPProxy(mesh.UDPDialFunc(func(dialCtx context.Context, request mesh.UDPDialRequest) (mesh.UDPFlow, error) {
		peer, port := request.Peer, request.Port
		options := meshsession.UDPOptions{Manager: appUDPMessages, Endpoint: request.Endpoint, Done: request.Done, Source: &request.Source}
		if meshDialer.IsSelf(peer) {
			return meshsession.DialLocalUDP(dialCtx, meshIngress, port, options)
		}
		localMeshMu.RLock()
		snapshot := localMeshSnapshot
		appClient := localMeshAppClient
		localMeshMu.RUnlock()
		if snapshot == nil || appClient == nil {
			return nil, meshsession.ErrNoRoute
		}
		address, err := meshsession.ResolveApp(snapshot(), peer)
		if err != nil {
			appClient.WithdrawRoute(peer)
			return nil, err
		}
		if address.Transport == localmesh.AppTLS {
			return appClient.DialUDPTLS(dialCtx, peer, address.Address, port, options)
		}
		if address.Transport != localmesh.AppQUIC {
			return nil, meshsession.ErrUDPUnsupported
		}
		return appClient.DialUDP(dialCtx, peer, address.Address, port, options)
	}), meshIngress, meshVIPs, appUDPMessages)
	if udpErr == nil {
		udpErr = udpProxy.Start(fmt.Sprintf(":%d", mesh.UDPProxyPort))
	}
	if udpErr != nil {
		logger.Warn("net UDP proxy failed to start; UDP VIP egress disabled", zap.Error(udpErr))
	}

	// Hybrid friendly-name resolver: <devicename>.<org-slug>.cloud.wendy.dev.
	// Mirrors the meshDialer construction just above — not gated on
	// alreadyProvisioned. On an unenrolled device certPEM/keyPEM are empty, so
	// MeshRoster has no device principal to authenticate with and Sync simply
	// fails closed (logged, retried) until the device is provisioned, rather
	// than never starting at all.
	meshRoster = services.NewMeshRoster(logger, cloudGRPCURLForCloudHost(cloudHost), certPEM, keyPEM, chainPEM)
	meshBrowse := func(ctx context.Context) ([]models.LANDevice, error) {
		col, err := discovery.Discover(ctx, discovery.DiscoveryOptions{
			Types:   []models.InterfaceType{models.InterfaceLAN},
			Timeout: 1 * time.Second,
		})
		if err != nil {
			return nil, err
		}
		return col.LANDevices, nil
	}
	meshResolver := services.NewMeshResolver(logger, meshRoster, meshBrowse)
	// The tenant mesh names resolve under follows the roster's provisioned
	// identity; unset until provisioning completes.
	var meshTenant meshid.Tenant
	if tenant, ok := meshRoster.OwnTenant(); ok {
		meshTenant = tenant
	}
	if meshDNS != nil {
		meshDNS.SetResolver(meshResolver)
		if meshVIPs != nil && meshTenant != (meshid.Tenant{}) {
			meshDNS.SetMeshIdentity(meshTenant, meshVIPs)
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		meshRoster.RunSync(ctx, 5*time.Minute)
	}()

	// Report every app's state to cloud (apps spec section 2). Like the
	// roster it starts unconditionally and stays silent until enrolled.
	var reportedState *services.ReportedState
	var postbox *services.Postbox
	if ctrdErr == nil {
		reportedState = services.NewReportedState(logger, ctrdClient, telemetryBuf, cloudGRPCURLForCloudHost(cloudHost), certPEM, keyPEM, chainPEM)
		// The operator-signed postbox (AAA contract section 5.3): woken by the
		// reported-state nudge, it applies only what it verifies itself.
		postbox = services.NewPostbox(logger, telemetryBuf, configPath, services.NewPostboxControl(containerSvc))
		postbox.UpdateIdentity(cloudGRPCURLForCloudHost(cloudHost), certPEM, keyPEM, chainPEM)
		reportedState.AttachPostbox(postbox)
		wg.Add(2)
		go func() {
			defer wg.Done()
			reportedState.Run(ctx)
		}()
		go func() {
			defer wg.Done()
			postbox.Run(ctx)
		}()
	}

	if alreadyProvisioned {
		startMTLSServer(certPEM, chainPEM, keyPEM)
		startTunnelBroker()
		configpartition.PersistDeviceUUID(logger, certPEM)
		advertiseProvisionedMeshIdentity(logger, mtlsPortNum, provisioningSvc)
		startBLEPeripheral(certPEM, chainPEM, keyPEM)
	}

	// Advertise the device's Ethernet addresses so hosts can prefer the cable.
	wg.Add(1)
	go func() {
		defer wg.Done()
		services.RunWiredAdvertisement(ctx, logger)
	}()

	// Start the embedded dev container registry (Linux only, best-effort).
	// If already provisioned, start immediately with HTTPS; otherwise HTTP until provisioned.
	if runtime.GOOS == "linux" && ctrdErr == nil {
		if alreadyProvisioned {
			startRegistry(registryTLSConfig(certPEM, chainPEM, keyPEM))
		} else {
			startRegistry(nil)
		}
	}

	// Plaintext gRPC server — only needed until the device is provisioned.
	// Once provisioned, the mTLS server handles remote gRPC traffic and this
	// plaintext port is shut down. The local unix socket (/run/wendy/agent.sock)
	// remains active for on-device containers with the admin entitlement.
	var agentServer *grpc.Server
	if !alreadyProvisioned {
		agentServer = grpc.NewServer(
			grpc.UnaryInterceptor(interceptor.UnaryErrorInterceptor(logger)),
			grpc.StreamInterceptor(interceptor.StreamErrorInterceptor(logger)),
			grpc.InitialWindowSize(8*1024*1024),
			grpc.InitialConnWindowSize(16*1024*1024),
			grpc.KeepaliveParams(keepalive.ServerParameters{
				MaxConnectionIdle: 5 * time.Minute,
				Time:              30 * time.Second,
				Timeout:           20 * time.Second,
			}),
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
				MinTime:             10 * time.Second,
				PermitWithoutStream: true,
			}),
		)
		registerAllServices(agentServer)

		agentLis, err := listenForAccess(accessPolicy, agentPort)
		if err != nil {
			logger.Fatal("Failed to listen on agent port", zap.String("port", agentPort), zap.Error(err))
		}

		// Avahi's service file survives agent restarts. Reconcile it after
		// binding the setup listener, including when stored enrollment has
		// lost its key or certificate. This only changes discovery metadata;
		// the enrollment record is retained for recovery.
		configpartition.UpdateAvahiForUnprovisioning(logger, agentPortNum)

		wg.Add(1)
		go func() {
			defer wg.Done()
			logger.Info("Agent gRPC server listening", zap.String("port", agentPort))
			if err := agentServer.Serve(agentLis); err != nil {
				logger.Error("Agent gRPC server error", zap.Error(err))
			}
		}()
	}

	// Keep the USB gadget interface reachable at the well-known link-local
	// address the CLI dials directly (no mDNS/DHCP needed over USB-C).
	if accessPolicy.AllowsNetwork {
		go usbgadget.EnsureWellKnownAddress(ctx, 30*time.Second, logger)
	}

	// Local control socket: the agent's full gRPC over a unix socket with NO
	// mTLS. Access is gated solely by the admin entitlement (oci.applyAdmin
	// bind-mounts this socket into entitled containers). Disabled with
	// WENDY_LOCAL_SOCKET=off.
	var localSocketServer *grpc.Server
	if os.Getenv("WENDY_LOCAL_SOCKET") != "off" {
		localSocketServer = grpc.NewServer(
			grpc.UnaryInterceptor(interceptor.UnaryErrorInterceptor(logger)),
			grpc.StreamInterceptor(interceptor.StreamErrorInterceptor(logger)),
		)
		registerAllServices(localSocketServer)
		agentpbv2.RegisterWendyUnitreeServiceServer(localSocketServer, unitreeSvc)
		agentpbv2.RegisterWendyUnitreeRobotServiceServer(localSocketServer, unitreeRobotSvc)

		// oci.AdminAgentSocketHostPath is the single source of truth for this
		// path: the admin entitlement bind-mounts its parent directory into
		// containers (see oci.applyAdmin), so the path we serve and the path we
		// mount must never drift. It lives on the disk-backed /var/lib/wendy
		// tree, not tmpfs /run, so its directory inode survives reboots — a /run
		// path was wiped on every boot and stranded long-lived admin containers
		// on the orphaned pre-reboot inode (in-container socket read as ENOENT).
		localSocketPath := oci.AdminAgentSocketHostPath
		localLis, err := localsocket.Listen(localSocketPath)
		if err != nil {
			logger.Error("Failed to listen on local control socket", zap.Error(err))
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				logger.Info("Agent local control socket listening", zap.String("path", localSocketPath))
				if err := localSocketServer.Serve(localLis); err != nil {
					logger.Error("Local control socket server error", zap.Error(err))
				}
			}()
		}
	}

	// Set up the provisioning callback to start the mTLS server, shut down
	// the plaintext server, and switch the registry to HTTPS.
	provisioningSvc.OnProvisioned = func(certPEM, chainPEM string, keyData []byte) {
		// Bind to the callback's authenticated leaf before exposing a new tenant.
		unitreeScope, _ := certs.ScopeFromCertPEM(certPEM)
		if err := unitreeSvc.Rebind(unitreePairingOwner(unitreeScope, true)); err != nil {
			logger.Error("Clearing previous enrollment's Unitree pairings failed", zap.Error(err))
		}
		defer func() {
			for i := range keyData {
				keyData[i] = 0
			}
		}()
		keyPEM := string(keyData)
		cloudHost, orgID, assetID, _ := provisioningSvc.ProvisioningInfo()
		if activeProvisioningIdentity.Changed(certPEM, chainPEM, keyPEM, orgID, assetID) {
			// The persisted identity is already durable. Let the provisioning RPC
			// finish, then restart so the app server, pool, and every carrier
			// replace their credentials together. Cancellation enters normal shutdown,
			// draining workers and deferred network cleanup before main returns.
			logger.Info("Provisioning identity rotated; restarting agent")
			time.AfterFunc(5*time.Second, cancel)
			return
		}
		startMTLSServer(certPEM, chainPEM, keyPEM)
		startTunnelBroker()
		startConfiguredLocalMesh(certPEM, chainPEM, keyPEM)
		// Refresh the mesh dialer with the fresh identity — like the mTLS
		// server and tunnel broker above, it consumes cert material, and BLE
		// first-boot enrollment happens while the agent is running, so the
		// boot-time snapshot is empty on virtually every new device.
		meshDialer.UpdateIdentity(certPEM, keyPEM, chainPEM)
		// Same story for the friendly-name roster/resolver: the boot-time
		// snapshot has no certificate and so no device principal, meaning the
		// roster cannot authenticate a sync and the resolver rejects every LAN
		// peer, until the agent is restarted — so swap in the freshly issued
		// identity here.
		meshRoster.UpdateIdentity(cloudGRPCURLForCloudHost(cloudHost), certPEM, keyPEM, chainPEM)
		if reportedState != nil {
			reportedState.UpdateIdentity(cloudGRPCURLForCloudHost(cloudHost), certPEM, keyPEM, chainPEM)
			postbox.UpdateIdentity(cloudGRPCURLForCloudHost(cloudHost), certPEM, keyPEM, chainPEM)
		}
		// The freshly issued identity may name a different tenant than the
		// boot snapshot. Retire every VIP slot outside the new tenant (they
		// are never re-allocated) and re-point DNS at the new scope.
		if newTenant, ok := meshRoster.OwnTenant(); ok && meshVIPs != nil {
			if err := meshVIPs.RetireTenant(newTenant); err != nil {
				logger.Error("wendy net: retiring VIPs outside the new tenant failed; VIP resolution is disabled", zap.Error(err))
			}
			meshTenant = newTenant
			if meshDNS != nil {
				meshDNS.SetMeshIdentity(meshTenant, meshVIPs)
			}
		}
		// Resync immediately so friendly names work without waiting for the periodic tick.
		go func() {
			sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if err := meshRoster.Sync(sctx); err != nil {
				logger.Warn("wendy net roster resync after provisioning failed", zap.Error(err))
			}
		}()
		configpartition.PersistDeviceUUID(logger, certPEM)
		advertiseProvisionedMeshIdentity(logger, mtlsPortNum, provisioningSvc)
		startBLEPeripheral(certPEM, chainPEM, keyPEM)
		if agentServer != nil {
			logger.Info("Device provisioned — shutting down plaintext gRPC port", zap.String("port", agentPort))
			go agentServer.GracefulStop()
		}
		if runtime.GOOS == "linux" && ctrdErr == nil {
			go startRegistry(registryTLSConfig(certPEM, chainPEM, keyPEM))
		}
	}

	// Set up the unprovisioning callback: revert the mDNS advertisement to the
	// plaintext port and exit so systemd restarts the agent into unprovisioned
	// mode. A clean restart is simpler and more reliable than tearing down the
	// mTLS server, tunnel broker, BLE peripheral, and HTTPS registry in place.
	// Revoke Unitree sessions and keys before Unprovision reports success.
	// A failed clear is retried when the agent restarts unenrolled.
	provisioningSvc.BeforeUnprovisioned = func() error {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		stopErr := localMeshLife.Stop(stopCtx)
		stopCancel()
		localMeshMu.Lock()
		localMeshSnapshot, localMeshAppClient = nil, nil
		localMeshMu.Unlock()
		meshDialer.UpdateIdentity("", "", "")
		meshRoster.UpdateIdentity("", "", "", "")
		if reportedState != nil {
			reportedState.UpdateIdentity("", "", "", "")
			postbox.UpdateIdentity("", "", "", "")
		}
		_ = meshProxy.Close()
		if udpProxy != nil {
			_ = udpProxy.Close()
		}
		if err := unitreeSvc.Rebind(unitree.PairingOwner{}); err != nil {
			logger.Error("Clearing Unitree pairings during unprovisioning failed", zap.Error(err))
		}
		if stopErr != nil {
			// Do not reclaim kernel objects while an undrained worker may still
			// use them. Reset stays unconfirmed; the scheduled restart retries.
			return fmt.Errorf("waiting for net shutdown: %w", stopErr)
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		return errors.Join(meshSharing.Deactivate(), cleanupResetMesh(cleanupCtx))
	}
	provisioningSvc.OnUnprovisioned = func() {
		configpartition.UpdateAvahiForUnprovisioning(logger, agentPortNum)
		logger.Info("Device unprovisioned — restarting agent into unprovisioned mode")
		os.Exit(0)
	}

	// Renew the device certificate before it expires, with a fresh ML-DSA-65
	// key. The renewed identity loads through the same clean restart.
	// ponytail: restart per renewal (once per lifetime/3); swap in place if
	// restarts start to hurt.
	go provisioningSvc.RunRenewal(ctx, func() {
		logger.Info("Device certificate renewed — restarting agent to load the new identity")
		os.Exit(0)
	})

	if accessPolicy.AllowsNetwork {
		go bluetooth.StartBLESetup(ctx, logger, bluetooth.NewSetupDispatcher(provisioningSvcV2, networkMgr))
	}

	// Enrollment is non-blocking and begins only after callbacks are installed.
	// A baked ACME credential must not fall through to legacy enrollment when
	// invalid or already attempted. Conflicting handoffs require operator recovery.
	go func() {
		const bakedACMEPath = "/config/acme-enrollment.json"
		if _, err := os.Stat(bakedACMEPath); err == nil {
			provisioningSvcV2.ApplyACMEEnrollmentFile(ctx, bakedACMEPath)
		} else if os.IsNotExist(err) {
			provisioningSvc.ApplyEnrollmentFile(ctx)
		} else {
			logger.Warn("Cannot inspect first-boot ACME handoff; automatic enrollment skipped")
		}
	}()

	// Restore audio peripherals paired before the last reboot. Nothing else
	// does: BlueZ only reconnects after a link supervision timeout and has no
	// startup path, and a speaker that was already powered when the host went
	// away never pages us. Runs once per boot and waits on the user audio
	// session, so it neither delays startup nor repeats on agent restarts.
	go btManager.ReconnectTrusted(ctx)

	// Keep BLE HID links (gamepads) at a short supervision timeout, so a dead
	// link is torn down in about 0.5 s instead of the ~3 s such devices ask
	// for, and log every Bluetooth disconnect with its reason (WDY-3189).
	go linkWatcher.Run(ctx)

	otelPort := defaultOTELPort
	if p := os.Getenv("WENDY_OTEL_PORT"); p != "" {
		otelPort = p
	}

	otelServer := grpc.NewServer(
		grpc.UnaryInterceptor(interceptor.UnaryErrorInterceptor(logger)),
		grpc.StreamInterceptor(interceptor.StreamErrorInterceptor(logger)),
		grpc.InitialWindowSize(8*1024*1024),
		grpc.InitialConnWindowSize(16*1024*1024),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 5 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           20 * time.Second,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	collogspb.RegisterLogsServiceServer(otelServer, otelLogReceiver)
	colmetricspb.RegisterMetricsServiceServer(otelServer, otelMetricReceiver)
	coltracepb.RegisterTraceServiceServer(otelServer, otelTraceReceiver)

	otelLis, err := listenDualStackLoopback(otelPort)
	if err != nil {
		logger.Fatal("Failed to listen on OTEL port", zap.String("port", otelPort), zap.Error(err))
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("OTEL gRPC receiver listening", zap.String("port", otelPort))
		if err := otelServer.Serve(otelLis); err != nil {
			logger.Error("OTEL gRPC server error", zap.Error(err))
		}
	}()

	otelHTTPPort := defaultOTELHTTPPort
	if p := os.Getenv("WENDY_OTEL_HTTP_PORT"); p != "" {
		otelHTTPPort = p
	}

	otelHTTPReceiver := services.NewOTELHTTPReceiver(logger, telemetryBuf)
	otelHTTPLis, err := listenDualStackLoopback(otelHTTPPort)
	if err != nil {
		logger.Fatal("Failed to listen on OTEL HTTP port", zap.String("port", otelHTTPPort), zap.Error(err))
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("OTEL HTTP receiver listening", zap.String("port", otelHTTPPort))
		if err := otelHTTPReceiver.Serve(otelHTTPLis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("OTEL HTTP server error", zap.Error(err))
		}
	}()

	cloudFlusher := services.NewCloudFlusherWithProvisioning(logger, telemetryBuf, provisioningSvc)
	if telemetryBuf.DiskEnabled() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cloudFlusher.Run(ctx)
		}()
	}

	// Episode transfer worker: uploads sealed episodes to the cloud ingest
	// service over the device's own enrolled identity, the certificate
	// provisioning obtained and persisted. Its
	// queue is the episode store the data manager owns, which NewManager above
	// created (the agent exits when it cannot), so the only start condition is
	// provisioning completion, which Run waits for itself. It is deliberately
	// NOT gated on telemetry disk buffering: that reports on a different store,
	// and gating on it would silently leave every sealed episode unuploaded
	// until the quota evicted it.
	dataTransferWorker := services.NewDataTransferWorker(logger, dataManager, provisioningSvc)
	// WENDY_DATA_INGEST_URL names the DataIngestService endpoint episode uploads
	// dial (ingest.data.wendy.sh in dev). There is no fallback: the enrolled
	// cloud host does not serve DataIngestService. Unset disables uploads, the
	// worker says so once, and sealed episodes stay queued on the device.
	// Identity is the enrolled device certificate presented in the TLS
	// handshake; no request header carries it.
	if v := os.Getenv("WENDY_DATA_INGEST_URL"); v != "" {
		dataTransferWorker.SetIngestEndpoint(v)
		logger.Info("data transfer worker: ingest endpoint", zap.String("url", v))
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		dataTransferWorker.Run(ctx)
	}()

	<-ctx.Done()
	logger.Info("Termination requested, shutting down")
	// Never disarmed: returning from main ends the process, and it also covers
	// the deferred cleanup that runs after shutdown.run.
	armShutdownWatchdog(logger, shutdownDeadline, os.Exit)

	shutdown := &agentShutdown{
		logger: logger,
		hostShuttingDown: func(ctx context.Context) bool {
			return hostShuttingDown(ctx, systemctlOutput)
		},
		cancel: func() {
			cancel()
			meshStopCtx, meshStopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := localMeshLife.Stop(meshStopCtx); err != nil {
				logger.Error("Local networking shutdown did not finish", zap.Error(err))
			}
			meshStopCancel()
			_ = meshProxy.Close()
			if udpProxy != nil {
				_ = udpProxy.Close()
			}
			if meshUDPReady {
				// Join an in-flight repair before removing its rules.
				<-meshUDPPolicyDone
				_ = hostnetwork.CloseMeshUDPTProxy()
			}
		},
		shutdownHTTP: otelHTTPReceiver.Shutdown,
		background:   &wg,
	}
	if ctrdClient != nil {
		shutdown.stopApps = func(ctx context.Context) (int, error) {
			return ctrdClient.StopTasksForHostShutdown(ctx, agentcontainerd.HostShutdownGrace)
		}
	}
	if agentServer != nil {
		shutdown.servers = append(shutdown.servers, namedGRPCServer{"agent", agentServer})
	}
	if localSocketServer != nil {
		shutdown.servers = append(shutdown.servers, namedGRPCServer{"local socket", localSocketServer})
	}
	shutdown.servers = append(shutdown.servers, namedGRPCServer{"OTEL", otelServer})
	mtlsMu.Lock()
	if mtlsServer != nil {
		shutdown.servers = append(shutdown.servers, namedGRPCServer{"mTLS", mtlsServer})
	}
	mtlsMu.Unlock()
	shutdown.run()

	logger.Info("wendy-agent stopped")
}

// certNotBeforeFloor parses the device's own provisioning cert and returns its
// NotBefore time to use as a lower bound on time.Now() during mTLS cert
// verification. This lets the device accept legitimate client certs even when
// the system clock has not yet been synchronised via NTP (e.g. after a power
// cycle without WiFi). Returns a zero time.Time if the cert cannot be parsed.
func certNotBeforeFloor(certPEM string) time.Time {
	if certPEM == "" {
		return time.Time{}
	}
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return time.Time{}
	}
	// ML-DSA certs from pki-core have trailing ASN.1 bytes that cause
	// x509.ParseCertificate to fail. Strip them with the same fallback
	// used elsewhere in this repo (e.g. internal/agent/mtls/mldsa_verify.go).
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		var raw asn1.RawValue
		if _, asn1Err := asn1.Unmarshal(block.Bytes, &raw); asn1Err == nil {
			cert, err = x509.ParseCertificate(raw.FullBytes)
		}
	}
	if err != nil {
		return time.Time{}
	}
	return cert.NotBefore
}

// deviceScope reports the tenant this device's own current leaf belongs to,
// falling back to the legacy org the provisioning record carries when the
// certificate names no tenant at all.
//
// The certificate is the identity source, so it is read fresh on every call:
// a BLE provisioning event or a pki-core renewal replaces it without
// restarting the agent, and a scope captured at boot would outlive it.
func deviceScope(provisioningSvc *services.ProvisioningService) certs.Scope {
	certPEM, _, _ := provisioningSvc.ProvisioningCerts()
	if scope, ok := certs.ScopeFromCertPEM(certPEM); ok {
		return scope
	}
	// An unprovisioned device reports org 0, which Scope.Known reads as
	// "unidentified" — the same grace the mTLS interceptor applies.
	_, orgID, _, _ := provisioningSvc.ProvisioningInfo()
	return certs.Scope{OrgID: orgID}
}

// unitreePairingOwner keeps locally created pairings separate from enrolled
// tenants. Unknown enrolled identities fail closed rather than adopt local keys.
func unitreePairingOwner(scope certs.Scope, enrolled bool) unitree.PairingOwner {
	if scope.Known() {
		return unitree.PairingOwner{TenantUUID: scope.TenantUUID, OrgID: scope.OrgID}
	}
	return unitree.PairingOwner{Local: !enrolled}
}

// deviceIdentity reports this device's current tenant and device principal
// from its provisioning leaf, re-read on every call for the same reason
// deviceScope reads fresh: enrollment and renewal replace the leaf while the
// agent runs. A leaf without a tenant SPIFFE principal (legacy chain or an
// unprovisioned device) yields the zero pair, which the admin service reads
// as "not enrolled".
func deviceIdentity(provisioningSvc *services.ProvisioningService) (meshid.Tenant, meshid.Device) {
	certPEM, _, _ := provisioningSvc.ProvisioningCerts()
	if certPEM == "" {
		return meshid.Tenant{}, meshid.Device{}
	}
	chain, err := certs.ParseCertsFromPEM([]byte(certPEM))
	if err != nil || len(chain) == 0 {
		return meshid.Tenant{}, meshid.Device{}
	}
	device, err := meshid.DeviceFromCert(chain[0])
	if err != nil {
		return meshid.Tenant{}, meshid.Device{}
	}
	return device.Tenant(), device
}

// advertiseProvisionedMeshIdentity republishes the avahi advertisement with
// this device's full SPIFFE principal, read from its current leaf. The
// principal is what mesh peers dial and pin, so a sighting of this device
// cannot be spoofed by re-advertising someone else's id.
//
// A provisioned device whose leaf carries no tenant principal is a broken
// identity, not a naming detail: advertising a zero or numeric id would put a
// fake claim at the center of the mesh, so the advertisement is left alone
// and the failure is logged.
func advertiseProvisionedMeshIdentity(logger *zap.Logger, mtlsPort int, provisioningSvc *services.ProvisioningService) {
	_, device := deviceIdentity(provisioningSvc)
	if device == (meshid.Device{}) {
		logger.Warn("Provisioned certificate carries no tenant SPIFFE principal; wendy net advertisement not updated")
		return
	}
	configpartition.UpdateAvahiForProvisioning(logger, mtlsPort, device.Principal())
}

// ensureCNIBinDir (re)creates agentcontainerd.CNIBinDir with "bridge" and
// "host-local" symlinks pointing at this running binary's own executable
// path. The vendored bridge CNI plugin (internal/agent/cni/bridge) delegates
// IPAM by exec'ing "host-local" from its CNI_PATH; CNIAdd/CNIDel
// (internal/agent/containerd/cni.go) set CNI_PATH to this directory, so that
// delegated exec resolves back into the agent itself — cniPluginName then
// recognises the "host-local" argv0 and dispatches to the vendored
// hostlocal package — instead of a third-party binary. This removes the
// need for a pinned digest: there is no third-party binary in the exec path
// to pin (SOC2-CC6, ISO27001-A.8, NIST-SI-3).
//
// Best-effort: failures are logged as warnings, not fatal, since isolated
// app networking is not required for the agent to otherwise function.
func ensureCNIBinDir(logger *zap.Logger) {
	selfPath, err := os.Executable()
	if err != nil {
		logger.Warn("CNI bin dir: could not resolve agent executable path; isolated app networking will be unavailable", zap.Error(err))
		return
	}
	if err := os.MkdirAll(agentcontainerd.CNIBinDir, 0o755); err != nil {
		logger.Warn("CNI bin dir: mkdir failed", zap.String("dir", agentcontainerd.CNIBinDir), zap.Error(err))
		return
	}
	for _, name := range []string{"bridge", "host-local"} {
		link := filepath.Join(agentcontainerd.CNIBinDir, name)
		// Idempotent: remove any stale symlink (e.g. left pointing at a
		// previous agent binary path after an OTA update) before recreating.
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			logger.Warn("CNI bin dir: could not remove stale symlink", zap.String("link", link), zap.Error(err))
			continue
		}
		if err := os.Symlink(selfPath, link); err != nil {
			logger.Warn("CNI bin dir: could not create symlink", zap.String("link", link), zap.String("target", selfPath), zap.Error(err))
		}
	}
}

// cloudGRPCURLForCloudHost retains the enrolled Cloud endpoint (or explicit
// API override). MeshRoster selects the device mTLS listener with DeviceEndpoint;
// the old broker's port mapping is not part of the Cloud v2 contract.
func cloudGRPCURLForCloudHost(cloudHost string) string {
	if v := os.Getenv("WENDY_CLOUD_URL"); v != "" {
		return v
	}
	return cloudHost
}

func handleUtilityCommand(args []string) (bool, int) {
	// CNI dispatch takes precedence over everything else, including the
	// len(args) == 0 case below: the vendored bridge plugin's IPAM
	// delegation invokes this binary as bare argv0 "host-local" with no
	// arguments, reading CNI_COMMAND from the environment instead (see
	// cniPluginName and cni_exec_linux.go).
	if name := cniPluginName(args, filepath.Base(os.Args[0])); name != "" {
		return true, runCNIPlugin(name)
	}

	if len(args) == 0 {
		return false, 0
	}

	if args[0] == "--version" || args[0] == "-v" {
		fmt.Println(version.Version)
		return true, 0
	}

	if args[0] != "utils" {
		return false, 0
	}

	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: wendy-agent utils <command>")
		return true, 2
	}
	if args[1] == "ipcam-gstreamer" {
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "invalid GStreamer helper invocation")
			return true, 2
		}
		if err := services.RunIPCameraGStreamerHelper(os.Stdin, os.Stdout); err != nil {
			// Keep this deliberately generic: the helper's pipeline contains camera
			// credentials, and library diagnostics may repeat property values.
			fmt.Fprintln(os.Stderr, "GStreamer capture pipeline failed")
			return true, 1
		}
		return true, 0
	}
	if args[1] != "open-browser" {
		return false, 0
	}

	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: wendy-agent utils open-browser <url>")
		return true, 2
	}

	rawURL := args[2]
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid URL %q: %v\n", rawURL, err)
		return true, 2
	}
	if parsed.Scheme == "" {
		fmt.Fprintf(os.Stderr, "invalid URL %q: missing scheme (e.g. http:// or https://)\n", rawURL)
		return true, 2
	}
	if (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host == "" {
		fmt.Fprintf(os.Stderr, "invalid URL %q: must include a host (e.g. http://localhost:3000)\n", rawURL)
		return true, 2
	}

	if err := browseropen.Open(rawURL); err != nil {
		fmt.Fprintf(os.Stderr, "Could not open browser: %v\n", err)
		fmt.Println(rawURL)
		return true, 0
	}

	fmt.Printf("Opening %s in default browser...\n", rawURL)
	return true, 0
}

// envBytes reads a byte count from the environment, falling back to fallback
// when the variable is unset. A value that is present but unusable is reported
// rather than silently ignored: a device configured with a bad quota should
// learn that its configuration did not take.
//
// minimum is the smallest value the setting accepts. It exists because the two
// callers disagree about zero: a reserve of zero is a real choice ("keep no
// headroom"), while a maximum quota of zero is not a store that holds nothing,
// it is a value SetQuota discards in favour of the default. Accepting zero for
// the quota therefore produced a device running on 50 GiB while its operator
// believed they had capped it, with nothing logged either way.
func envBytes(logger *zap.Logger, name string, fallback, minimum int64) int64 {
	raw, ok := os.LookupEnv(name)
	if !ok || strings.TrimSpace(raw) == "" {
		return fallback
	}
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || v < minimum {
		logger.Warn("ignoring unusable byte count in environment; using the default",
			zap.String("variable", name), zap.String("value", raw),
			zap.Int64("minimum", minimum), zap.Int64("default", fallback))
		return fallback
	}
	return v
}
