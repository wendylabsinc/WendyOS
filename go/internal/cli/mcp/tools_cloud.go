package mcp

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const mcpDefaultBrokerPort = "50052"

type mcpCloseFunc func()

func (f mcpCloseFunc) Close() error {
	f()
	return nil
}

type mcpCloudTunnel struct {
	cancel     context.CancelFunc
	listener   net.Listener
	brokerConn *grpc.ClientConn
}

func (t *mcpCloudTunnel) Close() error {
	if t == nil {
		return nil
	}
	if t.cancel != nil {
		t.cancel()
	}
	var errs []error
	if t.listener != nil {
		errs = append(errs, t.listener.Close())
	}
	if t.brokerConn != nil {
		errs = append(errs, t.brokerConn.Close())
	}
	return errors.Join(errs...)
}

func (s *mcpServer) registerCloudTools(srv *server.MCPServer) {
	srv.AddTool(mcpgo.NewTool("cloud_discover",
		mcpgo.WithDescription("List enrolled cloud devices for the selected Wendy Cloud auth session"),
		mcpgo.WithString("cloud_grpc",
			mcpgo.Description("Cloud gRPC endpoint to use when multiple auth sessions exist"),
		),
		mcpgo.WithBoolean("online_only",
			mcpgo.Description("Only list devices with active tunnel broker presence (default true)"),
		),
		mcpgo.WithString("filter",
			mcpgo.Description("Optional cloud-side asset filter"),
		),
	), s.handleCloudDiscover)

	srv.AddTool(mcpgo.NewTool("cloud_connect",
		mcpgo.WithDescription("Connect the MCP session to a cloud-enrolled device through the Wendy Cloud tunnel"),
		mcpgo.WithString("device_name",
			mcpgo.Description("Device name; optional only when exactly one cloud device is available"),
		),
		mcpgo.WithString("cloud_grpc",
			mcpgo.Description("Cloud gRPC endpoint to use when multiple auth sessions exist"),
		),
		mcpgo.WithString("broker_url",
			mcpgo.Description("Tunnel broker host:port (default: <cloud-host>:50052)"),
		),
	), s.handleCloudConnect)

	srv.AddTool(mcpgo.NewTool("cloud_device_connect",
		mcpgo.WithDescription("Alias for cloud_connect; connects existing MCP device tools through the Wendy Cloud tunnel"),
		mcpgo.WithString("device_name",
			mcpgo.Description("Device name; optional only when exactly one cloud device is available"),
		),
		mcpgo.WithString("cloud_grpc",
			mcpgo.Description("Cloud gRPC endpoint to use when multiple auth sessions exist"),
		),
		mcpgo.WithString("broker_url",
			mcpgo.Description("Tunnel broker host:port (default: <cloud-host>:50052)"),
		),
	), s.handleCloudConnect)

	srv.AddTool(mcpgo.NewTool("cloud_enroll_device",
		mcpgo.WithDescription("Enroll the currently connected device with Wendy Cloud"),
		mcpgo.WithString("name",
			mcpgo.Required(),
			mcpgo.Description("Name to assign to the device in Wendy Cloud"),
		),
		mcpgo.WithString("cloud_grpc",
			mcpgo.Description("Cloud gRPC endpoint to use when multiple auth sessions exist"),
		),
	), s.handleCloudEnrollDevice)

	srv.AddTool(mcpgo.NewTool("cloud_tunnel",
		mcpgo.WithDescription("Forward a local TCP port to a port on a cloud-enrolled device"),
		mcpgo.WithNumber("local_port",
			mcpgo.Required(),
			mcpgo.Description("Local TCP port to listen on"),
		),
		mcpgo.WithNumber("remote_port",
			mcpgo.Description("Remote device port; defaults to local_port"),
		),
		mcpgo.WithString("device_name",
			mcpgo.Description("Device name; optional only when exactly one cloud device is available"),
		),
		mcpgo.WithString("cloud_grpc",
			mcpgo.Description("Cloud gRPC endpoint to use when multiple auth sessions exist"),
		),
		mcpgo.WithString("broker_url",
			mcpgo.Description("Tunnel broker host:port (default: <cloud-host>:50052)"),
		),
	), s.handleCloudTunnel)

	srv.AddTool(mcpgo.NewTool("cloud_run",
		mcpgo.WithDescription("Run 'wendy cloud run' for a local project and return bounded command output"),
		mcpgo.WithString("project_path",
			mcpgo.Required(),
			mcpgo.Description("Project directory containing wendy.json"),
		),
		mcpgo.WithString("device_name",
			mcpgo.Description("Cloud device name"),
		),
		mcpgo.WithString("cloud_grpc",
			mcpgo.Description("Cloud gRPC endpoint to use when multiple auth sessions exist"),
		),
		mcpgo.WithString("broker_url",
			mcpgo.Description("Tunnel broker host:port (default: <cloud-host>:50052)"),
		),
		mcpgo.WithString("build_type",
			mcpgo.Description("Build type: docker, swift, or python"),
		),
		mcpgo.WithString("product",
			mcpgo.Description("Swift Package Manager product to build and run"),
		),
		mcpgo.WithBoolean("debug",
			mcpgo.Description("Enable debug logging"),
		),
		mcpgo.WithBoolean("deploy",
			mcpgo.Description("Create container but do not start it"),
		),
		mcpgo.WithBoolean("detach",
			mcpgo.Description("Start container but do not stream logs (default true for MCP)"),
		),
		mcpgo.WithNumber("timeout_seconds",
			mcpgo.Description("Maximum command runtime in seconds (default 300)"),
		),
	), s.handleCloudRun)
}

func (s *mcpServer) handleCloudDiscover(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	auth, err := s.cloudAuthEntry(stringParam(req, "cloud_grpc"))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	assets, err := mcpListCloudAssets(ctx, auth, stringParam(req, "filter"), req.GetBool("online_only", true))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	out := make([]map[string]any, 0, len(assets))
	for _, a := range assets {
		out = append(out, cloudAssetToMap(a))
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}

func (s *mcpServer) handleCloudConnect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn, asset, err := s.connectToCloudAgent(ctx, stringParam(req, "cloud_grpc"), stringParam(req, "device_name"), stringParam(req, "broker_url"))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	s.SetConn(conn)
	return mcpgo.NewToolResultText(fmt.Sprintf("connected to %s via cloud", asset.GetName())), nil
}

func (s *mcpServer) handleCloudEnrollDevice(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	name := stringParam(req, "name")
	if name == "" {
		return mcpgo.NewToolResultError("name is required"), nil
	}
	auth, err := s.cloudAuthEntry(stringParam(req, "cloud_grpc"))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	tokenResp, err := mcpCreateAssetEnrollmentToken(ctx, auth, name)
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	cloudHost := auth.CloudGRPC
	if h, _, splitErr := net.SplitHostPort(cloudHost); splitErr == nil {
		cloudHost = h
	}
	_, err = conn.ProvisioningService.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{
		OrganizationId:  tokenResp.GetOrganizationId(),
		AssetId:         tokenResp.GetAssetId(),
		EnrollmentToken: tokenResp.GetEnrollmentToken(),
		CloudHost:       cloudHost,
	})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	out := map[string]any{
		"organization_id": tokenResp.GetOrganizationId(),
		"asset_id":        tokenResp.GetAssetId(),
		"cloud_host":      cloudHost,
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}

func (s *mcpServer) handleCloudTunnel(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	localPort := intParam(req, "local_port", 0)
	remotePort := intParam(req, "remote_port", localPort)
	if err := validatePort(localPort); err != nil {
		return mcpgo.NewToolResultError("local_port " + err.Error()), nil
	}
	if err := validatePort(remotePort); err != nil {
		return mcpgo.NewToolResultError("remote_port " + err.Error()), nil
	}

	auth, err := s.cloudAuthEntry(stringParam(req, "cloud_grpc"))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	asset, err := s.pickCloudAsset(ctx, auth, stringParam(req, "device_name"))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}
	brokerConn, err := mcpDialCloudBroker(auth, stringParam(req, "broker_url"))
	if err != nil {
		return mcpgo.NewToolResultError(err.Error()), nil
	}

	listenAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		_ = brokerConn.Close()
		return mcpgo.NewToolResultError(fmt.Sprintf("listening on %s: %s", listenAddr, err.Error())), nil
	}
	tunnelCtx, cancel := context.WithCancel(context.Background())
	tunnel := &mcpCloudTunnel{cancel: cancel, listener: ln, brokerConn: brokerConn}
	key := fmt.Sprintf("%s:%d:%d", asset.GetName(), localPort, remotePort)
	s.mu.Lock()
	if existing := s.cloudTunnels[key]; existing != nil {
		_ = existing.Close()
	}
	s.cloudTunnels[key] = tunnel
	s.mu.Unlock()

	go func() {
		for {
			tcpConn, err := ln.Accept()
			if err != nil {
				return
			}
			go mcpServeTunnelConn(tunnelCtx, tcpConn, brokerConn, auth, asset.GetId(), uint32(remotePort))
		}
	}()

	out := map[string]any{
		"id":          key,
		"local_addr":  ln.Addr().String(),
		"device_name": asset.GetName(),
		"asset_id":    asset.GetId(),
		"remote_port": remotePort,
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}

func (s *mcpServer) handleCloudRun(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	projectPath := stringParam(req, "project_path")
	if projectPath == "" {
		return mcpgo.NewToolResultError("project_path is required"), nil
	}
	timeout := time.Duration(intParam(req, "timeout_seconds", 300)) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	bin, err := os.Executable()
	if err != nil || bin == "" {
		bin = "wendy"
	}
	args := []string{"cloud", "run", "--prefix", projectPath, "--yes"}
	if v := stringParam(req, "cloud_grpc"); v != "" {
		args = append(args, "--cloud-grpc", v)
	}
	if v := stringParam(req, "device_name"); v != "" {
		args = append(args, "--device", v)
	}
	if v := stringParam(req, "broker_url"); v != "" {
		args = append(args, "--broker-url", v)
	}
	if v := stringParam(req, "build_type"); v != "" {
		args = append(args, "--build-type", v)
	}
	if v := stringParam(req, "product"); v != "" {
		args = append(args, "--product", v)
	}
	if req.GetBool("debug", false) {
		args = append(args, "--debug")
	}
	if req.GetBool("deploy", false) {
		args = append(args, "--deploy")
	}
	if req.GetBool("detach", true) {
		args = append(args, "--detach")
	}

	cmd := exec.CommandContext(runCtx, bin, args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if runCtx.Err() != nil {
		if text == "" {
			text = runCtx.Err().Error()
		}
		return mcpgo.NewToolResultError(text), nil
	}
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return mcpgo.NewToolResultError(text), nil
	}
	if text == "" {
		text = "cloud run completed"
	}
	return mcpgo.NewToolResultText(text), nil
}

func (s *mcpServer) cloudAuthEntry(cloudGRPC string) (*config.AuthConfig, error) {
	if s.cfg == nil || len(s.cfg.Auth) == 0 {
		return nil, fmt.Errorf("not logged in; run 'wendy auth login' first")
	}
	if cloudGRPC != "" {
		for i := range s.cfg.Auth {
			if s.cfg.Auth[i].CloudGRPC == cloudGRPC {
				if len(s.cfg.Auth[i].Certificates) == 0 {
					return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
				}
				return &s.cfg.Auth[i], nil
			}
		}
		return nil, fmt.Errorf("no auth session for %s; run 'wendy auth login --cloud-grpc %s' first", cloudGRPC, cloudGRPC)
	}
	if len(s.cfg.Auth) > 1 {
		return nil, fmt.Errorf("multiple auth sessions exist; pass cloud_grpc to select one")
	}
	if len(s.cfg.Auth[0].Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	return &s.cfg.Auth[0], nil
}

func (s *mcpServer) connectToCloudAgent(ctx context.Context, cloudGRPC, deviceName, brokerURL string) (*grpcclient.AgentConnection, *cloudpb.Asset, error) {
	auth, err := s.cloudAuthEntry(cloudGRPC)
	if err != nil {
		return nil, nil, err
	}
	asset, err := s.pickCloudAsset(ctx, auth, deviceName)
	if err != nil {
		return nil, nil, err
	}
	brokerConn, err := mcpDialCloudBroker(auth, brokerURL)
	if err != nil {
		return nil, nil, err
	}
	cleanupBroker := true
	defer func() {
		if cleanupBroker {
			_ = brokerConn.Close()
		}
	}()

	tunnelConn, err := mcpOpenBrokerTunnel(ctx, brokerConn, auth, asset.GetId(), 50052)
	if err != nil {
		return nil, nil, fmt.Errorf("opening cloud tunnel to %s: %w", asset.GetName(), err)
	}
	dialOpt, closeTunnel := mcpTunnelDialer(tunnelConn)

	certInfo := auth.Certificates[0]
	x509Cert, err := tls.X509KeyPair([]byte(certInfo.PemCertificate), []byte(certInfo.PemPrivateKey))
	if err != nil {
		closeTunnel()
		return nil, nil, fmt.Errorf("loading agent mTLS cert: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{x509Cert},
		InsecureSkipVerify: true, //nolint:gosec
		MinVersion:         tls.VersionTLS12,
	}
	grpcConn, err := grpc.NewClient(
		"passthrough:///cloud-tunnel",
		dialOpt,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
	)
	if err != nil {
		closeTunnel()
		return nil, nil, fmt.Errorf("creating tunnelled gRPC connection: %w", err)
	}
	agentConn := grpcclient.NewFromConn(grpcConn)
	agentConn.Host = asset.GetName()
	agentConn.IsMTLS = true
	agentConn.RegistryDialer = func(ctx context.Context, port int) (net.Conn, error) {
		return mcpOpenBrokerTunnel(ctx, brokerConn, auth, asset.GetId(), uint32(port))
	}
	agentConn.ExtraClosers = append(agentConn.ExtraClosers, mcpCloseFunc(closeTunnel), brokerConn)
	cleanupBroker = false
	return agentConn, asset, nil
}

func (s *mcpServer) pickCloudAsset(ctx context.Context, auth *config.AuthConfig, deviceName string) (*cloudpb.Asset, error) {
	assets, err := mcpListCloudAssets(ctx, auth, "", true)
	if err != nil {
		return nil, err
	}
	if len(assets) == 0 {
		return nil, fmt.Errorf("no enrolled devices found for this org; enroll a device with cloud_enroll_device")
	}
	if deviceName == "" {
		if len(assets) == 1 {
			return assets[0], nil
		}
		return nil, fmt.Errorf("multiple cloud devices found; pass device_name")
	}
	lower := strings.ToLower(deviceName)
	var matched *cloudpb.Asset
	for _, a := range assets {
		if strings.ToLower(a.GetName()) == lower {
			if matched != nil {
				return nil, fmt.Errorf("multiple devices match %q; use a more specific name", deviceName)
			}
			matched = a
		}
	}
	if matched == nil {
		return nil, fmt.Errorf("no device named %q found; call cloud_discover to list devices", deviceName)
	}
	return matched, nil
}

func mcpCreateAssetEnrollmentToken(ctx context.Context, auth *config.AuthConfig, name string) (*cloudpb.CreateAssetEnrollmentTokenResponse, error) {
	conn, err := mcpDialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := cloudpb.NewCertificateServiceClient(conn).CreateAssetEnrollmentToken(mcpCloudContext(ctx, auth), &cloudpb.CreateAssetEnrollmentTokenRequest{
		OrganizationId: int32(auth.Certificates[0].OrganizationID),
		Name:           name,
		TtlSeconds:     600,
	})
	if err != nil {
		return nil, fmt.Errorf("creating enrollment token: %w", err)
	}
	return resp, nil
}

func mcpListCloudAssets(ctx context.Context, auth *config.AuthConfig, filter string, onlineOnly bool) ([]*cloudpb.Asset, error) {
	conn, err := mcpDialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	req := &cloudpb.ListAssetsRequest{
		OrganizationId:  int32(auth.Certificates[0].OrganizationID),
		IsComputeDevice: boolPtr(true),
		OnlineOnly:      boolPtr(onlineOnly),
	}
	if filter != "" {
		req.Filter = &filter
	}
	stream, err := cloudpb.NewAssetServiceClient(conn).ListAssets(mcpCloudContext(ctx, auth), req)
	if err != nil {
		return nil, fmt.Errorf("listing devices: %w", err)
	}
	var assets []*cloudpb.Asset
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing devices: %w", err)
		}
		if a := msg.GetAsset(); a != nil {
			assets = append(assets, a)
		}
	}
	return assets, nil
}

func mcpCloudContext(ctx context.Context, auth *config.AuthConfig) context.Context {
	if len(auth.Certificates) == 0 {
		return ctx
	}
	certInfo := auth.Certificates[0]
	md := metadata.MD{}
	if auth.APIKey != "" {
		md.Set("authorization", "Bearer "+auth.APIKey)
	}
	if certInfo.UserID != "" {
		md.Set("x-forwarded-client-cert", fmt.Sprintf("URI=urn:wendy:org:%d:user:%s", certInfo.OrganizationID, certInfo.UserID))
	} else {
		md.Set("x-forwarded-client-cert", fmt.Sprintf("URI=urn:wendy:org:%d:user:unknown", certInfo.OrganizationID))
	}
	return metadata.NewOutgoingContext(ctx, md)
}

func mcpDialCloudGRPC(auth *config.AuthConfig) (*grpc.ClientConn, error) {
	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	var transport grpc.DialOption
	if strings.HasSuffix(auth.CloudGRPC, ":443") {
		certInfo := auth.Certificates[0]
		tlsCfg, err := certs.LoadTLSConfig(
			certInfo.PemCertificate,
			certInfo.PemCertificateChain,
			certInfo.PemPrivateKey,
			"",
		)
		if err != nil {
			return nil, fmt.Errorf("loading TLS config: %w", err)
		}
		transport = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	} else {
		transport = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	conn, err := grpc.NewClient(auth.CloudGRPC, transport)
	if err != nil {
		return nil, fmt.Errorf("connecting to cloud: %w", err)
	}
	return conn, nil
}

func mcpDialCloudBroker(auth *config.AuthConfig, brokerURL string) (*grpc.ClientConn, error) {
	if brokerURL == "" {
		host := auth.CloudGRPC
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		brokerURL = net.JoinHostPort(host, mcpDefaultBrokerPort)
	}
	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	certInfo := auth.Certificates[0]
	tlsCfg, err := certs.LoadTLSConfig(
		certInfo.PemCertificate,
		certInfo.PemCertificateChain,
		certInfo.PemPrivateKey,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("loading broker TLS config: %w", err)
	}
	tlsCfg.InsecureSkipVerify = true //nolint:gosec
	conn, err := grpc.NewClient(brokerURL, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return nil, fmt.Errorf("connecting to broker at %s: %w", brokerURL, err)
	}
	return conn, nil
}

func mcpOpenBrokerTunnel(ctx context.Context, brokerConn *grpc.ClientConn, auth *config.AuthConfig, assetID int32, remotePort uint32) (net.Conn, error) {
	stream, err := cloudpb.NewTunnelBrokerServiceClient(brokerConn).ClientTunnel(mcpCloudContext(ctx, auth))
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
	go func() {
		defer remote.Close()
		for {
			msg, err := stream.Recv()
			if err != nil {
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
	return local, nil
}

func mcpTunnelDialer(tunnelConn net.Conn) (grpc.DialOption, func()) {
	var once sync.Once
	return grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
		return tunnelConn, nil
	}), func() { once.Do(func() { tunnelConn.Close() }) }
}

func mcpServeTunnelConn(ctx context.Context, tcpConn net.Conn, brokerConn *grpc.ClientConn, auth *config.AuthConfig, assetID int32, remotePort uint32) {
	defer tcpConn.Close()
	tunnelConn, err := mcpOpenBrokerTunnel(ctx, brokerConn, auth, assetID, remotePort)
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
	<-done
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
