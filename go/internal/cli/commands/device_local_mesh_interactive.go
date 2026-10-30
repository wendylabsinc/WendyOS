package commands

import (
	"context"
	"errors"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

var runLocalMeshChecklist = tui.RunChecklist

func interactiveLocalMeshRequest(ctx context.Context, client pb.WendyLocalMeshAdminServiceClient) (*pb.ConfigureLocalMeshRequest, error) {
	status, err := client.GetLocalMeshStatus(ctx, &pb.GetLocalMeshStatusRequest{})
	if err != nil {
		return nil, fmt.Errorf("reading Wendy Net settings: %w", err)
	}
	current := status.GetConfigured()
	if current == nil {
		return nil, fmt.Errorf("agent returned no saved Wendy Net settings")
	}
	req := &pb.ConfigureLocalMeshRequest{}
	fields := []struct {
		key, label, description string
		current                 bool
		dest                    **bool
	}{
		{"participate", "Participate", "Join and relay Wendy Net; disabling keeps carrier choices", current.GetParticipate(), &req.Participate},
		{"roam", "Use Wendy Net Internet", "Use a peer's Internet when this device has no independent uplink", current.GetRoam(), &req.Roam},
		{"share-uplink", "Share Internet", "Offer this device's independent uplink to peers", current.GetShareUplink(), &req.ShareUplink},
		{"nan", "Wi-Fi Aware", "Discover and connect over NAN", current.GetNan(), &req.Nan},
		{"ble", "Bluetooth", "Connect over BLE L2CAP", current.GetBle(), &req.Ble},
		{"ethernet", "Ethernet", "Discover local peers over Ethernet", current.GetEthernet(), &req.Ethernet},
		{"infrastructure-wifi", "Wi-Fi network", "Discover local peers on the connected Wi-Fi network", current.GetInfrastructureWifi(), &req.InfrastructureWifi},
	}
	items := make([]tui.ChecklistItem, 0, len(fields))
	for _, field := range fields {
		items = append(items, tui.ChecklistItem{Label: field.label, Description: field.description, Value: field.key, Selected: field.current})
	}
	selected, err := runLocalMeshChecklist("Wendy Net settings: Enter to save, Ctrl+C to cancel", items, tea.WithContext(ctx))
	if errors.Is(err, tui.ErrCancelled) {
		return nil, ErrUserCancelled
	}
	if err != nil {
		return nil, err
	}
	enabled := make(map[string]bool, len(selected))
	for _, item := range selected {
		enabled[item.Value] = true
	}
	changed := false
	for _, field := range fields {
		value := enabled[field.key]
		if value != field.current {
			*field.dest = &value
			changed = true
		}
	}
	if !changed {
		return nil, nil
	}
	return req, nil
}
