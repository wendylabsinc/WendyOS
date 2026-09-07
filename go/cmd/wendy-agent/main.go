package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	"github.com/wendylabsinc/wendy/go/internal/agent/bluetooth"
	"github.com/wendylabsinc/wendy/go/internal/agent/cdi"
	"github.com/wendylabsinc/wendy/go/internal/agent/configpartition"
	"github.com/wendylabsinc/wendy/go/internal/agent/container"
	agentcontainerd "github.com/wendylabsinc/wendy/go/internal/agent/containerd"
	"github.com/wendylabsinc/wendy/go/internal/agent/dbusproxy"
	"github.com/wendylabsinc/wendy/go/internal/agent/hardware"
	"github.com/wendylabsinc/wendy/go/internal/agent/hostexec"
	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/agent/localsocket"
	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	agentnet "github.com/wendylabsinc/wendy/go/internal/agent/network"
	"github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/agent/registry"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"github.com/wendylabsinc/wendy/go/internal/agent/usbgadget"
	"github.com/wendylabsinc/wendy/go/internal/shared/browseropen"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
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

	var networkMgr services.NetworkManager
	if nm := agentnet.NewNMCLINetworkManager(logger); nm != nil {
		networkMgr = nm
	}
	hwDiscoverer := hardware.NewSystemHardwareDiscoverer(logger)
	btManager := bluetooth.NewManager(logger)

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
		logger.Warn("failed to init mesh chain", zap.Error(err))
	}
	// Same non-fatal treatment for the NAT chain that redirects mesh VIP
	// traffic to the local mesh proxy (started below, once cert material is
	// in scope).
	if err := hostnetwork.InitMeshNATChain(); err != nil {
		logger.Warn("failed to init mesh nat chain", zap.Error(err))
	}
	// Same non-fatal treatment for the nat-table chain that forwards the
	// agent's own loopback MeshDial dial into a meshed container's published
	// port (see hostnetwork.MeshPortsChainName).
	if err := hostnetwork.InitMeshPortsChain(); err != nil {
		logger.Warn("failed to init mesh ports chain", zap.Error(err))
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

	provisioningSvc := services.NewProvisioningService(logger, configPath)
	telemetrySvc := services.NewTelemetryService(logger, broadcaster, telemetryBuf)

	deviceInfoSvc := services.NewDeviceInfoService(logger, hwDiscoverer)
	timeSyncSvc := services.NewTimeSyncService(logger, timesyncMgr)
	wifiSvc := services.NewWiFiService(logger, networkMgr)
	bluetoothSvc := services.NewBluetoothService(logger, btManager)
	agentUpdateSvc := services.NewAgentUpdateService(logger, installer)
	agentUpdateSvc.PrimeExecPath()
	osUpdateSvc := services.NewOSUpdateService(logger)
	driverSvc := services.NewDriverService(logger)
	// Before anything reads the store: devices updated from an older agent still
	// have add-ons in the pre-keyed flat layout.
	driverSvc.MigrateStore()
	containerSvcV2 := services.NewContainerServiceV2(containerSvc)
	provisioningSvcV2 := services.NewProvisioningServiceV2(provisioningSvc)
	audioSvcV2 := services.NewAudioServiceV2(audioSvc)
	telemetrySvcV2 := services.NewTelemetryServiceV2(logger, broadcaster, telemetryBuf)
	// ROS 2 inspection requires the containerd-backed sidecar runtime; the
	// service is only registered when containerd connected (WDY-1332).
	var ros2Svc *services.ROS2Service
	if ctrdClient != nil {
		ros2Svc = services.NewROS2Service(logger, ctrdClient, agentcontainerd.ROS2BagDir)
	}

	// OTEL receivers.
	otelLogReceiver := services.NewOTELLogsReceiver(telemetryBuf)
	otelMetricReceiver := services.NewOTELMetricsReceiver(telemetryBuf)
	otelTraceReceiver := services.NewOTELTraceReceiver(telemetryBuf)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	notificationSender := services.NewCloudNotificationSender(logger, provisioningSvc)
	systemAPISocketManager := services.NewAppSystemAPISocketManager(ctx, logger, notificationSender)
	if ctrdClient != nil {
		ctrdClient.SetAppSystemAPISocketProvider(systemAPISocketManager)
		ctrdClient.RestoreAppSystemAPISockets(ctx)
	}

	go timesyncMgr.RunDirect(ctx)
	go timesyncMgr.RunMulticast(ctx)

	startROS2BatteryMonitor(ctx, logger, configPath)

	var videoROSRuntime []services.ROS2Runtime
	if ctrdClient != nil {
		videoROSRuntime = append(videoROSRuntime, ctrdClient)
	}
	videoSvc := services.NewVideoService(ctx, logger, videoROSRuntime...)
	defer videoSvc.Shutdown()
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

		registryAddr := "0.0.0.0:5000"
		if addr := os.Getenv("WENDY_REGISTRY_ADDR"); addr != "" {
			registryAddr = addr
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
		wg.Add(1)
		go func() {
			defer wg.Done()
			cloudHost, orgID, assetID, enrolled := provisioningSvc.ProvisioningInfo()
			if !enrolled {
				return
			}
			brokerURL := os.Getenv("WENDY_BROKER_URL")
			if brokerURL == "" {
				brokerURL = brokerURLForCloudHost(cloudHost)
			}
			certPEM, chainPEM, keyData := provisioningSvc.ProvisioningCerts()
			keyPEM := string(keyData)
			for i := range keyData {
				keyData[i] = 0
			}
			if chainPEM == "" {
				logger.Warn("CA chain PEM unavailable; cannot start tunnel broker (re-provision if this persists)")
				return
			}
			client := services.NewTunnelBrokerClient(logger, brokerURL, orgID, assetID, certPEM, keyPEM, chainPEM, mtlsPortNum)
			client.Run(ctx)
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
	// All listeners extract into the same per-app context directories. Share
	// their locks so a local-socket build cannot race an mTLS build for the same
	// app and replace its source tree while buildctl is reading it.
	buildContextLocks := services.NewBuildContextLockSet()

	registerAllServices := func(srv *grpc.Server) {
		// MeshService's own-org check (assetIdentityFromContext / MeshDial)
		// must reflect this device's *current* org, not a value captured once
		// at process start: a live BLE-provisioning event updates
		// provisioningSvc's state without restarting the agent, and a stale
		// org (e.g. 0/unknown from an unprovisioned boot) would silently
		// disable the check for the rest of the process's life. Read fresh
		// here instead of caching a package-level meshSvc; registerAllServices
		// runs at most once per concrete server (plaintext agentServer, the
		// local control socket, and the mTLS server), so this is at most a
		// handful of cheap constructions over the process lifetime, not a hot
		// path. orgID == 0 (never provisioned) intentionally matches the mTLS
		// org interceptor's grace behavior: MeshService skips the org-equality
		// check rather than reject every caller.
		_, orgID, _, _ := provisioningSvc.ProvisioningInfo()
		meshSvc := services.NewMeshService(logger, configPath, orgID)
		buildSvc := services.NewBuildService(logger, services.BuildServiceOptions{
			ConfigPath:   configPath,
			Chunks:       buildChunkSource,
			Peers:        meshDialer,
			ContextLocks: buildContextLocks,
			// Read fresh per build rather than captured: a certificate rotated
			// while the agent runs must be picked up without a restart.
			//
			// ExpectingPeer, not the plain client config: the plain one validates
			// the chain but skips hostname verification (device certs carry wendy
			// URN SANs, not DNS names), so any org-issued certificate could
			// terminate the registry hop and receive the image. The mesh LAN path
			// picks its peer from an unauthenticated mDNS TXT record, which is the
			// spoofing gap this helper was written for.
			PushTLS: func(targetAssetID int32) (*tls.Config, error) {
				certPEM, chainPEM, keyData := provisioningSvc.ProvisioningCerts()
				_, pushOrgID, _, _ := provisioningSvc.ProvisioningInfo()
				return mtls.NewClientTLSConfigExpectingPeer(certPEM, chainPEM, string(keyData), logger,
					pushOrgID, strconv.FormatInt(int64(targetAssetID), 10))
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
		agentpbv2.RegisterWendyMeshServiceServer(srv, meshSvc)
		agentpbv2.RegisterWendyBuildServiceServer(srv, buildSvc)
		if ros2Svc != nil {
			agentpbv2.RegisterROS2ServiceServer(srv, ros2Svc)
		}
	}

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
		expectedOrg, haveOrg := deviceOrgFromCertPEM(certPEM)
		effectiveMode := orgMode
		if orgMode != interceptor.OrgModeOff && !haveOrg {
			// Fail safe: the device cannot determine its own org, so it cannot
			// meaningfully compare a client's org against it. Rather than brick the
			// device (rejecting all clients) or silently enforce against an unknown
			// self-org, disable enforcement for this server and log loudly.
			logger.Error("cannot determine device organization from own certificate; mTLS org enforcement DISABLED for this server",
				zap.String("configuredMode", orgMode.String()))
			effectiveMode = interceptor.OrgModeOff
		}
		if effectiveMode != interceptor.OrgModeOff {
			logger.Info("mTLS server enforcing org",
				zap.Int32("org", expectedOrg),
				zap.String("mode", effectiveMode.String()))
		}

		srv, err := mtls.NewServer(certPEM, chainPEM, keyPEM, logger, floor, expectedOrg, effectiveMode,
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

		// WendyShellService opens an interactive *host* root shell. It is
		// deliberately registered ONLY here, on the mTLS server, so it is
		// reachable only on a provisioned device over an authenticated,
		// org-checked connection. It is intentionally NOT part of
		// registerAllServices: that would also expose it on the unauthenticated
		// plaintext pre-provisioning server (handing anyone on the LAN a host
		// root shell) and on the local admin socket.
		agentpb.RegisterWendyShellServiceServer(srv, shellSvc)

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

		lis, err := net.Listen("tcp", "[::]:"+mtlsPort)
		if err != nil {
			logger.Error("Failed to listen on mTLS port", zap.String("port", mtlsPort), zap.Error(err))
			return
		}

		mtlsServer = srv

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

	// Client side of the mesh data plane: dials peers LAN-direct or via the
	// cloud tunnel broker, fed by whatever REDIRECTed VIP connections the nat
	// chain above sends to ProxyPort. Constructed here — the first point where
	// cert material (certPEM/chainPEM/keyPEM, just above) and provisioning
	// info are both in scope — rather than gated on alreadyProvisioned: on an
	// unenrolled device brokerURL/cert fields are empty, so DialDevice simply
	// fails closed at runtime with a clear error instead of never starting.
	cloudHost, orgID, assetID, _ := provisioningSvc.ProvisioningInfo()
	brokerURL := os.Getenv("WENDY_BROKER_URL")
	if brokerURL == "" {
		brokerURL = brokerURLForCloudHost(cloudHost)
	}
	meshMetrics := services.NewMeshMetrics(telemetryBuf, logger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		meshMetrics.Collect(ctx)
	}()
	meshDialer = services.NewMeshDialer(logger, brokerURL, orgID, assetID, certPEM, keyPEM, chainPEM, meshMetrics)
	meshProxy := mesh.NewProxy(logger, meshDialer, meshMetrics)
	if err := meshProxy.Start(fmt.Sprintf(":%d", mesh.ProxyPort)); err != nil {
		logger.Warn("mesh proxy failed to start; mesh egress disabled", zap.Error(err))
	}

	// Hybrid friendly-name resolver: <devicename>.<org-slug>.cloud.wendy.dev.
	// Mirrors the meshDialer construction just above — not gated on
	// alreadyProvisioned. On an unenrolled device orgID/assetID/chainPEM are
	// zero/empty, so MeshRoster.Sync simply fails closed (logged, retried)
	// until the device is provisioned, rather than never starting at all.
	meshRoster := services.NewMeshRoster(logger, cloudGRPCURLForCloudHost(cloudHost), orgID, assetID, certPEM, keyPEM, chainPEM)
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
	meshResolver := services.NewMeshResolver(logger, orgID, meshRoster, meshBrowse)
	if meshDNS != nil {
		meshDNS.SetResolver(meshResolver)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		meshRoster.RunSync(ctx, 5*time.Minute)
	}()

	if alreadyProvisioned {
		startMTLSServer(certPEM, chainPEM, keyPEM)
		startTunnelBroker()
		_, orgID, assetID, _ := provisioningSvc.ProvisioningInfo()
		configpartition.UpdateAvahiForProvisioning(logger, mtlsPortNum, assetID, orgID)
		startBLEPeripheral(certPEM, chainPEM, keyPEM)
	}

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
				Timeout:           10 * time.Second,
			}),
			grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
				MinTime:             10 * time.Second,
				PermitWithoutStream: true,
			}),
		)
		registerAllServices(agentServer)

		agentLis, err := net.Listen("tcp", "[::]:"+agentPort)
		if err != nil {
			logger.Fatal("Failed to listen on agent port", zap.String("port", agentPort), zap.Error(err))
		}

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
	go usbgadget.EnsureWellKnownAddress(ctx, 30*time.Second, logger)

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
		defer func() {
			for i := range keyData {
				keyData[i] = 0
			}
		}()
		keyPEM := string(keyData)
		startMTLSServer(certPEM, chainPEM, keyPEM)
		startTunnelBroker()
		cloudHost, orgID, assetID, _ := provisioningSvc.ProvisioningInfo()
		// Refresh the mesh dialer with the fresh identity — like the mTLS
		// server and tunnel broker above, it consumes cert material, and BLE
		// first-boot enrollment happens while the agent is running, so the
		// boot-time snapshot is empty on virtually every new device.
		freshBrokerURL := os.Getenv("WENDY_BROKER_URL")
		if freshBrokerURL == "" {
			freshBrokerURL = brokerURLForCloudHost(cloudHost)
		}
		meshDialer.UpdateIdentity(freshBrokerURL, orgID, assetID, certPEM, keyPEM, chainPEM)
		// Same story for the friendly-name roster/resolver: the boot-time
		// snapshot is org=0/asset=0/empty chain, so without this refresh
		// friendly names stay dead (resolver rejects every LAN peer, roster
		// dials with a null identity) until the agent is restarted.
		meshRoster.UpdateIdentity(cloudGRPCURLForCloudHost(cloudHost), orgID, assetID, certPEM, keyPEM, chainPEM)
		meshResolver.UpdateOwnOrgID(orgID)
		// Resync immediately so friendly names work without waiting for the periodic tick.
		go func() {
			sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if err := meshRoster.Sync(sctx); err != nil {
				logger.Warn("mesh roster resync after provisioning failed", zap.Error(err))
			}
		}()
		configpartition.UpdateAvahiForProvisioning(logger, mtlsPortNum, assetID, orgID)
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
	provisioningSvc.OnUnprovisioned = func() {
		configpartition.UpdateAvahiForUnprovisioning(logger, agentPortNum)
		logger.Info("Device unprovisioned — restarting agent into unprovisioned mode")
		os.Exit(0)
	}

	// Self-enroll from a token staged by agent.sh (Linux Desktop install).
	// Best-effort and non-blocking: a cloud outage must never delay the agent
	// coming up locally (mDNS discovery still works unenrolled).
	go provisioningSvc.ApplyEnrollmentFile(context.Background())

	// Restore audio peripherals paired before the last reboot. Nothing else
	// does: BlueZ only reconnects after a link supervision timeout and has no
	// startup path, and a speaker that was already powered when the host went
	// away never pages us. Runs once per boot and waits on the user audio
	// session, so it neither delays startup nor repeats on agent restarts.
	go btManager.ReconnectTrusted(ctx)

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
			Timeout:           10 * time.Second,
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
		if err := otelHTTPReceiver.Serve(otelHTTPLis); err != nil && err != http.ErrServerClosed {
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

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.Info("Received signal, shutting down", zap.String("signal", sig.String()))

	cancel()
	_ = meshProxy.Close()
	if agentServer != nil {
		agentServer.GracefulStop()
	}
	if localSocketServer != nil {
		localSocketServer.GracefulStop()
	}
	otelServer.GracefulStop()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := otelHTTPReceiver.Shutdown(shutdownCtx); err != nil {
		logger.Error("OTEL HTTP server shutdown error", zap.Error(err))
	}

	mtlsMu.Lock()
	if mtlsServer != nil {
		mtlsServer.GracefulStop()
	}
	mtlsMu.Unlock()

	wg.Wait()

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

// deviceOrgFromCertPEM parses the device's own leaf certificate (ML-DSA aware,
// mirroring certNotBeforeFloor) and extracts its organization ID via
// certs.OrgFromClientCert. It returns (org, true) when an org identity is present
// and valid, and (0, false) on any parse/extract error or when the cert carries no
// org identity. The caller treats (0, false) as "device org unknown".
func deviceOrgFromCertPEM(certPEM string) (int32, bool) {
	if certPEM == "" {
		return 0, false
	}
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		return 0, false
	}
	// ML-DSA certs from pki-core have trailing ASN.1 bytes that cause
	// x509.ParseCertificate to fail. Strip them with the same fallback used by
	// certNotBeforeFloor and internal/agent/mtls/mldsa_verify.go.
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		var raw asn1.RawValue
		if _, asn1Err := asn1.Unmarshal(block.Bytes, &raw); asn1Err == nil {
			cert, err = x509.ParseCertificate(raw.FullBytes)
		}
	}
	if err != nil {
		return 0, false
	}
	org, hasOrg, err := certs.OrgFromClientCert(cert)
	if err != nil || !hasOrg {
		return 0, false
	}
	return org, true
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

func brokerURLForCloudHost(cloudHost string) string {
	host, port, err := net.SplitHostPort(cloudHost)
	if err == nil {
		if port == "443" {
			return cloudHost
		}
		return net.JoinHostPort(host, "50052")
	}
	return net.JoinHostPort(cloudHost, "50052")
}

// cloudGRPCURLForCloudHost returns the cloud API gRPC target for the mesh
// roster RPC, derived from the provisioning cloud host the same way the broker
// URL is. If WENDY_CLOUD_URL is set it wins (dev/self-host override).
func cloudGRPCURLForCloudHost(cloudHost string) string {
	if v := os.Getenv("WENDY_CLOUD_URL"); v != "" {
		return v
	}
	return brokerURLForCloudHost(cloudHost) // same host:port; MeshRosterService is served there
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
