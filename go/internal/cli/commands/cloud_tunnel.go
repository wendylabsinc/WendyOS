package commands

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

const defaultBrokerPort = "50053"

// dialCloudBroker opens an mTLS gRPC connection to the tunnel broker.
// brokerURL is host:port; if empty it is derived from auth.CloudGRPC.
func dialCloudBroker(auth *config.AuthConfig, brokerURL string) (*grpc.ClientConn, error) {
	if brokerURL == "" {
		host := auth.CloudGRPC
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		brokerURL = net.JoinHostPort(host, defaultBrokerPort)
	}

	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	cert := auth.Certificates[0]
	tlsCfg, err := certs.LoadTLSConfig(
		cert.PemCertificate,
		cert.PemCertificateChain,
		cert.PemPrivateKey,
		cert.PemCertificateChain,
	)
	if err != nil {
		return nil, fmt.Errorf("loading broker TLS config: %w", err)
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

	callCtx := ctx
	if auth.APIKey != "" {
		callCtx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+auth.APIKey))
	}
	stream, err := client.ClientTunnel(callCtx)
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

// pickCloudDevice lists compute-device assets in the org and shows a TUI
// picker. If deviceName is non-empty and matches exactly one asset name
// (case-insensitive), the picker is skipped.
func pickCloudDevice(ctx context.Context, auth *config.AuthConfig, deviceName string) (*cloudpb.Asset, error) {
	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	cert := auth.Certificates[0]
	tlsCfg, err := certs.LoadTLSConfig(
		cert.PemCertificate,
		cert.PemCertificateChain,
		cert.PemPrivateKey,
		cert.PemCertificateChain,
	)
	if err != nil {
		return nil, fmt.Errorf("loading TLS config: %w", err)
	}

	cloudConn, err := grpc.NewClient(auth.CloudGRPC, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return nil, fmt.Errorf("connecting to cloud: %w", err)
	}
	defer cloudConn.Close()

	assetClient := cloudpb.NewAssetServiceClient(cloudConn)
	callCtx := ctx
	if auth.APIKey != "" {
		callCtx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+auth.APIKey))
	}
	resp, err := assetClient.ListAssets(callCtx, &cloudpb.ListAssetsRequest{
		OrganizationId:  int32(cert.OrganizationID),
		IsComputeDevice: boolPtr(true),
		PageSize:        100,
	})
	if err != nil {
		return nil, fmt.Errorf("listing devices: %w", err)
	}

	assets := resp.GetAssets()
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
	p.Send(tui.PickerAddMsg{Items: items})
	p.Send(tui.PickerDoneMsg{})

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

// tunnelDialer returns a grpc.DialOption that routes all dials through the
// given net.Conn (the broker tunnel). The returned closer shuts the conn.
func tunnelDialer(tunnelConn net.Conn) (grpc.DialOption, func()) {
	var once sync.Once
	return grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
		return tunnelConn, nil
	}), func() { once.Do(func() { tunnelConn.Close() }) }
}
