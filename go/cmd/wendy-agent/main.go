package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/internal/agent/bluetooth"
	"github.com/wendylabsinc/wendy/internal/agent/cdi"
	"github.com/wendylabsinc/wendy/internal/agent/configpartition"
	"github.com/wendylabsinc/wendy/internal/agent/container"
	agentcontainerd "github.com/wendylabsinc/wendy/internal/agent/containerd"
	"github.com/wendylabsinc/wendy/internal/agent/dbusproxy"
	"github.com/wendylabsinc/wendy/internal/agent/hardware"
	"github.com/wendylabsinc/wendy/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/internal/agent/mtls"
	agentnet "github.com/wendylabsinc/wendy/internal/agent/network"
	"github.com/wendylabsinc/wendy/internal/agent/registry"
	"github.com/wendylabsinc/wendy/internal/agent/services"
	"github.com/wendylabsinc/wendy/internal/shared/browseropen"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
)

const (
	defaultAgentPort    = "50051"
	defaultOTELPort     = "4317"
	defaultOTELHTTPPort = "4318"
)

func main() {
	if handled, code := handleUtilityCommand(os.Args[1:]); handled {
		os.Exit(code)
	}

	// Setup logger.
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

	// Wrap the logger so agent internal logs are published to the telemetry stream.
	telemetryCore := services.NewTelemetryCore(broadcaster, zapcore.DebugLevel)
	logger = zap.New(zapcore.NewTee(logger.Core(), telemetryCore))

	logger.Info("Starting wendy-agent", zap.String("version", version.Version))

	configpartition.Apply(logger)
	services.CommitMenderUpdate(logger)

	// Clean up old agent binary backups from previous updates.
	services.CleanupOldBackups(logger)

	// Ensure NVIDIA CDI spec exists for GPU container support.
	cdi.EnsureNVIDIACDISpec(logger)

	var networkMgr services.NetworkManager
	if nm := agentnet.NewNMCLINetworkManager(logger); nm != nil {
		networkMgr = nm
	}
	hwDiscoverer := hardware.NewSystemHardwareDiscoverer(logger)
	btManager := bluetooth.NewManager(logger)

	// Initialize D-Bus proxy manager if xdg-dbus-proxy is available.
	var proxyMgr *dbusproxy.Manager
	if dbusproxy.IsAvailable() {
		proxyMgr = dbusproxy.NewManager(logger)
	} else {
		logger.Warn("xdg-dbus-proxy not found, Bluetooth containers will have unfiltered D-Bus access")
	}

	// Initialize containerd client (best-effort; may fail on non-Linux or without containerd).
	var containerdClient services.ContainerdClient
	containerdAddr := os.Getenv("WENDY_CONTAINERD_ADDR")
	if containerdAddr == "" {
		containerdAddr = agentcontainerd.DefaultAddress
	}
	ctrdClient, ctrdErr := agentcontainerd.NewClient(logger, containerdAddr, proxyMgr)
	if ctrdErr != nil {
		logger.Warn("Failed to connect to containerd (container features will be unavailable)", zap.Error(ctrdErr))
	} else {
		containerdClient = ctrdClient
		defer ctrdClient.Close()
	}

	logManager := services.NewContainerLogManager(logger, broadcaster)

	agentSvc := services.NewAgentService(logger, networkMgr, hwDiscoverer, btManager)
	containerSvc := services.NewContainerService(logger, containerdClient, services.WithLogManager(logManager))
	audioSvc := services.NewAudioService(logger)
	videoSvc := services.NewVideoService(logger)

	configPath := "/etc/wendy-agent"
	if envPath := os.Getenv("WENDY_CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}
	provisioningSvc := services.NewProvisioningService(logger, configPath)
	telemetrySvc := services.NewTelemetryService(logger, broadcaster)

	// OTEL receivers.
	otelLogReceiver := services.NewOTELLogsReceiver(broadcaster)
	otelMetricReceiver := services.NewOTELMetricsReceiver(broadcaster)
	otelTraceReceiver := services.NewOTELTraceReceiver(broadcaster)

	// Start container monitor.
	monitor := container.NewContainerMonitor(logger, containerdClient, 15*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// registryTLSConfig builds a minimal server TLS config from provisioning PEM strings.
	// Returns nil if the PEM data is invalid, which causes the registry to stay HTTP.
	registryTLSConfig := func(certPEM, chainPEM, keyPEM string) *tls.Config {
		fullChain := certPEM
		if chainPEM != "" {
			fullChain = certPEM + "\n" + chainPEM
		}
		cert, err := tls.X509KeyPair([]byte(fullChain), []byte(keyPEM))
		if err != nil {
			logger.Error("Failed to build registry TLS config", zap.Error(err))
			return nil
		}
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
	}

	// Track the registry server so it can be restarted with HTTPS on provisioning.
	var (
		registrySrv   *registry.Server
		registrySrvMu sync.Mutex
	)

	// startRegistry starts (or restarts) the embedded OCI registry. When tlsConfig
	// is non-nil it serves HTTPS; nil means plain HTTP (pre-provisioning only).
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

	// Start container monitor in background.
	wg.Add(1)
	go func() {
		defer wg.Done()
		monitor.Run(ctx)
	}()

	// Collect CPU/memory metrics for all running containers.
	if containerdClient != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			services.CollectContainerMetrics(ctx, containerdClient, broadcaster, logManager)
		}()
	}

	// Collect CPU/memory metrics for the agent process itself.
	wg.Add(1)
	go func() {
		defer wg.Done()
		services.CollectAgentMetrics(ctx, broadcaster)
	}()

	// Main agent gRPC server port.
	agentPort := defaultAgentPort
	if p := os.Getenv("WENDY_AGENT_PORT"); p != "" {
		agentPort = p
	}

	// startTunnelBroker launches the tunnel broker presence loop in the background.
	// ProvisioningInfo() is called inside the goroutine to avoid re-entering the
	// provisioning mutex when called from the OnProvisioned callback.
	startTunnelBroker := func(certPEM, chainPEM, keyPEM string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cloudHost, orgID, assetID, enrolled := provisioningSvc.ProvisioningInfo()
			if !enrolled {
				return
			}
			brokerURL := os.Getenv("WENDY_BROKER_URL")
			if brokerURL == "" {
				brokerURL = fmt.Sprintf("%s:50053", cloudHost)
			}
			client := services.NewTunnelBrokerClient(logger, brokerURL, orgID, assetID, certPEM, chainPEM, keyPEM, "")
			client.Run(ctx)
		}()
	}

	// Track the mTLS server so we can shut it down gracefully.
	var mtlsServer *grpc.Server
	var mtlsMu sync.Mutex

	// registerAllServices registers all agent services on the given gRPC server.
	registerAllServices := func(srv *grpc.Server) {
		agentpb.RegisterWendyAgentServiceServer(srv, agentSvc)
		agentpb.RegisterWendyContainerServiceServer(srv, containerSvc)
		agentpb.RegisterWendyAudioServiceServer(srv, audioSvc)
		agentpb.RegisterWendyVideoServiceServer(srv, videoSvc)
		agentpb.RegisterWendyProvisioningServiceServer(srv, provisioningSvc)
		agentpb.RegisterWendyTelemetryServiceServer(srv, telemetrySvc)
	}

	// startMTLSServer creates and starts the mTLS gRPC server on agentPort+1.
	startMTLSServer := func(certPEM, chainPEM, keyPEM string) {
		mtlsMu.Lock()
		defer mtlsMu.Unlock()

		if mtlsServer != nil {
			logger.Warn("mTLS server already running, skipping")
			return
		}

		srv, err := mtls.NewServer(certPEM, chainPEM, keyPEM,
			grpc.UnaryInterceptor(interceptor.UnaryErrorInterceptor(logger)),
			grpc.StreamInterceptor(interceptor.StreamErrorInterceptor(logger)),
		)
		if err != nil {
			logger.Error("Failed to create mTLS server", zap.Error(err))
			return
		}

		// Register all services on the mTLS server.
		registerAllServices(srv)

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

	// mtlsPortNum is agentPort+1; used for the mTLS server and Avahi advertisement.
	agentPortNum, _ := strconv.Atoi(agentPort)
	mtlsPortNum := agentPortNum + 1

	// Set up the provisioning callback to start the mTLS server dynamically.
	provisioningSvc.OnProvisioned = func(certPEM, chainPEM, keyPEM string) {
		startMTLSServer(certPEM, chainPEM, keyPEM)
		configpartition.UpdateAvahiForProvisioning(logger, mtlsPortNum)
	}

	// Check if already provisioned and start mTLS server and tunnel broker if certificates exist.
	certPEM, chainPEM, keyPEM := provisioningSvc.ProvisioningCerts()
	alreadyProvisioned := certPEM != "" && keyPEM != ""

	if alreadyProvisioned {
		startMTLSServer(certPEM, chainPEM, keyPEM)
		configpartition.UpdateAvahiForProvisioning(logger, mtlsPortNum)
		startTunnelBroker(certPEM, chainPEM, keyPEM)
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
	// Once provisioned the mTLS server handles all gRPC traffic and the plaintext
	// port is shut down so unprovisioned clients cannot access device services.
	var agentServer *grpc.Server
	if !alreadyProvisioned {
		agentServer = grpc.NewServer(
			grpc.UnaryInterceptor(interceptor.UnaryErrorInterceptor(logger)),
			grpc.StreamInterceptor(interceptor.StreamErrorInterceptor(logger)),
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

	// Set up the provisioning callback to start the mTLS server, shut down
	// the plaintext server, and switch the registry to HTTPS.
	provisioningSvc.OnProvisioned = func(certPEM, chainPEM, keyPEM string) {
		startMTLSServer(certPEM, chainPEM, keyPEM)
		configpartition.UpdateAvahiForProvisioning(logger, mtlsPortNum)
		if agentServer != nil {
			logger.Info("Device provisioned — shutting down plaintext gRPC port", zap.String("port", agentPort))
			go agentServer.GracefulStop()
		}
		if runtime.GOOS == "linux" && ctrdErr == nil {
			go startRegistry(registryTLSConfig(certPEM, chainPEM, keyPEM))
		}
	}

	// OTEL gRPC receiver server.
	otelPort := defaultOTELPort
	if p := os.Getenv("WENDY_OTEL_PORT"); p != "" {
		otelPort = p
	}

	otelServer := grpc.NewServer(
		grpc.UnaryInterceptor(interceptor.UnaryErrorInterceptor(logger)),
		grpc.StreamInterceptor(interceptor.StreamErrorInterceptor(logger)),
	)
	otelpb.RegisterLogsServiceServer(otelServer, otelLogReceiver)
	otelpb.RegisterMetricsServiceServer(otelServer, otelMetricReceiver)
	otelpb.RegisterTraceServiceServer(otelServer, otelTraceReceiver)

	otelLis, err := net.Listen("tcp", "[::]:"+otelPort)
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

	// OTEL HTTP/protobuf receiver server (port 4318).
	otelHTTPPort := defaultOTELHTTPPort
	if p := os.Getenv("WENDY_OTEL_HTTP_PORT"); p != "" {
		otelHTTPPort = p
	}

	otelHTTPReceiver := services.NewOTELHTTPReceiver(logger, broadcaster)
	otelHTTPLis, err := net.Listen("tcp", "[::]:"+otelHTTPPort)
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

	// Graceful shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	logger.Info("Received signal, shutting down", zap.String("signal", sig.String()))

	cancel()
	if agentServer != nil {
		agentServer.GracefulStop()
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

func handleUtilityCommand(args []string) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}

	if args[0] != "utils" {
		return false, 0
	}

	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: wendy-agent utils open-browser <url>")
		return true, 2
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
