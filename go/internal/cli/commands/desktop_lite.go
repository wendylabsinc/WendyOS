package commands

import (
	"context"
	"errors"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

var desktopConnectLite = func(ctx context.Context, provider *providers.MicroWendyProvider, device models.ExternalDevice) (*liteclient.WendyLiteClient, error) {
	return provider.ConnectSensorLinkContext(ctx, device)
}

// DesktopConnectTarget keeps Lite selections on their authenticated WendyCom
// transport. DesktopConnect remains agent-only for VPN and HTTP tunnels.
func DesktopConnectTarget(ctx context.Context, selector string) (*grpcclient.AgentConnection, *liteclient.WendyLiteClient, error) {
	if strings.TrimSpace(selector) == "" {
		return nil, nil, errors.New("choose a device")
	}
	if conn, ok, err := dialAgentSocketIfSet(ctx); ok {
		return conn, nil, err
	}
	ctx = context.WithValue(ctx, detachedJSONRunKey{}, true)
	picked, matched, err := connectNamedDeviceSelector(robotRuntimePromptContext(ctx, true), selector, true)
	if err != nil {
		return nil, nil, err
	}
	if !matched && strings.HasPrefix(selector, "wendy-lite:") {
		provider := &providers.MicroWendyProvider{}
		devices, err := provider.DiscoverDevices(ctx)
		if err != nil {
			return nil, nil, err
		}
		for _, device := range devices {
			if device.ID == selector {
				picked = &SelectedDevice{External: &device, Provider: provider}
				break
			}
		}
		if picked == nil {
			return nil, nil, errors.New("Wendy Lite device was not found; refresh nearby devices and try again")
		}
		matched = true
	}
	if !matched {
		conn, err := DesktopConnect(ctx, selector)
		return conn, nil, err
	}
	if picked.Agent != nil {
		return picked.Agent, nil, nil
	}
	provider, ok := picked.Provider.(*providers.MicroWendyProvider)
	if !ok || picked.External == nil {
		picked.Close()
		return nil, nil, errors.New("selected device has no supported desktop transport")
	}
	client, err := desktopConnectLite(ctx, provider, *picked.External)
	return nil, client, err
}

// DesktopLiteCamera reuses the CLI's bounded SensorLink assembler, frame-drop
// handling and subscription cleanup with the desktop's existing connection.
type DesktopLiteCamera interface {
	Recv() (*sensorlink.SensorFrame, error)
	Close()
}

func DesktopSubscribeLiteCamera(ctx context.Context, client liteCameraClient, channel *sensorlinkpb.SensorDescriptor) (DesktopLiteCamera, error) {
	switch channel.GetVideo().GetCodec() {
	case sensorlinkpb.VideoFormat_H264, sensorlinkpb.VideoFormat_MJPEG:
		return subscribeLiteCamera(ctx, client, channel)
	default:
		return nil, errors.New("this SensorLink camera uses an unsupported video codec")
	}
}
