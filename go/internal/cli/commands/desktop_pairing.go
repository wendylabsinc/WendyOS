package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Desktop pairing reuses the CLI handlers, including tenant checks and the
// device-side discovery fallback. No source address or identity comes from UI input.
type DesktopPairing struct {
	ID, Name, Address, Detail  string
	CameraID                   uint32
	Paired, Connected, CanPair bool
}
type DesktopPairings struct {
	Items   []DesktopPairing
	Warning string
}

func desktopSensorHandler(ctx context.Context, conn *grpcclient.AgentConnection) *sensorPairHandler {
	return &sensorPairHandler{ctx: ctx, client: conn.SensorPairingService,
		discoverLocal: discoverLocalSensorSources, orgIDs: cliOrgIDs, tenantIDs: cliTenantIDs}
}

func DesktopSensorPairings(ctx context.Context, conn *grpcclient.AgentConnection) (DesktopPairings, error) {
	msg := desktopSensorHandler(ctx, conn).scan()().(sensorPairScanMsg)
	if msg.pairingsErr != nil {
		return DesktopPairings{}, msg.pairingsErr
	}
	out := DesktopPairings{}
	if msg.localNetwork {
		out.Warning = "This Agent is too old to scan its network. These discoveries are from this Mac; update the Agent for remote discovery."
	}
	if msg.discoveryErr != nil {
		out.Warning = "Could not discover sources: " + msg.discoveryErr.Error()
	}
	for _, row := range sensorPairRows(msg.devices, msg.pairings) {
		item := DesktopPairing{ID: sensorPairKey(row.assetID, row.principal), Name: row.name, Paired: row.pairing != nil,
			CanPair: row.source != nil && (row.principal == "" || msg.principalPairing)}
		if row.source != nil {
			item.Address = row.source.IPAddress
			item.Detail = "SensorLink"
			if row.source.WendyLite {
				item.Detail = "Wendy Lite"
			}
		}
		if row.pairing != nil {
			item.Connected = row.pairing.Connected
		}
		if row.principal != "" && !msg.principalPairing {
			item.Detail = "Update the Agent to pair this device"
		}
		out.Items = append(out.Items, item)
	}
	for _, source := range msg.devices {
		if source.Sensorlink && source.AssetID <= 0 && sensorSourcePrincipal(source) == "" {
			out.Items = append(out.Items, DesktopPairing{ID: "unenrolled:" + source.IPAddress + ":" + source.DisplayName, Name: source.DisplayName,
				Address: source.IPAddress, Detail: "Enroll this device in your organization before pairing"})
		}
	}
	return out, nil
}

func DesktopPairSensor(ctx context.Context, conn *grpcclient.AgentConnection, id string, remove bool) error {
	h := desktopSensorHandler(ctx, conn)
	msg := h.scan()().(sensorPairScanMsg)
	if msg.pairingsErr != nil {
		return msg.pairingsErr
	}
	for _, row := range sensorPairRows(msg.devices, msg.pairings) {
		if sensorPairKey(row.assetID, row.principal) != id {
			continue
		}
		if remove {
			if row.pairing == nil {
				return errors.New("this device is not paired")
			}
			return h.forget(row)().(sensorPairOpMsg).err
		}
		if row.principal != "" && !msg.principalPairing {
			return errors.New("update the Agent to pair this device identity")
		}
		if row.source == nil {
			return errors.New("this device is no longer discoverable; scan again")
		}
		return h.pair(*row.source)().(sensorPairOpMsg).err
	}
	if msg.discoveryErr != nil {
		return msg.discoveryErr
	}
	return errors.New("this device is no longer available; scan again")
}

func DesktopIPCameraPairings(ctx context.Context, conn *grpcclient.AgentConnection) (DesktopPairings, error) {
	h := cameraPairHandler{ctx: ctx, client: conn.VideoService, preflight: func(ctx context.Context) error { return cameraServicePreflight(ctx, conn, cameraNetworkManagement) }}
	msg := h.scan()().(cameraPairScanMsg)
	if msg.err != nil {
		return DesktopPairings{}, msg.err
	}
	out := DesktopPairings{}
	for _, d := range msg.devices {
		if d.GetTransport() != pb.VideoTransport_VIDEO_TRANSPORT_IP {
			continue
		}
		name := d.GetName()
		if name == "" {
			name = fmt.Sprintf("Camera %d", d.GetId())
		}
		out.Items = append(out.Items, DesktopPairing{ID: fmt.Sprint(d.GetId()), CameraID: d.GetId(), Name: name,
			Address: d.GetAddress(), Detail: "IP camera", Paired: d.GetHasCredentials(), Connected: d.GetOnline(), CanPair: true})
	}
	return out, nil
}

func DesktopPairIPCamera(ctx context.Context, conn *grpcclient.AgentConnection, id uint32, username, password string, remove bool) error {
	if err := cameraServicePreflight(ctx, conn, cameraNetworkManagement); err != nil {
		return err
	}
	h := cameraPairHandler{ctx: ctx, client: conn.VideoService}
	if remove {
		return h.forget(id)().(cameraPairOpMsg).err
	}
	if strings.TrimSpace(username) == "" {
		return errors.New("enter a camera username")
	}
	return h.pair(id, strings.TrimSpace(username), password)().(cameraPairOpMsg).err
}
