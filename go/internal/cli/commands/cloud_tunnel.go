package commands

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

const (
	defaultBrokerPort = "50052"
	maxCloudAssets    = 10_000
)

type closeFunc func()

func (f closeFunc) Close() error {
	f()
	return nil
}

// certXFCC builds the X-Forwarded-Client-Cert header value from stored cert
// metadata. The server auth interceptor extracts the Wendy URI to identify
// the caller when there is no TLS peer certificate (plaintext connections).
func certXFCC(cert config.CertificateInfo) string {
	if cert.UserID != "" {
		return fmt.Sprintf("URI=urn:wendy:org:%d:user:%s", cert.OrganizationID, cert.UserID)
	}
	return fmt.Sprintf("URI=urn:wendy:org:%d:user:unknown", cert.OrganizationID)
}

// cloudContext returns ctx enriched with cloud auth metadata.
// The server checks TLS peer cert first and falls back to x-forwarded-client-cert,
// so we always include the XFCC header; it is harmless when TLS peer cert is present.
func cloudContext(ctx context.Context, auth *config.AuthConfig) context.Context {
	if len(auth.Certificates) == 0 {
		return ctx
	}
	cert := auth.Certificates[0]
	md := metadata.MD{}
	if auth.APIKey != "" {
		md.Set("authorization", "Bearer "+auth.APIKey)
	}
	certHeader := certXFCC(cert)
	md.Set("x-wendy-client-cert", certHeader)
	md.Set("x-forwarded-client-cert", certHeader)
	return metadata.NewOutgoingContext(ctx, md)
}

func connectToCloudAgent(ctx context.Context, cloudGRPC, deviceName, brokerURL string) (*grpcclient.AgentConnection, error) {
	auth, err := pickAuthEntry(cloudGRPC)
	if err != nil {
		return nil, err
	}

	asset, err := pickCloudDevice(ctx, auth, deviceName)
	if err != nil {
		return nil, err
	}
	cliLogln("Connecting to %s via cloud tunnel...", asset.GetName())

	brokerConn, err := dialCloudBroker(auth, brokerURL)
	if err != nil {
		return nil, err
	}

	cleanupBroker := true
	defer func() {
		if cleanupBroker {
			_ = brokerConn.Close()
		}
	}()

	// Provisioned agents only serve mTLS on agentPort+1 (50052); the plaintext
	// port (50051) is shut down after provisioning.
	tunnelConn, err := openBrokerTunnel(ctx, brokerConn, auth, asset.GetId(), defaultAgentPort+1)
	if err != nil {
		return nil, fmt.Errorf("opening cloud tunnel to %s: %w", asset.GetName(), err)
	}

	dialOpt, closeTunnel := tunnelDialer(tunnelConn)

	cert := auth.Certificates[0]
	x509Cert, err := tls.X509KeyPair([]byte(cert.PemCertificate), []byte(cert.PemPrivateKey))
	if err != nil {
		closeTunnel()
		return nil, fmt.Errorf("loading agent mTLS cert: %w", err)
	}
	tlsCfg := &tls.Config{
		Certificates:       []tls.Certificate{x509Cert},
		InsecureSkipVerify: true, //nolint:gosec — agent uses self-signed certs; chain verified server-side
		MinVersion:         tls.VersionTLS12,
	}

	grpcConn, err := grpc.NewClient(
		"passthrough:///cloud-tunnel",
		dialOpt,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
	)
	if err != nil {
		closeTunnel()
		return nil, fmt.Errorf("creating tunnelled gRPC connection: %w", err)
	}

	agentConn := grpcclient.NewFromConn(grpcConn)
	agentConn.Host = asset.GetName()
	agentConn.IsMTLS = true
	agentConn.RegistryDialer = func(ctx context.Context, port int) (net.Conn, error) {
		return openBrokerTunnel(ctx, brokerConn, auth, asset.GetId(), uint32(port))
	}
	agentConn.ExtraClosers = append(agentConn.ExtraClosers, closeFunc(closeTunnel), brokerConn)
	cleanupBroker = false
	return agentConn, nil
}

// dialCloudBroker opens an mTLS gRPC connection to the tunnel broker.
// brokerURL is host:port; if empty it is derived from auth.CloudGRPC.
func dialCloudBroker(auth *config.AuthConfig, brokerURL string) (*grpc.ClientConn, error) {
	brokerURL = clouddefaults.BrokerURL(auth.CloudGRPC, brokerURL, defaultBrokerPort)

	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	cert := auth.Certificates[0]
	tlsCfg, err := certs.LoadTLSConfig(
		cert.PemCertificate,
		cert.PemCertificateChain,
		cert.PemPrivateKey,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("loading broker TLS config: %w", err)
	}
	// Broker cert CN is localhost and won't match the cloud host — skip hostname
	// verification but still validate the chain against the Wendy CA.
	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM([]byte(cert.PemCertificateChain)) {
		return nil, fmt.Errorf("no valid CA certificates in PemCertificateChain")
	}
	tlsCfg.InsecureSkipVerify = true //nolint:gosec
	tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return fmt.Errorf("broker presented no TLS certificate")
		}
		intermediates := x509.NewCertPool()
		for _, c := range cs.PeerCertificates[1:] {
			intermediates.AddCert(c)
		}
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
			Roots:         caPool,
			Intermediates: intermediates,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		return err
	}
	conn, err := grpc.NewClient(brokerURL, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return nil, fmt.Errorf("connecting to broker at %s: %w", brokerURL, err)
	}
	return conn, nil
}

// openBrokerTunnel asks the broker to connect to remotePort on the given asset
// and returns a net.Conn whose reads/writes are relayed through the tunnel stream.
// The caller is responsible for closing the returned conn.
func openBrokerTunnel(ctx context.Context, brokerConn *grpc.ClientConn, auth *config.AuthConfig, assetID int32, remotePort uint32) (net.Conn, error) {
	client := cloudpb.NewTunnelBrokerServiceClient(brokerConn)

	stream, err := client.ClientTunnel(cloudContext(ctx, auth))
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

	// Bridge the gRPC stream into a net.Conn via a synchronous pipe.
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

// fetchCloudAssets retrieves all online compute-device assets for the org.
func fetchCloudAssets(ctx context.Context, auth *config.AuthConfig) ([]*cloudpb.Asset, error) {
	cert := auth.Certificates[0]
	cloudConn, err := dialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer cloudConn.Close()

	assetClient := cloudpb.NewAssetServiceClient(cloudConn)
	req := &cloudpb.ListAssetsRequest{
		OrganizationId:  int32(cert.OrganizationID),
		IsComputeDevice: boolPtr(true),
		OnlineOnly:      boolPtr(true),
	}
	stream, err := assetClient.ListAssets(cloudContext(ctx, auth), req)
	if err != nil {
		return nil, fmt.Errorf("listing devices: %w", err)
	}
	var assets []*cloudpb.Asset
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("listing devices: %w", err)
		}
		if len(assets) >= maxCloudAssets {
			return nil, fmt.Errorf("cloud returned more than %d devices", maxCloudAssets)
		}
		assets = append(assets, resp.GetAsset())
	}
	return assets, nil
}

// resolveCloudAsset performs name matching and single-device auto-select.
// Returns (nil, nil) when multiple devices are present and a picker is needed.
func resolveCloudAsset(assets []*cloudpb.Asset, deviceName string) (*cloudpb.Asset, error) {
	if len(assets) == 0 {
		return nil, fmt.Errorf("no enrolled devices found for this org; enroll a device with 'wendy device enroll' first")
	}
	if deviceName != "" {
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
		if matched != nil {
			return matched, nil
		}
		return nil, fmt.Errorf("no device named %q found; omit --device to choose from a list", deviceName)
	}
	if len(assets) == 1 {
		return assets[0], nil
	}
	return nil, nil // multiple devices — show picker
}

// pickCloudDevice shows a spinner while fetching online compute-device assets,
// auto-selects when only one matches, and shows an interactive TUI picker when
// multiple devices are available. If deviceName is non-empty and matches exactly
// one asset name (case-insensitive), the picker is skipped.
func pickCloudDevice(ctx context.Context, auth *config.AuthConfig, deviceName string) (*cloudpb.Asset, error) {
	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}

	// Fetch device list, showing a spinner in interactive terminals.
	var assets []*cloudpb.Asset
	if isInteractiveTerminal() {
		prog := tea.NewProgram(tui.NewSpinner("Fetching devices from cloud..."))
		var fetchErr error
		go func() {
			assets, fetchErr = fetchCloudAssets(ctx, auth)
			prog.Send(tui.SpinnerDoneMsg{})
		}()
		finalModel, err := prog.Run()
		if err != nil {
			return nil, fmt.Errorf("spinner: %w", err)
		}
		if sm, ok := finalModel.(tui.SpinnerModel); ok && !sm.Done() {
			return nil, ErrUserCancelled
		}
		if fetchErr != nil {
			return nil, fetchErr
		}
	} else {
		var err error
		assets, err = fetchCloudAssets(ctx, auth)
		if err != nil {
			return nil, err
		}
	}

	asset, err := resolveCloudAsset(assets, deviceName)
	if err != nil || asset != nil {
		return asset, err
	}

	if !isInteractiveTerminal() {
		return nil, fmt.Errorf("multiple cloud devices found; rerun with --device in a non-interactive environment")
	}
	// Multiple devices — show interactive picker.
	picker := tui.NewPickerWithTitle("Select a cloud device")
	items := make([]tui.PickerItem, 0, len(assets))
	for _, a := range assets {
		aCopy := a
		items = append(items, tui.PickerItem{
			Name:        a.GetName(),
			Description: fmt.Sprintf("asset %d", a.GetId()),
			Type:        "Cloud",
			Value:       aCopy,
		})
	}
	p := tea.NewProgram(picker)
	go func() {
		p.Send(tui.PickerAddMsg{Items: items})
		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("device picker: %w", err)
	}
	pm := finalModel.(tui.PickerModel)
	if pm.Cancelled() {
		return nil, ErrUserCancelled
	}
	sel := pm.Selected()
	if sel == nil {
		return nil, fmt.Errorf("no device selected")
	}
	asset, ok := sel.Value.(*cloudpb.Asset)
	if !ok {
		return nil, fmt.Errorf("invalid picker selection")
	}
	return asset, nil
}

func boolPtr(b bool) *bool { return &b }

// dialCloudGRPC opens a gRPC connection to auth.CloudGRPC using the same
// transport selection logic as dialCloudBroker: :443 gets mTLS, anything else
// gets plaintext h2c.
func dialCloudGRPC(auth *config.AuthConfig) (*grpc.ClientConn, error) {
	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	cert := auth.Certificates[0]
	var transport grpc.DialOption
	if strings.HasSuffix(auth.CloudGRPC, ":443") {
		tlsCfg, err := certs.LoadTLSConfig(
			cert.PemCertificate,
			cert.PemCertificateChain,
			cert.PemPrivateKey,
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

// tunnelDialer returns a grpc.DialOption that routes all dials through the
// given net.Conn (the broker tunnel). The returned closer shuts the conn.
func tunnelDialer(tunnelConn net.Conn) (grpc.DialOption, func()) {
	var once sync.Once
	return grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
		return tunnelConn, nil
	}), func() { once.Do(func() { tunnelConn.Close() }) }
}
