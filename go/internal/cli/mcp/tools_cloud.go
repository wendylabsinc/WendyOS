package mcp

import (
	"context"
	"crypto"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshname"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

type mcpCloudTunnel struct {
	info       cloudTunnelInfo
	closeOnce  sync.Once
	closeErr   error
	cancel     context.CancelFunc
	listener   net.Listener
	udpConn    *net.UDPConn
	session    *mcpDatagramSession
	brokerConn *grpc.ClientConn
	brokerURL  string
}

func (t *mcpCloudTunnel) Close() error {
	if t == nil {
		return nil
	}
	t.closeOnce.Do(func() {
		if t.cancel != nil {
			t.cancel()
		}
		var errs []error
		if t.listener != nil {
			errs = append(errs, t.listener.Close())
		}
		if t.udpConn != nil {
			errs = append(errs, t.udpConn.Close())
		}
		if t.session != nil {
			t.session.close()
		}
		if t.brokerConn != nil {
			errs = append(errs, t.brokerConn.Close())
		}
		for _, err := range errs {
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.closeErr = errors.Join(t.closeErr, err)
			}
		}
	})
	return t.closeErr
}

func (s *mcpServer) registerCloudTools(srv *server.MCPServer) {
	discoverOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("List enrolled cloud devices for the selected Wendy Cloud auth session"),
		mcpgo.WithString("cloud_grpc",
			mcpgo.Description("Cloud gRPC endpoint to use, e.g. cloud.wendy.dev:443 (optional when a default session is set via 'wendy auth use')"),
		),
		mcpgo.WithBoolean("online_only",
			mcpgo.Description("Only list devices with active tunnel broker presence (default true)"),
		),
		mcpgo.WithString("filter",
			mcpgo.Description("Optional cloud-side asset filter"),
		),
		mcpgo.WithNumber("max_bytes", mcpgo.Description("Maximum output size in bytes before the result is truncated (default 100000)")),
	}
	discoverOpts = append(discoverOpts, readOnly()...)
	discoverOpts = append(discoverOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("cloud_discover", discoverOpts...), s.handleCloudDiscover)

	connectOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Connect the MCP session to a cloud-enrolled device through the Wendy Cloud tunnel"),
		mcpgo.WithString("device_name",
			mcpgo.Description("Device name; optional only when exactly one cloud device is available"),
		),
		mcpgo.WithString("cloud_grpc",
			mcpgo.Description("Cloud gRPC endpoint to use, e.g. cloud.wendy.dev:443 (optional when a default session is set via 'wendy auth use')"),
		),
		mcpgo.WithString("broker_url",
			mcpgo.Description("Tunnel broker host:port (default: cloud :443 endpoint, otherwise <cloud-host>:50052)"),
		),
	}
	connectOpts = append(connectOpts, mutating()...)
	connectOpts = append(connectOpts, idempotent()...)
	connectOpts = append(connectOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("cloud_connect", connectOpts...), s.handleCloudConnect)

	enrollOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Enroll the connected device using your current cloud login. Use provisioning_start only with an externally supplied enrollment token."),
		mcpgo.WithString("name",
			mcpgo.Required(),
			mcpgo.Description("Name to assign to the device in Wendy Cloud"),
		),
		mcpgo.WithString("cloud_grpc",
			mcpgo.Description("Cloud gRPC endpoint to use, e.g. cloud.wendy.dev:443 (optional when a default session is set via 'wendy auth use')"),
		),
	}
	enrollOpts = append(enrollOpts, mutating()...)
	enrollOpts = append(enrollOpts, idempotent()...)
	enrollOpts = append(enrollOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("cloud_enroll_device", enrollOpts...), s.handleCloudEnrollDevice)

	tunnelOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Forward a local TCP or UDP port to a port on a cloud-enrolled device"),
		mcpgo.WithInteger("local_port",
			mcpgo.Required(),
			mcpgo.Min(1), mcpgo.Max(65535),
			mcpgo.Description("Local loopback port to listen on"),
		),
		mcpgo.WithInteger("remote_port",
			mcpgo.Min(1), mcpgo.Max(65535),
			mcpgo.Description("Remote device port; defaults to local_port"),
		),
		mcpgo.WithString("protocol",
			mcpgo.Enum("tcp", "udp"),
			mcpgo.Description("Transport protocol to forward: tcp or udp (default tcp)"),
		),
		mcpgo.WithString("device_name",
			mcpgo.Description("Device name; optional only when exactly one cloud device is available"),
		),
		mcpgo.WithString("cloud_grpc",
			mcpgo.Description("Cloud gRPC endpoint to use, e.g. cloud.wendy.dev:443 (optional when a default session is set via 'wendy auth use')"),
		),
		mcpgo.WithString("broker_url",
			mcpgo.Description("Tunnel broker host:port (default: cloud :443 endpoint, otherwise <cloud-host>:50052)"),
		),
	}
	tunnelOpts = append(tunnelOpts, mutating()...)
	tunnelOpts = append(tunnelOpts, idempotent()...)
	tunnelOpts = append(tunnelOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("cloud_tunnel", tunnelOpts...), s.handleCloudTunnel)
	s.registerCloudTunnelManagementTools(srv)

	pingOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Ping a cloud-enrolled device through the Wendy Cloud tunnel broker using an echo request/reply over the datagram session (no ICMP sockets or privileges required)"),
		mcpgo.WithString("device_name",
			mcpgo.Required(),
			mcpgo.Description("Device name"),
		),
		mcpgo.WithNumber("count",
			mcpgo.Description("Number of echoes to send (default 4, max 20)"),
		),
		mcpgo.WithString("cloud_grpc",
			mcpgo.Description("Cloud gRPC endpoint to use, e.g. cloud.wendy.dev:443 (optional when a default session is set via 'wendy auth use')"),
		),
		mcpgo.WithString("broker_url",
			mcpgo.Description("Tunnel broker host:port (default: cloud :443 endpoint, otherwise <cloud-host>:50052)"),
		),
	}
	pingOpts = append(pingOpts, readOnly()...)
	pingOpts = append(pingOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("cloud_ping", pingOpts...), s.handleCloudPing)

	runOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Build and deploy a local project to device or the connected target. Returns status and build-log tail. Check container_list and telemetry_logs for application readiness."),
		mcpgo.WithString("project_path", mcpgo.Required(), mcpgo.Description("Project directory to build and deploy")),
		mcpgo.WithString("device", mcpgo.Description("device selector from device_list (host:port, vm:NAME, or a cloud:// selector); omit to reuse the connection")),
		mcpgo.WithString("build_type", mcpgo.Enum("docker", "compose", "swift", "python"), mcpgo.Description("Build system; omit for automatic detection")),
		mcpgo.WithString("product", mcpgo.Description("Swift package product")),
		mcpgo.WithBoolean("debug", mcpgo.Description("Start under a debugger")),
		mcpgo.WithBoolean("start", mcpgo.DefaultBool(true), mcpgo.Description("Start after deployment; false only creates the container")),
		mcpgo.WithInteger("timeout_seconds", mcpgo.Min(1), mcpgo.Max(3600), mcpgo.DefaultNumber(300), mcpgo.Description("Command timeout")),
		mcpgo.WithInteger("max_bytes", mcpgo.Min(1), mcpgo.Max(1000000), mcpgo.DefaultNumber(16384), mcpgo.Description("Build-log tail byte limit")),
	}
	runOpts = append(runOpts, mutating()...)
	runOpts = append(runOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("run", runOpts...), s.handleRun)
}

func (s *mcpServer) handleCloudDiscover(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	auth, err := s.cloudAuthEntry(stringParam(req, "cloud_grpc"))
	if err != nil {
		return cloudErrResult(err), nil
	}
	filter := stringParam(req, "filter")
	onlineOnly := req.GetBool("online_only", true)
	out, err := discoverCloudDevices(ctx, auth, filter, onlineOnly)
	if err != nil {
		return cloudErrResult(err), nil
	}
	return okListBounded("devices", out, intParam(req, "max_bytes", 100000)), nil
}

func discoverCloudDevices(ctx context.Context, auth *config.AuthConfig, filter string, onlineOnly bool) ([]map[string]any, error) {
	// A v2 (UUID-identity) session must use the v2 AssetService — the v1 arm
	// sends cert.OrganizationID (0 for these sessions) and silently returns an
	// empty roster (WDY-3146). Legacy sessions keep the v1 arm.
	var out []map[string]any
	if len(auth.Certificates) > 0 && auth.Certificates[0].TenantUUID() != "" {
		assets, err := mcpListCloudAssetsV2(ctx, auth, filter, onlineOnly)
		if err != nil {
			return nil, err
		}
		out = make([]map[string]any, 0, len(assets))
		for _, a := range assets {
			entry := cloudAssetV2ToMap(a)
			entry["device"] = cloudCommandTarget(auth, mcpCloudDevice{name: a.GetName(), key: a.GetId(), isV2: true}, "").Selector
			out = append(out, entry)
		}
	} else {
		assets, err := mcpListCloudAssets(ctx, auth, filter, onlineOnly)
		if err != nil {
			return nil, err
		}
		out = make([]map[string]any, 0, len(assets))
		for _, a := range assets {
			entry := cloudAssetToMap(a)
			entry["device"] = cloudCommandTarget(auth, mcpCloudDevice{name: a.GetName(), legacyID: a.GetId()}, "").Selector
			out = append(out, entry)
		}
	}
	return out, nil
}

func (s *mcpServer) handleCloudConnect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn, device, target, err := s.connectToCloudAgent(ctx, stringParam(req, "cloud_grpc"), stringParam(req, "device_name"), stringParam(req, "broker_url"))
	if err != nil {
		return cloudErrResult(err), nil
	}
	s.setConnection(conn, "cloud", target)
	return okText(fmt.Sprintf("connected to %s via cloud", device.GetName())), nil
}

func (s *mcpServer) handleCloudEnrollDevice(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	name := stringParam(req, "name")
	if name == "" {
		return errResult(errCodeInvalidArgument, "name is required"), nil
	}
	auth, err := s.cloudAuthEntry(stringParam(req, "cloud_grpc"))
	if err != nil {
		return cloudErrResult(err), nil
	}

	if err := cloudenroll.CheckAgentEnrollment(ctx, conn.Conn); err != nil {
		return cloudErrResult(err), nil
	}

	// Same EAB path as the CLI: mint device_id, operator-sign the enrollment
	// request, EnrollDevice -> EAB, then have the connected agent ACME-enroll.
	deviceID := uuid.NewString()
	cfg, err := cloudenroll.EnrollmentConfig(auth, deviceID, "")
	if err != nil {
		return cloudErrResult(err), nil
	}
	cloudConn, err := mcpDialCloudGRPC(auth)
	if err != nil {
		return cloudErrResult(err), nil
	}
	defer cloudConn.Close()
	tokenCtx, err := mcpCloudContext(ctx, auth)
	if err != nil {
		return cloudErrResult(err), nil
	}
	cfg, assetID, err := cloudenroll.MintEAB(tokenCtx, cloudConn, auth, cfg, name)
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}

	resp, err := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn).StartACMEProvisioning(ctx, &agentpbv2.StartACMEProvisioningRequest{
		CloudHost: auth.CloudGRPC, DirectoryUrl: cfg.DirectoryURL, DeviceId: cfg.DeviceID,
		EabKeyId: cfg.EABKeyID, EabHmacKey: cfg.EABHMACKey,
	})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	out := map[string]any{
		"asset_id":      assetID,
		"device_id":     cfg.DeviceID,
		"principal_uri": resp.GetPrincipalUri(),
		"cloud_host":    auth.CloudGRPC,
	}
	return okResult(out), nil
}

func (s *mcpServer) handleCloudTunnel(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	localPort, err := ros2Int(req, "local_port", 0, 1, 65535)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	if localPort == 0 {
		return errResult(errCodeInvalidArgument, "local_port is required"), nil
	}
	remotePort, err := ros2Int(req, "remote_port", localPort, 1, 65535)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	protocol := stringParam(req, "protocol")
	if protocol == "" {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "udp" {
		return errResult(errCodeInvalidArgument, `protocol must be "tcp" or "udp"`), nil
	}

	auth, err := s.cloudAuthEntry(stringParam(req, "cloud_grpc"))
	if err != nil {
		return cloudErrResult(err), nil
	}
	device, err := s.resolveCloudDevice(ctx, auth, stringParam(req, "device_name"))
	if err != nil {
		return cloudErrResult(err), nil
	}
	// v2 has no datagram relay (cloudrelay is TCP-only); refuse UDP there,
	// mirroring the CLI. v2 ping/UDP is tracked as a WDY-3148 follow-up.
	if protocol == "udp" && device.isV2 {
		return errResult(errCodeInvalidArgument, "Cloud's authorized service catalog does not expose UDP forwarding over v2 sessions"), nil
	}
	target := cloudCommandTarget(auth, device, stringParam(req, "broker_url"))
	s.mu.RLock()
	for _, existing := range s.cloudTunnels {
		if existing.info.Device == target.Selector && existing.brokerURL == stringParam(req, "broker_url") && existing.info.Protocol == protocol && existing.info.LocalPort == localPort && existing.info.RemotePort == remotePort {
			info := existing.info
			s.mu.RUnlock()
			return okResult(info), nil
		}
	}
	s.mu.RUnlock()
	// The v1 datagram and v1 broker tunnel need a broker connection; the v2
	// relay selects its own broker, so brokerConn stays nil there.
	var brokerConn *grpc.ClientConn
	if !device.isV2 {
		brokerConn, err = clouddefaults.DialBroker(auth, stringParam(req, "broker_url"))
		if err != nil {
			return cloudErrResult(err), nil
		}
	} else if stringParam(req, "broker_url") != "" {
		return errResult(errCodeInvalidArgument, "Cloud selects the authorized relay; broker_url is supported only for legacy sessions"), nil
	}
	closeBroker := func() {
		if brokerConn != nil {
			_ = brokerConn.Close()
		}
	}

	key := uuid.NewString()
	info := cloudTunnelInfo{ID: key, Protocol: protocol, Device: target.Selector, DeviceName: device.GetName(), DeviceID: device.key, CloudGRPC: target.CloudGRPC, LocalPort: localPort, RemotePort: remotePort, CreatedAt: time.Now().UTC()}
	tunnelCtx, cancel := context.WithCancel(context.Background())

	if protocol == "udp" {
		// Legacy sessions only (v2 refused above).
		pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: localPort})
		if err != nil {
			cancel()
			closeBroker()
			return errResultf(errCodeInternal, "listening on udp 127.0.0.1:%d: %s", localPort, err.Error()), nil
		}
		session, err := mcpOpenDatagramSession(tunnelCtx, brokerConn, auth, device.legacyID)
		if err != nil {
			cancel()
			_ = pc.Close()
			closeBroker()
			return errResult(errCodeDeviceUnreachable, mcpDatagramOpenError(err, device.GetName()).Error()), nil
		}

		info.LocalAddr = pc.LocalAddr().String()
		tunnel := &mcpCloudTunnel{info: info, cancel: cancel, udpConn: pc, session: session, brokerConn: brokerConn, brokerURL: stringParam(req, "broker_url")}
		if !s.addCloudTunnel(key, tunnel) {
			return errResult(errCodeInternal, "MCP server is shutting down"), nil
		}

		go func() {
			defer s.removeCloudTunnel(key, tunnel)
			_ = mcpServeUDPForward(tunnelCtx, pc, session, uint32(remotePort), mcpUDPFlowIdleTimeout)
		}()

		return okResult(info), nil
	}

	listenAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		cancel()
		closeBroker()
		return errResultf(errCodeInternal, "listening on %s: %s", listenAddr, err.Error()), nil
	}
	info.LocalAddr = ln.Addr().String()
	tunnel := &mcpCloudTunnel{info: info, cancel: cancel, listener: ln, brokerConn: brokerConn, brokerURL: stringParam(req, "broker_url")}
	if !s.addCloudTunnel(key, tunnel) {
		return errResult(errCodeInternal, "MCP server is shutting down"), nil
	}

	go func() {
		defer s.removeCloudTunnel(key, tunnel)
		for {
			tcpConn, err := ln.Accept()
			if err != nil {
				return
			}
			go mcpServeTunnelConn(tunnelCtx, tcpConn, func(c context.Context) (net.Conn, error) {
				return device.openTunnel(c, brokerConn, auth, uint32(remotePort))
			})
		}
	}()

	return okResult(info), nil
}

func (s *mcpServer) handleCloudPing(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	deviceName := stringParam(req, "device_name")
	if deviceName == "" {
		return errResult(errCodeInvalidArgument, "device_name is required"), nil
	}
	count := intParam(req, "count", 4)
	if count < 1 || count > 20 {
		return errResult(errCodeInvalidArgument, "count must be between 1 and 20"), nil
	}

	auth, err := s.cloudAuthEntry(stringParam(req, "cloud_grpc"))
	if err != nil {
		return cloudErrResult(err), nil
	}

	// v2 sessions have no broker DATAGRAM/ping form (the v2 relay is TCP-only),
	// so ping RTT is measured with an agent-RPC round-trip over the authorized
	// tunnel, mirroring the CLI. Legacy sessions keep the v1 datagram echo.
	if isV2Session(auth) {
		conn, device, _, err := s.connectToCloudAgent(ctx, stringParam(req, "cloud_grpc"), deviceName, stringParam(req, "broker_url"))
		if err != nil {
			return cloudErrResult(err), nil
		}
		defer conn.Close()
		stats := mcpRunPingLoop(ctx, newMCPAgentRPCPingSession(ctx, conn), device.GetName(), count, time.Second, io.Discard)
		return mcpPingResult(stats, device.GetName()), nil
	}

	asset, err := s.pickCloudAsset(ctx, auth, deviceName)
	if err != nil {
		return cloudErrResult(err), nil
	}
	brokerConn, err := clouddefaults.DialBroker(auth, stringParam(req, "broker_url"))
	if err != nil {
		return errResult(errCodeDeviceUnreachable, err.Error()), nil
	}
	defer brokerConn.Close()

	session, err := mcpOpenDatagramSession(ctx, brokerConn, auth, asset.GetId())
	if err != nil {
		return errResult(errCodeDeviceUnreachable, mcpDatagramOpenError(err, asset.GetName()).Error()), nil
	}
	defer session.close()

	stats := mcpRunPingLoop(ctx, session, asset.GetName(), count, time.Second, io.Discard)
	return mcpPingResult(stats, asset.GetName()), nil
}

// mcpPingResult renders ping stats into a tool result, shared by the v1
// datagram and v2 agent-RPC arms. On zero replies it surfaces the transport
// error (mcpDatagramOpenError folds DeadlineExceeded/Unavailable into the
// offline/old-agent hint) or, for a silent device, the generic hint.
func mcpPingResult(stats mcpPingStats, name string) *mcpgo.CallToolResult {
	if stats.Received == 0 {
		if stats.Err != nil {
			return errResult(codeFromGRPC(stats.Err), mcpDatagramOpenError(stats.Err, name).Error())
		}
		return errResultf(errCodeDeviceUnreachable, "no replies from %s: the device may be offline or need a WendyOS update for ping support", name)
	}
	out := map[string]any{
		"sent":       stats.Sent,
		"received":   stats.Received,
		"min_rtt_ms": stats.Min.Seconds() * 1000,
		"avg_rtt_ms": stats.Avg.Seconds() * 1000,
		"max_rtt_ms": stats.Max.Seconds() * 1000,
	}
	return okResult(out)
}

// cloudResolveErr is returned by the cloud auth/asset-resolution helpers
// carrying the precise error_code the MCP layer should surface.
type cloudResolveErr struct {
	code errorCode
	msg  string
}

func (e *cloudResolveErr) Error() string { return e.msg }

// cloudErrResult maps an error from the cloud auth/asset-resolution helpers
// (cloudAuthEntry, pickCloudAsset, connectToCloudAgent, mcpListCloudAssets) to
// an MCP error result with the correct error_code. Tagged errors get their
// specific code; anything else (including real gRPC errors, which may be
// wrapped with fmt.Errorf %w) falls back to codeFromGRPC.
func cloudErrResult(err error) *mcpgo.CallToolResult {
	var re *cloudResolveErr
	if errors.As(err, &re) {
		return errResult(re.code, re.msg)
	}
	return errResult(codeFromGRPC(err), grpcErrString(err))
}

func (s *mcpServer) cloudAuthEntry(cloudGRPC string) (*config.AuthConfig, error) {
	// MCP is non-interactive: pass a nil picker so resolution stops at the
	// persisted default (or errors when several sessions remain ambiguous).
	auth, err := config.ResolveAuth(s.currentConfig(), cloudGRPC, nil)
	if errors.Is(err, config.ErrNotLoggedIn) {
		return nil, &cloudResolveErr{code: errCodeAuthRequired, msg: "not signed in to Wendy Cloud; call auth_login and show the user the link (or run 'wendy auth login' in a terminal)"}
	}
	if errors.Is(err, config.ErrMultipleSessions) {
		return nil, &cloudResolveErr{code: errCodeMultipleSessions, msg: "multiple auth sessions exist; pass cloud_grpc to select one, or set a default with 'wendy auth use'"}
	}
	return auth, err
}

func cloudCommandTarget(auth *config.AuthConfig, asset interface{ GetName() string }, brokerURL string) commandTarget {
	if auth == nil || auth.CloudGRPC == "" || asset.GetName() == "" {
		return commandTarget{}
	}
	target := commandTarget{
		Device:    asset.GetName(),
		Transport: "cloud",
		CloudGRPC: auth.CloudGRPC,
		BrokerURL: brokerURL,
	}
	// A subprocess reloads credentials from disk. Pin the org/tenant and asset
	// as well as the endpoint so a concurrent context switch cannot redirect it
	// to a same-named robot in another organization.
	if device, ok := asset.(mcpCloudDevice); ok && len(auth.Certificates) > 0 {
		cert := auth.Certificates[0]
		var path string
		if device.isV2 && cert.TenantUUID() != "" && device.key != "" {
			path = fmt.Sprintf("/tenant/%s/asset/%s", cert.TenantUUID(), device.key)
		} else if !device.isV2 && cert.OrganizationID > 0 && device.legacyID > 0 {
			path = fmt.Sprintf("/org/%d/asset/%d", cert.OrganizationID, device.legacyID)
		}
		if path != "" {
			target.Selector = (&url.URL{Scheme: "cloud", Host: auth.CloudGRPC, Path: path}).String()
		}
	}
	return target
}

func (s *mcpServer) connectToCloudAgent(ctx context.Context, cloudGRPC, deviceName, brokerURL string) (*grpcclient.AgentConnection, mcpCloudDevice, commandTarget, error) {
	auth, err := s.cloudAuthEntry(cloudGRPC)
	if err != nil {
		return nil, mcpCloudDevice{}, commandTarget{}, err
	}
	device, err := s.resolveCloudDevice(ctx, auth, deviceName)
	if err != nil {
		return nil, mcpCloudDevice{}, commandTarget{}, err
	}
	return connectPinnedMCPCloudAgent(ctx, auth, device, brokerURL)
}

func connectPinnedMCPCloudAgent(ctx context.Context, auth *config.AuthConfig, device mcpCloudDevice, brokerURL string) (*grpcclient.AgentConnection, mcpCloudDevice, commandTarget, error) {
	var err error

	// Legacy sessions dial the v1 broker; the v2 relay picks its own broker, so
	// there is no broker connection to hold for a v2 session.
	var brokerConn *grpc.ClientConn
	if !device.isV2 {
		brokerConn, err = clouddefaults.DialBroker(auth, brokerURL)
		if err != nil {
			return nil, mcpCloudDevice{}, commandTarget{}, err
		}
	} else if brokerURL != "" {
		return nil, mcpCloudDevice{}, commandTarget{}, fmt.Errorf("Cloud selects the authorized relay; broker_url is supported only for legacy sessions")
	}
	cleanupBroker := true
	defer func() {
		if cleanupBroker && brokerConn != nil {
			_ = brokerConn.Close()
		}
	}()

	// Provisioned agents serve mTLS on agentPort+1 (50052) for remote clients.
	dialOpt := clouddefaults.TunnelDialer(func(tunnelCtx context.Context) (net.Conn, error) {
		return device.openTunnel(tunnelCtx, brokerConn, auth, 50052)
	})

	certInfo := auth.Certificates[0]
	keyPEM, err := certInfo.PrivateKeyPEM()
	if err != nil {
		return nil, mcpCloudDevice{}, commandTarget{}, fmt.Errorf("loading client key: %w", err)
	}
	x509Cert, err := tls.X509KeyPair([]byte(certInfo.PemCertificate), []byte(keyPEM))
	if err != nil {
		return nil, mcpCloudDevice{}, commandTarget{}, fmt.Errorf("loading agent mTLS cert: %w", err)
	}
	verifyConn, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM:      certInfo.PemCertificateChain,
		ExpectedOrgID: int32(certInfo.OrganizationID),
	})
	if err != nil {
		return nil, mcpCloudDevice{}, commandTarget{}, fmt.Errorf("building TLS verifier: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{x509Cert},
		InsecureSkipVerify: true, //nolint:gosec — hostname bypass only; VerifyConnection validates server cert against Wendy PKI
		VerifyConnection:   verifyConn,
		MinVersion:         tls.VersionTLS12,
	}
	grpcConn, err := grpc.NewClient(
		"passthrough:///cloud-tunnel",
		dialOpt,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
	)
	if err != nil {
		return nil, mcpCloudDevice{}, commandTarget{}, fmt.Errorf("creating tunnelled gRPC connection: %w", err)
	}
	agentConn := grpcclient.NewFromConn(grpcConn)
	agentConn.Host = device.GetName()
	if !device.isV2 {
		agentConn.MeshHost = meshname.Device(device.legacyID)
	}
	agentConn.IsMTLS = true
	agentConn.RegistryDialer = func(ctx context.Context, port int) (net.Conn, error) {
		return device.openTunnel(ctx, brokerConn, auth, uint32(port))
	}
	agentConn.Reconnect = func(ctx context.Context) (*grpcclient.AgentConnection, error) {
		next, _, _, err := connectPinnedMCPCloudAgent(ctx, auth, device, brokerURL)
		return next, err
	}
	if brokerConn != nil {
		agentConn.ExtraClosers = append(agentConn.ExtraClosers, brokerConn)
	}
	cleanupBroker = false
	return agentConn, device, cloudCommandTarget(auth, device, brokerURL), nil
}

func (s *mcpServer) pickCloudAsset(ctx context.Context, auth *config.AuthConfig, deviceName string) (*cloudpb.Asset, error) {
	assets, err := mcpListCloudAssets(ctx, auth, "", true)
	if err != nil {
		return nil, err
	}
	if deviceName == "" {
		if len(assets) == 0 {
			return nil, &cloudResolveErr{code: errCodeNotFound, msg: "no enrolled devices found for this org; enroll a device with cloud_enroll_device"}
		}
		if len(assets) == 1 {
			return assets[0], nil
		}
		return nil, &cloudResolveErr{code: errCodeInvalidArgument, msg: "multiple cloud devices found; pass device_name"}
	}

	lower := strings.ToLower(deviceName)
	var matched *cloudpb.Asset
	for _, a := range assets {
		if strings.ToLower(a.GetName()) == lower {
			if matched != nil {
				return nil, &cloudResolveErr{code: errCodeInvalidArgument, msg: fmt.Sprintf("multiple devices match %q; use a more specific name", deviceName)}
			}
			matched = a
		}
	}
	if matched == nil {
		// Numeric asset-id fallback: allows targeting unnamed (or
		// differently-named) devices by id, mirroring resolveCloudAsset's
		// fallback in commands/cloud_tunnel.go. No ambiguity check here —
		// ids are unique, unlike names, so unlike the name loop above a
		// second match can't happen.
		if id, err := strconv.Atoi(strings.TrimSpace(deviceName)); err == nil {
			for _, a := range assets {
				if a.GetId() == int32(id) {
					matched = a
					break
				}
			}
		}
	}
	if matched != nil {
		return matched, nil
	}

	// No match in the online-only listing (by name or id), including the
	// degenerate case where that listing was empty. Re-check the
	// offline-inclusive listing before concluding the device doesn't exist:
	// it may just be enrolled-but-unreachable right now.
	if err := s.offlineDeviceErr(ctx, auth, deviceName); err != nil {
		return nil, err
	}
	return nil, &cloudResolveErr{code: errCodeNotFound, msg: fmt.Sprintf("no device named %q found; call cloud_discover to list devices", deviceName)}
}

// offlineDeviceErr distinguishes "enrolled but currently offline" from
// "never enrolled" once the online-only asset listing has failed to produce
// a match for deviceName. It re-queries the cloud without the online-only
// filter; if the device turns up there (by name or numeric id — see
// clouddefaults.FindAssetByNameOrID), it's enrolled but unreachable right
// now, so that's reported as errCodeDeviceUnreachable instead of the
// generic NOT_FOUND the caller would otherwise return. Returns nil (not an
// error) both when the re-query also misses AND when the re-query itself
// fails (e.g. a transient cloud-API outage) — in both cases the caller's
// original NOT_FOUND message is preserved unchanged, mirroring
// upgradeOfflineResolveErr in commands/cloud_tunnel.go. Surfacing a re-query
// transport failure here instead would route through
// cloudErrResult/codeFromGRPC and, for an Unavailable-shaped status,
// mislabel a cloud-API outage as DEVICE_UNREACHABLE — implying this
// specific device is known-and-offline, when the lookup itself simply
// couldn't be completed.
func (s *mcpServer) offlineDeviceErr(ctx context.Context, auth *config.AuthConfig, deviceName string) error {
	all, err := mcpListCloudAssets(ctx, auth, "", false)
	if err != nil {
		return nil
	}
	if clouddefaults.FindAssetByNameOrID(all, deviceName) == nil {
		return nil
	}
	return &cloudResolveErr{code: errCodeDeviceUnreachable, msg: fmt.Sprintf("device %q is enrolled but currently reported offline; check the device's power and network connection, or call cloud_discover with online_only=false to list enrolled devices", deviceName)}
}

func mcpListCloudAssets(ctx context.Context, auth *config.AuthConfig, filter string, onlineOnly bool) ([]*cloudpb.Asset, error) {
	conn, err := mcpDialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	pageSize := int32(200)
	req := &cloudpb.ListAssetsRequest{
		OrganizationId:  int32(auth.Certificates[0].OrganizationID),
		IsComputeDevice: boolPtr(true),
		Limit:           &pageSize,
	}
	if filter != "" {
		req.Filter = &filter
	}
	if onlineOnly {
		req.OnlineOnly = boolPtr(true)
	}
	client := cloudpb.NewAssetServiceClient(conn)
	cloudCtx, err := mcpCloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}
	const maxAssets = 10_000
	var assets []*cloudpb.Asset
	for offset := int32(0); ; {
		req.Offset = &offset
		stream, err := client.ListAssets(cloudCtx, req)
		if err != nil {
			return nil, fmt.Errorf("listing devices: %w", err)
		}
		var page, total int32
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("listing devices: %w", err)
			}
			if len(assets) >= maxAssets {
				return nil, &cloudResolveErr{code: errCodeInvalidArgument, msg: fmt.Sprintf("cloud returned more than %d devices", maxAssets)}
			}
			assets = append(assets, resp.GetAsset())
			page++
			total = resp.GetTotal()
		}
		offset += page
		// Match the CLI's pagination, including its empty-page guard for a
		// changing roster or an inconsistent server-reported total.
		if page == 0 || offset >= total {
			break
		}
	}
	return assets, nil
}

func mcpCloudContext(ctx context.Context, auth *config.AuthConfig) (context.Context, error) {
	if len(auth.Certificates) == 0 {
		return ctx, nil
	}
	certInfo := auth.Certificates[0]
	md := metadata.MD{}
	// A DPoP-bound token goes out as DPoP via the shared interceptor
	// (mcpDialCloudGRPC installs it); only unbound API-key/legacy sessions carry
	// Bearer here (WDY-3107).
	if auth.HasAPIKey() && !cloudrequest.IsDPoPBound(auth) {
		bearerToken, err := auth.BearerToken()
		if err != nil {
			return nil, fmt.Errorf("loading API token: %w", err)
		}
		md.Set("authorization", "Bearer "+bearerToken)
	}
	// Legacy (urn:wendy) sessions only; see certXFCC in the CLI.
	if certInfo.PrincipalURI == "" {
		certHeader := fmt.Sprintf("URI=urn:wendy:org:%d:user:unknown", certInfo.OrganizationID)
		if certInfo.UserID != "" {
			certHeader = fmt.Sprintf("URI=urn:wendy:org:%d:user:%s", certInfo.OrganizationID, certInfo.UserID)
		}
		md.Set("x-wendy-client-cert", certHeader)
		md.Set("x-forwarded-client-cert", certHeader)
	}
	return metadata.NewOutgoingContext(ctx, md), nil
}

func mcpDialCloudGRPC(auth *config.AuthConfig) (*grpc.ClientConn, error) {
	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	var transport grpc.DialOption
	if clouddefaults.UsesPublicCA(auth.CloudGRPC) {
		certInfo := auth.Certificates[0]
		keyPEM, err := certInfo.PrivateKeyPEM()
		if err != nil {
			return nil, fmt.Errorf("loading client key: %w", err)
		}
		tlsCfg, err := certs.LoadTLSConfig(
			certInfo.PemCertificate,
			certInfo.PemCertificateChain,
			keyPEM,
			"",
		)
		if err != nil {
			return nil, fmt.Errorf("loading TLS config: %w", err)
		}
		transport = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	} else {
		transport = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	dialOptions := []grpc.DialOption{transport}
	// Per-RPC DPoP proof for a cnf-bound token; nil for unbound sessions. The
	// MCP provider uses the stored token without refreshing (WDY-3107).
	dialOptions = append(dialOptions, cloudrequest.DPoPDialOptions(auth, mcpDPoPTokenProvider(auth))...)
	conn, err := grpc.NewClient(auth.CloudGRPC, dialOptions...)
	if err != nil {
		return nil, fmt.Errorf("connecting to cloud: %w", err)
	}
	return conn, nil
}

// mcpDPoPTokenProvider returns the stored access token and its bound ML-DSA key
// for a DPoP proof. Unlike the CLI it does NOT auto-refresh — the MCP tools path
// has never refreshed OAuth tokens, so this only adds the proof (no behavior
// change); an expired token still fails the same way it does today.
func mcpDPoPTokenProvider(auth *config.AuthConfig) cloudrequest.DPoPTokenProvider {
	return func(context.Context) (string, crypto.Signer, error) {
		keyPEM, err := auth.OAuthDPoPKey()
		if err != nil {
			return "", nil, fmt.Errorf("DPoP: loading bound key: %w", err)
		}
		key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
		if err != nil {
			return "", nil, fmt.Errorf("DPoP: parsing bound key: %w", err)
		}
		return auth.APIKey, key, nil
	}
}

func mcpOpenBrokerTunnel(ctx context.Context, brokerConn *grpc.ClientConn, auth *config.AuthConfig, assetID int32, remotePort uint32) (net.Conn, error) {
	cloudCtx, err := mcpCloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}
	stream, err := cloudpb.NewTunnelBrokerServiceClient(brokerConn).ClientTunnel(cloudCtx)
	if err != nil {
		return nil, fmt.Errorf("opening tunnel stream: %w", err)
	}
	if err := stream.Send(&cloudpb.ClientTunnelMessage{
		Content: &cloudpb.ClientTunnelMessage_Open{
			Open: &cloudpb.ClientTunnelOpen{
				AssetId: assetID,
				Host:    "localhost",
				Port:    remotePort,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("sending tunnel open: %w", err)
	}

	local, remote := net.Pipe()
	// Record the broker's verdict (a non-EOF stream end) on the local end so
	// callers see why the tunnel died instead of a bare EOF; see BrokerTunnelConn.
	tunnel := clouddefaults.NewBrokerTunnelConn(local)
	go func() {
		defer remote.Close()
		for {
			msg, err := stream.Recv()
			if err != nil {
				tunnel.Fail(err)
				break
			}
			if len(msg.Payload) > 0 {
				if _, err := remote.Write(msg.Payload); err != nil {
					break
				}
			}
			if msg.HalfClose {
				break
			}
		}
	}()
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, readErr := remote.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				if err := stream.Send(&cloudpb.ClientTunnelMessage{
					Content: &cloudpb.ClientTunnelMessage_Data{
						Data: &cloudpb.TunnelData{Payload: payload},
					},
				}); err != nil {
					break
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					_ = stream.Send(&cloudpb.ClientTunnelMessage{
						Content: &cloudpb.ClientTunnelMessage_Data{
							Data: &cloudpb.TunnelData{HalfClose: true},
						},
					})
				}
				break
			}
		}
		_ = stream.CloseSend()
	}()
	return tunnel, nil
}

func mcpServeTunnelConn(ctx context.Context, tcpConn net.Conn, dial func(context.Context) (net.Conn, error)) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopClose := context.AfterFunc(ctx, func() { _ = tcpConn.Close() })
	defer stopClose()
	defer tcpConn.Close()
	tunnelConn, err := dial(ctx)
	if err != nil {
		return
	}
	defer tunnelConn.Close()
	done := make(chan struct{}, 2)
	relay := func(dst io.Writer, src io.Reader) {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(dst, src)
	}
	go relay(tunnelConn, tcpConn)
	go relay(tcpConn, tunnelConn)
	select {
	case <-done:
	case <-ctx.Done():
	}
}

func cloudAssetToMap(a *cloudpb.Asset) map[string]any {
	out := map[string]any{
		"id":                 a.GetId(),
		"organization_id":    a.GetOrganizationId(),
		"name":               a.GetName(),
		"asset_type":         a.GetAssetType(),
		"is_compute_device":  a.GetIsComputeDevice(),
		"created_at_unix_ns": int64(0),
	}
	if a.GetCreatedAt() != nil {
		out["created_at_unix_ns"] = a.GetCreatedAt().AsTime().UnixNano()
	}
	if a.DeviceType != nil {
		out["device_type"] = a.GetDeviceType()
	}
	if a.Architecture != nil {
		out["architecture"] = a.GetArchitecture()
	}
	if a.OsType != nil {
		out["os_type"] = a.GetOsType()
	}
	if a.OsVersion != nil {
		out["os_version"] = a.GetOsVersion()
	}
	if a.IpAddress != nil {
		out["ip_address"] = a.GetIpAddress()
	}
	return out
}

func validatePort(port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("must be between 1 and 65535")
	}
	return nil
}

func boolPtr(v bool) *bool {
	return &v
}
